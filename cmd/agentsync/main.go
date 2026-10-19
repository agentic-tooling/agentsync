// Command agentsync inspects how project-local Claude agents relate to the
// canonical agent repository, and captures the ones that exist nowhere else.
//
// scan is strictly read-only: it classifies what is on disk and reports it.
//
// import writes, but only into this repo — it copies project files that exist
// only on disk into project-agents/<key>/, and never touches the project's own
// .claude/agents directory. It is a dry run unless --apply is given.
//
// link is the only command that writes into a project's .claude/agents
// directory. It replaces real agent files with symlinks into this repo, and it
// refuses to operate in any project holding a file this repo has no copy of —
// the invariant import exists to satisfy. It too is a dry run unless --apply is
// given.
//
// doctor is read-only like scan, and reports the drift the other three do not:
// broken and foreign links, links pointing at the wrong file in this repo, and
// captures in project-agents/ whose project no longer has them. It has no
// --apply because it has nothing to apply — see the doctor package doc for why
// repair is deliberately a separate command.
//
// reconcile is the command doctor defers to. It acts on the same drift, but
// only where a recorded fact authorises the act: the state file for whether
// this tool created a link, and scan.CommittedInHEAD for whether a capture's
// bytes survive its deletion. Dry run without --apply, like import and link.
//
// inventory writes too, but only inventory.json in this repo, and without an
// --apply. The distinction is not an oversight: --apply guards writes that can
// destroy something, and this command's only output is a derived file it alone
// owns, recomputed from the filesystem every run. Regenerating it is the
// documented fix for any inconsistency rather than a risk to be gated — a
// materialized view that is unsafe to re-run is not a materialized view. Its
// output is derived and must never be used to decide a write; see the inventory
// package doc.
//
// init writes only agentsync's own config file, from values the user named or
// confirmed, and refuses to replace an existing one without --force.
//
// optimize is the only command that uses the network: it asks an OpenRouter
// advisor about each agent's model and effort, and writes nothing.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/agentic-tooling/agentsync/internal/config"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

