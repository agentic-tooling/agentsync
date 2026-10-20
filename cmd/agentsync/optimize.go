package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/agentic-tooling/agentsync/internal/discover"
	"github.com/agentic-tooling/agentsync/internal/openrouter"
	"github.com/agentic-tooling/agentsync/internal/optimize"
	"github.com/agentic-tooling/agentsync/internal/override"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// openrouterURL is where optimize sends its requests; tests point it at an
// httptest server. optimize is the only command that uses the network.
var openrouterURL = openrouter.DefaultBaseURL

const (
	envOpenRouterKey = "OPENROUTER_API_KEY"
	defaultAdvisor   = "~typesafe/jev-latest"
)

// Claude Code reads these at run time and lets them beat the frontmatter.
var claudeOverrides = []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL"}

// runOptimize asks an advisor model about each Claude agent under the root
// and reports its advice. Only --apply writes, and only the parts it suggests.
func runOptimize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("optimize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	rootF := registerRoot(fs)
	repoPath := fs.String("repo", "", "canonical agent repo")
	asJSON := fs.Bool("json", false, "emit JSON")
	advisor := fs.String("advisor", defaultAdvisor, "OpenRouter decision model to ask")
	maxCost := fs.Float64("max-cost", 0.50, "most this run may spend, in USD")
	minConf := fs.Float64("min-confidence", 0.6, "least probability a pick needs to be suggested")
	apply := fs.Bool("apply", false, "write each suggested change into its agent's file")
	var names stringList
	fs.Var(&names, "agent", "ask only about this agent (repeatable)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFailure
	}
	if err := optimizeFlagsErr(fs, *advisor, *maxCost, *minConf); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	key := strings.TrimSpace(os.Getenv(envOpenRouterKey))
	if key == "" {
		fmt.Fprintf(stderr, "agentsync: optimize needs an OpenRouter API key in $%s\n", envOpenRouterKey)
		return exitFailure
	}

	repo, repoFrom, err := openRepo(*repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	root, err := rootF.resolve(repo.Home)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	res, err := repo.Scan(root.Path)
	if err == nil {
		err = scanProblemsErr(res.Problems)
	}
	var agents []optimize.Agent
	if err == nil {
		agents, err = optimizeAgents(res, names)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	client := openrouter.New(key)
	client.BaseURL = openrouterURL
	ctx := context.Background()
	cfg := optimize.Config{Advisor: *advisor, MaxCost: *maxCost, MinConfidence: *minConf}
	models, err := client.Models(ctx)
	if err == nil {
		cfg.Price, err = optimize.InputPrice(models, *advisor)
	}
	if err == nil {
		if cfg.Families = optimize.Available(models); len(cfg.Families) == 0 {
			err = errors.New("the OpenRouter models list has no anthropic/claude-<family>-<version> model, so there is nothing to suggest")
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: OpenRouter models list: %v\n", err)
		return exitFailure
	}

	notes := reportStream(*asJSON, stdout, stderr)
	emitRepoChoice(notes, repo, repoFrom)
	emitRootChoice(notes, repo.Home, root)
	var overrides []string
	for _, v := range claudeOverrides {
		if os.Getenv(v) != "" {
			overrides = append(overrides, v)
			fmt.Fprintf(notes, "Note: $%s is set; Claude Code uses it over the frontmatter at run time.\n", v)
		}
	}
	ov := entryOverrides(res, stderr)
	for _, e := range res.Entries {
		if v, ok := ov[e.Path]; ok && (len(names) == 0 || slices.Contains(names, e.Name)) {
			fmt.Fprintf(notes, "Note: %s/%s has a project override (%s) in %s; Claude Code uses it over the frontmatter there.\n",
				e.Project, e.Name, v, displayPath(repo.Home, settingsPath(filepath.Dir(e.Path))))
		}
	}

	rep := optimize.Run(ctx, client, agents, cfg)
	rep.Overrides = overrides
	code := exitOK
	if rep.Count(optimize.StatusUnresolved)+rep.Count(optimize.StatusSkipped) > 0 {
		code = exitAtRisk
	}
	var applied strings.Builder
	if *apply {
		code = max(code, applyChanges(&applied, stderr, repo, res, ov, agents, rep.Rows))
	}
	if *asJSON {
		fmt.Fprint(notes, applied.String())
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		emitOptimizeText(stdout, rep, cfg, *apply, applied.String())
	}
	return code
}

// refusal is a change left unwritten on purpose, as opposed to a failed write.
type refusal struct{ error }

// applyChanges writes each change row into the file its agent resolves to and
// records the outcome on the row; rows[i] is the advice for agents[i]. It
// returns exitAtRisk for a refused row and exitFailure for a failed write.
func applyChanges(w, stderr io.Writer, repo *scan.Repo, res scan.Result, ov map[string]override.Value, agents []optimize.Agent, rows []optimize.Row) int {
	code := exitOK
	repoReal := resolved(repo.Root)
	links, stateErr := state.Load(repo)
	inRepo, warned := false, false
	for i := range rows {
		r := &rows[i]
		if r.Status != optimize.StatusChange {
			continue
		}
		real, err := writeChange(agents[i], r.SuggestedModel, r.SuggestedEffort, repoReal, links, stateErr)
		if err != nil {
			r.ApplyError = err.Error()
			fmt.Fprintf(w, "%s: not written: %v\n", r.Agent, err)
			if errors.As(err, new(refusal)) {
				code = max(code, exitAtRisk)
			} else {
				code = exitFailure
			}
			continue
		}
		r.Applied = true
		where := ""
		if within(real, repoReal) {
			inRepo = true
			if stateErr != nil && !warned {
				warned = true
				fmt.Fprintf(stderr, "agentsync: warning: link counts not shown: %v\n", stateErr)
			}
			n := 0
			if links != nil {
				for _, l := range links.Records() {
					if resolved(l.Target) == real {
						n++
					}
				}
			}
			where = " (canonical file)"
			if n > 0 {
				where = fmt.Sprintf(" (canonical; linked into %d project%s)", n, plural(n, "", "s"))
			}
		}
		fmt.Fprintf(w, "wrote %s%s\n", displayPath(repo.Home, real), where)
		for _, e := range res.Entries {
			v, ok := ov[e.Path]
			masks := (v.Model != "" && r.SuggestedModel != "") || (v.Effort != "" && r.SuggestedEffort != "")
			if ok && masks && resolved(e.Path) == real {
				fmt.Fprintf(w, "Note: %s/%s's project override (%s) still wins over the written frontmatter there.\n", e.Project, e.Name, v)
			}
		}
	}
	if inRepo {
		fmt.Fprintf(w, "Changed files in %s are not committed; review and commit them there.\n", displayPath(repo.Home, repo.Root))
	}
	return code
}

// writeChange edits the file a.Path resolves to, never the link itself, and
// returns that file's path. stateErr is why the state file could not be read,
// which leaves no way to tell a regular file from a dereferenced link.
func writeChange(a optimize.Agent, model, effort, repoReal string, links *state.File, stateErr error) (string, error) {
	li, err := os.Lstat(a.Path)
	var real string
	if err == nil {
		real, err = filepath.EvalSymlinks(a.Path)
	}
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(real)
	}
	if err != nil {
		return "", err
	}
	link := li.Mode()&os.ModeSymlink != 0
	recorded := false
	if stateErr == nil {
		_, recorded = links.LookupLink(a.Path)
	}
	switch {
	case link && !within(real, repoReal):
		return "", refusal{errors.New("it links outside the canonical repo, and agentsync leaves foreign links alone")}
	case !link && stateErr != nil:
		return "", fmt.Errorf("the state file could not be read to check that this was never an agentsync link: %w", stateErr)
	case !link && recorded:
		// Editing a dereferenced copy would turn reconcile's clean relink into a conflict.
		return "", refusal{errors.New("agentsync recorded a link here that is now a copy; run agentsync link --apply first")}
	case fi.Mode().Perm()&0o200 == 0:
		return "", refusal{errors.New("the file is read-only")}
	case string(data) != a.Content:
		return "", refusal{errors.New("the file changed since it was read; run optimize again")}
	}
	out, err := optimize.Edit(a.Content, model, effort)
	if err != nil {
		return "", refusal{err}
	}
	return real, writeAtomic(real, []byte(out), fi.Mode().Perm())
}

// within reports whether p lies below dir.
func within(p, dir string) bool {
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

// writeAtomic replaces path through a temp file beside it, so a reader sees
// the old file or the new one and never half of either. The temp name does not
// end in .md, so a scan running meanwhile never takes it for an agent.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentsync-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

// resolved is p with every symlink followed, or p as given when that fails.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func optimizeFlagsErr(fs *flag.FlagSet, advisor string, maxCost, minConf float64) error {
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("optimize takes no arguments, got %q", fs.Arg(0))
	case strings.TrimSpace(advisor) == "":
		return errors.New("--advisor is empty")
	case !(maxCost >= 0) || math.IsInf(maxCost, 1):
		return fmt.Errorf("--max-cost must be a finite amount of USD at or above 0, got %v", maxCost)
	case !(minConf >= 0 && minConf <= 1):
		return fmt.Errorf("--min-confidence must be between 0 and 1, got %v", minConf)
	}
	return nil
}

// scanProblemsErr refuses a scan that could not read everything: an agent it
// missed is one the report would silently leave out.
func scanProblemsErr(problems []scan.ScanProblem) error {
	if len(problems) == 0 {
		return nil
	}
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = "  " + p.String()
	}
	return fmt.Errorf("could not read every agent under the root:\n%s", strings.Join(lines, "\n"))
}

