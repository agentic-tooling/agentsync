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
	"strings"
	"text/tabwriter"

	"github.com/agentic-tooling/agentsync/internal/discover"
	"github.com/agentic-tooling/agentsync/internal/openrouter"
	"github.com/agentic-tooling/agentsync/internal/optimize"
	"github.com/agentic-tooling/agentsync/internal/scan"
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
// and reports its advice. It writes nothing.
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

	rep := optimize.Run(ctx, client, agents, cfg)
	rep.Overrides = overrides
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		emitOptimizeText(stdout, rep, cfg)
	}
	if rep.Count(optimize.StatusUnresolved)+rep.Count(optimize.StatusSkipped) > 0 {
		return exitAtRisk
	}
	return exitOK
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
		real := e.Path
		if r, err := filepath.EvalSymlinks(e.Path); err == nil {
			real = r
		}
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

func emitOptimizeText(w io.Writer, rep optimize.Report, cfg optimize.Config) {
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
	fmt.Fprintf(w, "Spent $%.6f (reported by OpenRouter). Dry run: nothing written.\n", rep.Spent)
}