const usage = `agentsync — inspect project-local Claude agents against the canonical repo

Usage:
  agentsync init [flags]      find your agent repo and write the config file
  agentsync scan [flags]      classify project agents against the repo (read-only)
  agentsync import [flags]    capture project-only agents into project-agents/
  agentsync link [flags]      replace project agent files with symlinks into the repo
  agentsync doctor [flags]    report broken/foreign/mismatched links and stale captures
  agentsync reconcile [flags] act on that drift, gated on what this tool recorded
  agentsync inventory [flags] regenerate inventory.json: every agent on this machine
  agentsync optimize [flags]  ask an OpenRouter advisor which model and effort each
                              agent should use (report only; the one networked command)
  agentsync version

Common flags:
  --root PATH       directory tree to scan for projects. Without it the root is
                    derived from the working directory: the first ancestor
                    below $HOME holding .claude/agents/, else .claude/, else
                    .git/ — so running inside a project means that project, and
                    for link it also selects that project as if --project had
                    named it. Nothing found before $HOME is refused rather
                    than widened: pass --all or --root to scan from there.
                    Every run prints the root it chose and how it decided.
  --all             scan the whole default tree ($AGENTSYNC_ROOT, else root in the
                    config file) rather than the project the working directory
                    is in. Refused together with --root: the two say different
                    things and neither should quietly win.

inventory is the one command that does NOT derive a root: it describes the whole
machine by definition, so --root still names a tree but the default is the whole
default tree and never the project you are standing in. --all is accepted there
and means what the default already means.
  --repo PATH       canonical agent repo (default $AGENTSYNC_REPO, else repo in the
                    config file; required from one of the three)
  --json            emit machine-readable JSON
  --allow-outside-home
                    your projects legitimately live outside $HOME; do not warn
                    (scan, doctor, and a reconcile dry run) or refuse (import,
                    link, reconcile --apply) that every project key falls under
                    _outside-home/

Config file: $XDG_CONFIG_HOME/agentsync/config.toml, else
~/.config/agentsync/config.toml. It may set repo and root; a flag beats the
environment, which beats the file. A file that exists but is broken refuses.
agentsync init writes it.

init flags:
  --repo PATH       the canonical agent repo to write (must hold agents/)
  --root PATH       the default tree to write (default shown: the repo's parent)
  --apply           write the file; without it init is a dry run
  --force           replace an existing config file, which is otherwise refused
                    with exit 1 and its contents shown
  --search PATH     where to look for agent repos when --repo is not given
                    (default ~, four levels deep)

In a terminal with neither --repo nor --root, init lists the repos it found,
asks which to use and which default tree, shows the file, and writes it only
after you answer y. Anywhere else it never prompts: without --repo it lists
the candidates and prints the exact command to run, and it writes only with
--apply and both --repo and --root.

scan flags:
  --at-risk         list only entries whose content is protected nowhere
  --fail-on-risk    exit 1 if any at-risk entry is found (for CI)

import flags:
  --apply           write the captures; without it import is a dry run
  --update=KEY      replace the captured copy of one file, named as the
                    <project>/<name> pair the report prints (repeatable).
                    Attach the value with '='; import takes no positional
                    arguments, so '--update KEY' is refused rather than read
                    as the blanket form below.
  --update          bare, with no value: replace every captured copy that
                    differs, across the whole run. Still supported, but the
                    per-file form above is the one to reach for — the decision
                    is made one file at a time after looking at a diff.
                    Neither form replaces a capture whose current bytes are in
                    no commit in this repo — untracked, or edited since it was
                    last committed. That copy may be the only record of an
                    older override, and uncommitted bytes exist nowhere else.
                    The check is on the content, not the path: --update itself
                    leaves a capture modified against HEAD, so a path-only
                    check would let the next --update destroy this one's work.
  --include-tracked also capture files that are committed in their own
                    project's git repository, which are not at risk
  --fail-on-pending exit 1 if anything is left uncaptured (for CI). Without
                    it a dry run exits 0 whether or not captures are pending,
                    so its exit code says nothing about what is protected.
                    With --apply it fires only on writes that did not happen.

link flags:
  --apply           create the links; without it link is a dry run
  --project KEY     restrict to this project key (repeatable; use the key
                    scan reports, not a directory name)
  --agent NAME      also link this agent into the selected projects even if
                    no file is present (repeatable; requires --project)
  --exclude-local   add the agents directory to the project's
                    .git/info/exclude before linking. Without it, a project
                    whose own git tracks that directory is refused: a symlink
                    into this repo names an absolute path that resolves on
                    this machine only, so committing one breaks every other
                    clone. info/exclude is used rather than .gitignore
                    because .gitignore is tracked and shared.
  --undecline=KEY   clear the declined marker for one agent, named as the
                    <project>/<name> pair the report prints (repeatable), so
                    it can be linked again. A declined marker records that
                    somebody deleted that link on purpose; link reports it and
                    never recreates it. Clearing one is a write, so it needs
                    --apply. There is no bare form.

When link --apply or reconcile --apply actually creates or removes a symlink,
the run's last line says the Claude Code session must be restarted: agents are
read when a session starts, so the session you are sitting in is unaffected.

link --apply records every link it creates in <repo>/.agentsync-state.json,
with the target's content hash. That record is what later tells this tool's
links from handmade ones; nothing is ever removed without one. Gitignored.
reconcile is the command that reads it back and acts.

doctor flags:
  (no flags of its own — it takes only the common ones above)

reconcile flags:
  --apply           make the changes; without it reconcile is a dry run
  --adopt           record every currently-healthy link this tool has no
                    record of, with origin 'adopted'. It exists because links
                    created before the state file existed are unowned, and
                    nothing may remove an unowned link. This pass CANNOT tell
                    such a link from one somebody made by hand — they are
                    byte-identical on disk — which is why removing an adopted
                    record later needs --prune-adopted as well.
  --prune           remove a recorded link that is broken and still points
                    exactly where the record says. Never a link with no
                    record, never a foreign link, never a real file, and
                    never one whose target came back.
  --prune-adopted   widen --prune to records an adopt pass made. Requires
                    --prune; on its own it authorises nothing.
  --rekey=OLD=NEW   move project-agents/OLD/ to project-agents/NEW/ and
                    repoint the links that pointed into it (repeatable).
                    Explicit only: reconcile proposes candidates in its
                    report and applies none of them. Refused if NEW already
                    holds captures — merging two projects' captures is what
                    this gate exists to prevent.
  --drop-capture=KEY
                    delete one capture, named as the <project>/<name> pair
                    the report prints (repeatable). Refused outright unless
                    that capture's current bytes are in this repo's HEAD, so
                    'git show' still has them. "Could not ask git" refuses
                    with the rest. There is no bare form and no bulk form,
                    and no flag deletes an orphaned capture directory.

Without --apply, and without any of the flags above, reconcile is a report:
what it could do, what it refuses to do, and why. A declined marker is the one
write no flag guards — it records that a link was deleted and nothing took its
place, which is an observation rather than a choice, and link --undecline
reverses it.

inventory flags:
  --out PATH        write here instead of <repo>/inventory.json
  --stdout          write the document to stdout and leave the file alone
  --check           write nothing; exit 1 if the file on disk is not what a
                    regeneration would produce. The document holds nothing
                    volatile — no timestamp, no tool version — so a difference
                    is a real change in the tree, which is what makes this a
                    freshness test rather than a clock reading.

optimize flags:
  --advisor MODEL   OpenRouter decision model to ask (default ~typesafe/jev-latest;
                    cloudflare/clef also works)
  --max-cost USD    skip any request that could take reported spend past this
                    (default 0.50)
  --min-confidence P
                    least probability a pick needs before it is suggested
                    (default 0.6); below it the row says keep (low confidence)
  --agent NAME      ask only about this agent (repeatable)

optimize needs $OPENROUTER_API_KEY and sends each agent file (first 24 KB) to
OpenRouter, one request per agent with no retries. It reads model: and effort:
from each agent's frontmatter and writes nothing. Exit 1 when a row is
unresolved or skipped.

scan and doctor are read-only. import writes only into this repo, only with
--apply, and never into a project's .claude/agents directory. link is the one
command that writes into a project, only with --apply, and it refuses any
project holding a file this repo has no copy of — import that first.

inventory also writes only into this repo, and takes no --apply: its sole
output is inventory.json, which it owns, recomputes from scratch every run, and
overwrites atomically. That file is derived — the filesystem is the source of
truth and regenerating is the documented fix for any inconsistency — so nothing
may decide a write from it. Read it for orientation; re-scan before acting.

doctor never writes anywhere at all: it reports broken links, foreign links,
links pointing at the wrong file in this repo, captures whose project no longer
has a live file, and capture directories whose project is gone. It never
unlinks and never deletes a capture — a stale capture may be the only surviving
copy, which is what project-agents/ is for.

reconcile acts on that same drift, and only where a gate can prove the act is
safe: <repo>/.agentsync-state.json says which links this tool created, and
scan's HEAD check says which captures are recoverable from a commit. It never
removes a link with no record, never a foreign link, and never a capture whose
bytes are in no commit. doctor's exit code, not reconcile's, is what asserts
that nothing has drifted.

Exit codes: 0 clean, 1 something needs resolving (at-risk files, conflicts, a
blocked or broken project, or a path the scan could not read), 2 the tool could
not do its job.
`