// optimizeAgents reads the Claude agents scan found, once per real file, so
// an agent linked into many projects costs one request.
func optimizeAgents(res scan.Result, names []string) ([]optimize.Agent, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = false
	}
	seen := map[string]bool{}
	var out []optimize.Agent
	for _, e := range res.Entries {
		if _, ok := want[e.Name]; len(names) > 0 && !ok {
			continue
		}
		data, err := os.ReadFile(e.Path)
		if err != nil {
			return nil, fmt.Errorf("could not read agent %s (%w); agentsync doctor reports broken links", e.Path, err)
		}
		if !discover.IsAgent(string(data)) {
			continue
		}
		want[e.Name] = true
		real := resolved(e.Path)
		if seen[real] {
			continue
		}
		seen[real] = true
		label := e.Name
		if len(res.Dirs) > 1 {
			label = e.Project + "/" + e.Name
		}
		out = append(out, optimize.Agent{Label: label, Path: e.Path, Content: string(data)})
	}
	for _, n := range names {
		if !want[n] {
			return nil, fmt.Errorf("--agent %s: no Claude agent by that name under the root", n)
		}
	}
	return out, nil
}

func emitOptimizeText(w io.Writer, rep optimize.Report, cfg optimize.Config, apply bool, applied string) {
	skipped := rep.Count(optimize.StatusSkipped)
	asked := len(rep.Rows) - skipped
	fmt.Fprintf(w, "Asked %s about %d agent%s", rep.Advisor, asked, plural(asked, "", "s"))
	if skipped > 0 {
		fmt.Fprintf(w, ", %d skipped", skipped)
	}
	fmt.Fprintf(w, " (cap $%.2f, min confidence %.2f).\n\n", cfg.MaxCost, cfg.MinConfidence)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Typed as io.Writer so errcheck's report-writer exemption applies.
	var table io.Writer = tw
	fmt.Fprintln(table, "AGENT\tNOW\tSUGGESTED\tCONFIDENCE")
	for _, r := range rep.Rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", r.Agent, r.Now(), r.Suggested(), r.Confidence())
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	for _, r := range rep.Rows {
		if r.Reason != "" {
			fmt.Fprintf(w, "%s: %s: %s\n", r.Agent, r.Status, r.Reason)
		}
	}
	if skipped > 0 {
		fmt.Fprintf(w, "%d agent%s skipped (reasons above).\n", skipped, plural(skipped, " was", "s were"))
	}
	fmt.Fprint(w, applied)
	fmt.Fprintf(w, "Spent $%.6f (reported by OpenRouter). ", rep.Spent)
	n := rep.Count(optimize.StatusChange)
	switch {
	case apply:
		written := 0
		for _, r := range rep.Rows {
			if r.Applied {
				written++
			}
		}
		fmt.Fprintf(w, "Wrote %d change%s.\n", written, plural(written, "", "s"))
	case n > 0:
		fmt.Fprintf(w, "Dry run: nothing written. Run with --apply to write %d change%s.\n", n, plural(n, "", "s"))
	default:
		fmt.Fprintln(w, "Dry run: nothing written.")
	}
}