// Exit codes. These are part of the CLI contract: CI keys off 1 meaning
// "at-risk files exist" and 2 meaning "the tool could not do its job".
const (
	exitOK      = 0
	exitAtRisk  = 1
	exitFailure = 2
)

// exitConflict is exitAtRisk under the name import uses for it. It is
// deliberately the same code: both mean "the run completed and found something
// only you can resolve", and a conflict is an at-risk file this run could not
// capture. CI keys off the number, which must not depend on the subcommand.
const exitConflict = exitAtRisk

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return exitOK
	}

	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "link":
		return runLink(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "reconcile":
		return runReconcile(args[1:], stdout, stderr)
	case "inventory":
		return runInventory(args[1:], stdout, stderr)
	case "optimize":
		return runOptimize(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "-v", "--version", "version":
		return runVersion(stdout)
	default:
		fmt.Fprintf(stderr, "agentsync: unknown command %q (try: agentsync help)\n", args[0])
		return exitFailure
	}
}

// Environment variables naming the two trees this tool cannot guess.
const (
	envRepo = "AGENTSYNC_REPO"
	envRoot = "AGENTSYNC_ROOT"
)

// loadConfig reads the config file. Callers reach it only once flag and env
// have left a value unset, so a broken file never blocks a run that named both.
func loadConfig() (config.Config, string, error) {
	path, err := config.Path()
	if err != nil {
		return config.Config{}, "", err
	}
	c, err := config.Load(path)
	return c, path, err
}

// openRepo resolves the repo by flag, then $AGENTSYNC_REPO, then the config
// file, and refuses rather than guessing: every link this tool writes points
// into it. The string returned names where the path came from.
func openRepo(flagValue string) (*scan.Repo, string, error) {
	path, from := strings.TrimSpace(flagValue), "--repo"
	if path == "" {
		path, from = strings.TrimSpace(os.Getenv(envRepo)), "$"+envRepo
	}
	if path == "" {
		c, cfgPath, err := loadConfig()
		if err != nil {
			return nil, "", err
		}
		if c.Repo == "" {
			return nil, "", notConfigured("canonical agent repo", "repo", envRepo, cfgPath)
		}
		path, from = c.Repo, "config file "+cfgPath
	}
	repo, err := scan.NewRepo(path)
	if err != nil {
		return nil, "", fmt.Errorf("repo from %s: %w", from, err)
	}
	return repo, from, nil
}

// notConfigured is the refusal for a setting no source supplied. key is both
// the flag name and the config key.
func notConfigured(what, key, env, cfgPath string) error {
	return fmt.Errorf("no %s configured.\n  Create a config file: agentsync init\n  Pass it each run: --%s PATH (or set $%s)\n  Or set %s in %s", what, key, env, key, cfgPath)
}

// emitRepoChoice prints which repo this run uses and where that came from,
// beside the Root line and under the same stdout/stderr rule.
func emitRepoChoice(w io.Writer, repo *scan.Repo, from string) {
	fmt.Fprintf(w, "Repo: %s (from %s).\n", displayPath(repo.Home, repo.Root), from)
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	rootF := registerRoot(fs)
	repoPath := fs.String("repo", "", "canonical agent repo")
	asJSON := fs.Bool("json", false, "emit JSON")
	atRiskOnly := fs.Bool("at-risk", false, "list only at-risk entries")
	failOnRisk := fs.Bool("fail-on-risk", false, "exit 1 if any at-risk entry is found")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; suppress the home warning")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr. Printing it again would double up.
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
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// After the scan, because the projects it found are what decides whether
	// $HOME is the right one — and before the report, so a terminal user
	// reads the caveat before the numbers it applies to.
	warnHome(stderr, repo, root.Path, res.Dirs, *allowOutsideHome)

	emitRepoChoice(reportStream(*asJSON, stdout, stderr), repo, repoFrom)
	emitRootChoice(reportStream(*asJSON, stdout, stderr), repo.Home, root)
	if *asJSON {
		if err := emitJSON(stdout, res, *atRiskOnly); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		emitText(stdout, res, repo.Root, root.Path, *atRiskOnly)
	}

	if *failOnRisk && len(res.AtRisk()) > 0 {
		return exitAtRisk
	}
	return exitOK
}

// warnHome reports a suspect $HOME after the scan and before the report, and
// returns whether it said anything.
//
// It runs on the directories the scan found rather than ahead of it, because
// the check asks the same question ProjectKey answers, of the same directories
// the scan just keyed. The warning and the keying it predicts therefore cannot
// drift: it can neither warn about a re-keying that will not happen nor stay
// silent about one that will.
//
// Warning, not failing. A wrong $HOME makes scan over-report — files protected
// under the real home look unprotected — which fails safe, and the same
// condition is produced legitimately by anyone whose projects live outside
// $HOME. Turning it into an error would break that case and change the meaning
// of an exit code CI already keys off. The write paths take the same
// classification at error severity via scan.RequireHome; a read-only command
// only has to make sure the user is not left guessing.
//
// It goes to stderr for the same reason every diagnostic here does: stdout
// carries the report, and with --json it must stay machine-parseable.
func warnHome(stderr io.Writer, repo *scan.Repo, root string, dirs []string, allowOutsideHome bool) bool {
	check := repo.CheckHome(root, dirs)
	if check.OK() || check.SuppressedBy(allowOutsideHome) {
		return false
	}
	fmt.Fprintf(stderr, "agentsync: warning: %s\n", check.Explain())
	return true
}

// filterAtRisk narrows a result to the entries that are unprotected everywhere.
//
// The filter is per-entry, never per-state: a state such as "override" only
// says the agent repo lacks a copy, while Entry.AtRisk also accounts for the
// file being committed in its own project. Text and JSON must apply the same
// filter or --at-risk means two different things depending on the format, and
// neither would match --fail-on-risk.
func filterAtRisk(res scan.Result) scan.Result {
	return scan.Result{
		Entries:  res.AtRisk(),
		Dirs:     res.Dirs,
		Problems: res.Problems,
	}
}

func emitJSON(w io.Writer, res scan.Result, atRiskOnly bool) error {
	out := res
	if atRiskOnly {
		out = filterAtRisk(res)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// stateOrder controls report grouping: most urgent first.
var stateOrder = []scan.State{
	scan.StateUnadopted,
	scan.StateOverride,
	scan.StateBrokenLink,
	scan.StateForeignLink,
	scan.StateAdoptable,
	scan.StateHealthyLink,
}

var stateNote = map[scan.State]string{
	scan.StateUnadopted:   "exists ONLY on disk — no copy in the repo",
	scan.StateOverride:    "customized; differs from its repo counterpart",
	scan.StateBrokenLink:  "link into the repo whose target is gone",
	scan.StateForeignLink: "link pointing outside the repo — not ours",
	scan.StateAdoptable:   "identical to its repo counterpart; safe to link",
	scan.StateHealthyLink: "already linked to the repo",
}

func emitText(w io.Writer, res scan.Result, repoRoot, root string, atRiskOnly bool) {
	shown := res
	if atRiskOnly {
		shown = filterAtRisk(res)
	}

	fmt.Fprintf(w, "Scanned %s against %s\n", root, repoRoot)
	fmt.Fprintf(w, "%d agent director%s, %d file%s\n\n",
		len(res.Dirs), plural(len(res.Dirs), "y", "ies"),
		len(res.Entries), plural(len(res.Entries), "", "s"))

	if len(shown.Entries) == 0 {
		if atRiskOnly {
			fmt.Fprintln(w, "No at-risk agents found.")
		} else {
			fmt.Fprintln(w, "No project-local agents found.")
		}
		fmt.Fprintln(w)
	} else {
		emitGroups(w, shown.Entries)
	}

	emitProblems(w, res.Problems)

	fmt.Fprintln(w, strings.Repeat("-", 60))
	risk := res.AtRisk()
	if len(risk) == 0 {
		if len(res.Problems) > 0 {
			// Never let an incomplete scan read as an all-clear.
			fmt.Fprintf(w, "No at-risk files among what could be read — but %d path%s could not be checked (see above).\n",
				len(res.Problems), plural(len(res.Problems), "", "s"))
			return
		}
		fmt.Fprintln(w, "No at-risk files: every project agent is captured in the repo.")
		return
	}
	fmt.Fprintf(w, "%d file%s hold content that is protected nowhere.\n",
		len(risk), plural(len(risk), "", "s"))
	fmt.Fprintln(w, "Import these before anything writes to those directories.")
	if len(res.Problems) > 0 {
		fmt.Fprintf(w, "%d path%s could not be checked and may hide more.\n",
			len(res.Problems), plural(len(res.Problems), "", "s"))
	}
}

// projectColumn is the width of the project column, sized to the widest key
// actually being printed. Project keys are paths now, not bare directory
// names, so a fixed width silently ragged the report the moment a nested
// project appeared. Width is counted in runes because that is what fmt pads by.
func projectColumn(entries []scan.Entry) int {
	const minWidth = 24
	width := minWidth
	for _, e := range entries {
		if n := utf8.RuneCountInString(e.Project); n > width {
			width = n
		}
	}
	return width
}

func emitGroups(w io.Writer, entries []scan.Entry) {
	width := projectColumn(entries)

	byState := make(map[scan.State][]scan.Entry)
	for _, e := range entries {
		byState[e.State] = append(byState[e.State], e)
	}

	for _, st := range stateOrder {
		group := byState[st]
		if len(group) == 0 {
			continue
		}

		// Count how many of this group are genuinely unprotected. A state
		// that lacks a repo copy is not automatically a problem: the file
		// may be committed in its own project.
		risky := 0
		for _, e := range group {
			if e.AtRisk() {
				risky++
			}
		}

		marker := "  "
		if risky > 0 {
			marker = "!!"
		}
		header := fmt.Sprintf("%s %s (%d)", marker, st, len(group))
		if st.AtRisk() && risky != len(group) {
			header += fmt.Sprintf(" — %d unprotected", risky)
		}
		fmt.Fprintf(w, "%s — %s\n", header, stateNote[st])

		sort.Slice(group, func(i, j int) bool {
			if group[i].Project != group[j].Project {
				return group[i].Project < group[j].Project
			}
			return group[i].Name < group[j].Name
		})
		for _, e := range group {
			note := ""
			if st.AtRisk() {
				if e.AtRisk() {
					note = "  UNPROTECTED"
				} else {
					note = "  (committed in its own repo)"
				}
			}
			fmt.Fprintf(w, "     %-*s %-28s%s\n", width, e.Project, e.Name, note)
		}
		fmt.Fprintln(w)
	}
}

func emitProblems(w io.Writer, problems []scan.ScanProblem) {
	if len(problems) == 0 {
		return
	}
	fmt.Fprintf(w, "?? problems (%d) — NOT classified; these paths were not checked for risk\n",
		len(problems))
	sorted := make([]scan.ScanProblem, len(problems))
	copy(sorted, problems)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, p := range sorted {
		fmt.Fprintf(w, "     %s\n", p)
	}
	fmt.Fprintln(w)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
