package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// minColumn is the narrowest the project column ever gets, matching the scan
// and import reports.
const minColumn = 24

// stringList collects a flag given more than once.
//
// --project and --agent are repeatable rather than comma-separated because
// both take values that may legitimately contain a comma: a project key is a
// path built from real directory names, and nothing forbids one. A separator a
// value can contain is a separator that silently splits a value in half.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty value")
	}
	// Stored exactly as typed. Tidying values here — trimming a trailing
	// separator, say — would also tidy an --agent that is not a bare agent
	// name into one that looks like it, and the validation that rejects those
	// must see what the user actually wrote. Project keys are normalised where
	// they are looked up instead.
	*s = append(*s, v)
	return nil
}

// undeclineFlag collects --undecline=<project>/<name>, repeatable, in the same
// pair form import's --update takes.
//
// It has deliberately no bare form: the equivalent of bare --update would be
// "clear every marker", undoing every deliberate deletion at once. That is also
// why `--undecline KEY` with a space parses correctly here while `--update KEY`
// cannot — this is not a bool flag, so the value is unambiguous. The hazard that
// leaves is `--undecline --apply` swallowing the next flag; state.ParseKey
// refuses a value with no interior slash, so that is a message, not a surprise.
//
// A bad selector is held rather than returned, following updateFlag: the flag
// package would bury the message under the whole usage block.
type undeclineFlag struct {
	keys []state.Key
	seen map[string]bool
	bad  error
}

func (u *undeclineFlag) String() string {
	if u == nil {
		return ""
	}
	out := make([]string, 0, len(u.keys))
	for _, k := range u.keys {
		out = append(out, k.String())
	}
	return strings.Join(out, ",")
}

func (u *undeclineFlag) Set(v string) error {
	k, err := state.ParseKey(v)
	if err != nil {
		if u.bad == nil {
			u.bad = err
		}
		return nil
	}
	// Deduplicated: the same pair twice would otherwise report one cleared
	// marker and one unmatched, and the second would read as a typo.
	if u.seen == nil {
		u.seen = make(map[string]bool)
	}
	if u.seen[k.String()] {
		return nil
	}
	u.seen[k.String()] = true
	u.keys = append(u.keys, k)
	return nil
}

// undeclineHint suggests the attached-value form for a stray argument that could
// plausibly have been a selector, and says nothing for one that could not. Same
// rule as import's updateHint.
func undeclineHint(arg string) string {
	if strings.HasPrefix(arg, "-") || !strings.Contains(arg, "/") {
		return ""
	}
	return fmt.Sprintf(" To clear one declined marker, attach the value to the flag: --%s=%s",
		linker.UndeclineFlag, arg)
}

// clearDeclined removes the selected markers from the in-memory document and
// reports which selectors found one and which did not.
//
// An unmatched selector is reported and does not move the exit code, as with
// importer.UpdateSet.Unmatched: no marker *is* the state the user asked for, so
// failing would make the successful command's own second run the failing one. A
// typo leaves the real marker in place, and the plan reports that agent as
// declined, which is where the user finds out.
func clearDeclined(st *state.File, keys []state.Key) (cleared, unmatched []state.Key) {
	for _, k := range keys {
		if st.Undecline(k) {
			cleared = append(cleared, k)
			continue
		}
		unmatched = append(unmatched, k)
	}
	return cleared, unmatched
}

// runLink replaces project agent files with symlinks into this repo.
//
// The order of operations carries the safety story, as it does for import:
// scan, plan, home guard, then — only with --apply — write. The plan comes
// before the guard here rather than after it, which is a deliberate difference:
// planning is read-only, and it is the plan that determines which directories
// are in scope. --project can name a project the scan never found, so the scan's
// own directory list is not the set the guard has to judge.
func runLink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	rootF := registerRoot(fs)
	repoPath := fs.String("repo", "", "canonical agent repo")
	apply := fs.Bool("apply", false, "create the links; without it this is a dry run")
	excludeLocal := fs.Bool(linker.ExcludeLocalFlag, false,
		"add the agents directory to the project's .git/info/exclude before linking")
	asJSON := fs.Bool("json", false, "emit JSON")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; permit the write anyway")

	var projects, agents stringList
	fs.Var(&projects, linker.ProjectFlag, "restrict to this project key (repeatable)")
	fs.Var(&agents, linker.AgentFlag,
		"also link this agent into the selected projects even if absent (repeatable; requires --"+linker.ProjectFlag+")")

	var undecline undeclineFlag
	fs.Var(&undecline, linker.UndeclineFlag,
		"clear the declined marker for --"+linker.UndeclineFlag+"=<project>/<name> so the agent can be linked again (repeatable; the marker is only cleared with --apply)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr.
		return exitFailure
	}

	if undecline.bad != nil {
		fmt.Fprintf(stderr, "agentsync: --%s: %v\n", linker.UndeclineFlag, undecline.bad)
		return exitFailure
	}

	// Refused rather than ignored, as import does: an argument that lands here
	// is one the user believed was doing something.
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "agentsync: unexpected argument %q; link takes flags only.%s\n",
			rest[0], undeclineHint(rest[0]))
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

	opts := linker.Options{
		Projects:     projects,
		Agents:       agents,
		ExcludeLocal: *excludeLocal,
	}
	// A root derived from the working directory selects that one project, by
	// the same key --project would have named. This is a selection, not an
	// exemption: --agent still requires a project, and still refuses when the
	// root is the default tree and nobody named one.
	if len(opts.Projects) == 0 && root.Project != "" {
		opts.Projects = []string{root.Project}
	}
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		// The root is *why* no project was selected, and saying so is what
		// makes the short form discoverable: run the same command inside the
		// project and the derived root selects it, so this never arises.
		if len(opts.Agents) > 0 && len(opts.Projects) == 0 {
			fmt.Fprintf(stderr, "agentsync: the root is %s, which selects no project — run this from inside the project you mean, or name it with --%s.\n",
				displayPath(repo.Home, root.Path), linker.ProjectFlag)
		}
		return exitFailure
	}

	// Before the scan: a failure to read it must stop the run rather than
	// become a plan built on "nothing was ever declined".
	st, err := state.Load(repo)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// Cleared in memory before planning, so the plan previews what the run
	// will do. Nothing is persisted here — Plan.Apply saves the document and
	// runs only with --apply, so a dry run leaves the markers on disk.
	cleared, unmatched := clearDeclined(st, undecline.keys)

	res, err := repo.Scan(root.Path)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	plan, err := linker.NewPlan(repo, res, st, opts)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// The guard, on the directories the plan is about and before a single
	// byte is written. A HomeError here means nothing is written at all, not
	// even the links that would have been correct: if the keys are wrong then
	// every counterpart lookup behind them is wrong, and a link built on one
	// points a project at another project's agent.
	if err := repo.RequireHome(root.Path, plan.Dirs, *allowOutsideHome); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	run := linkRun{
		repoRoot:  repo.Root,
		root:      root.Path,
		res:       res,
		opts:      opts,
		apply:     *apply,
		plan:      plan,
		statePath: state.Path(repo),
		cleared:   cleared,
		unmatched: unmatched,
	}
	if run.apply {
		run.done, run.errs = plan.Apply(repo, st)
	}

	emitRepoChoice(reportStream(*asJSON, stdout, stderr), repo, repoFrom)
	emitRootChoice(reportStream(*asJSON, stdout, stderr), repo.Home, root)
	if *asJSON {
		if err := run.emitJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		run.emitText(stdout)
	}
	for _, err := range run.errs {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
	}
	// Last, after the summary and after the failures — and only when a link
	// really appeared. Excludes and state records change nothing a session
	// loads, so neither earns this line.
	emitRestartNotice(reportStream(*asJSON, stdout, stderr), len(run.done.Links) > 0)

	return run.exitCode()
}

// linkRun is one invocation: what was planned, what was written, and what went
// wrong. It exists so the report and the exit code are computed from the same
// values rather than from a dozen parameters threaded through both.
type linkRun struct {
	repoRoot string
	root     string
	res      scan.Result
	opts     linker.Options
	apply    bool
	plan     linker.Plan
	done     linker.Applied
	errs     []error

	// statePath is where the run's records go, named in the report because
	// people go and look at this file by hand.
	statePath string

	// cleared and unmatched are the --undecline selectors that found a marker
	// and the ones that did not. Both are reported.
	cleared   []state.Key
	unmatched []state.Key
}

// exitCode maps the run onto the CLI contract.
//
// A write failure and an unjudgeable path both mean the tool could not do its
// job (2). A blocked project, a broken link, a mismatched link, an agent the
// repo cannot supply, and a --project key naming nothing all mean it did its
// job and found something only a human can resolve (1).
//
// The scan's own problems count as unresolved for the same reason they do in
// import: link's exit 0 asserts that the selected projects are provisioned, and
// an unread path may hide a state-6 or state-7 file that would have blocked one
// of them. An assertion cannot be made about a tree that was not fully read.
//
// A foreign link never affects the code. It is somebody else's link, correctly
// left alone, and failing on it forever would make the exit code useless on any
// machine that runs another tool alongside this one.
func (r linkRun) exitCode() int {
	if len(r.errs) > 0 || len(r.plan.Problems) > 0 {
		return exitFailure
	}
	if len(r.plan.Blocked) > 0 || len(r.plan.Unknown) > 0 ||
		len(r.plan.Unresolved()) > 0 || len(r.res.Problems) > 0 {
		return exitAtRisk
	}
	return exitOK
}

// incomplete reports whether the scan failed to look at part of the tree,
// which disqualifies the summary from claiming the selection is provisioned.
func (r linkRun) incomplete() bool {
	return len(r.res.Problems) > 0
}

func (r linkRun) linkCaveat() string {
	if !r.incomplete() {
		return ""
	}
	return ", but the scan could not read every path, so this is NOT proof that every project is provisioned"
}

// linkReport is the --json form. It reports the whole plan, not just the
// writes: what was refused and why is the part a user cannot reconstruct.
type linkReport struct {
	Apply        bool                `json:"apply"`
	ExcludeLocal bool                `json:"exclude_local"`
	Repo         string              `json:"repo"`
	Root         string              `json:"root"`
	Projects     []string            `json:"projects"`
	Agents       []string            `json:"agents"`
	Items        []linker.Item       `json:"items"`
	Counts       map[string]int      `json:"counts"`
	Blocked      []linker.Blocked    `json:"blocked"`
	Git          []linker.ProjectGit `json:"git"`
	Excludes     []linker.Exclude    `json:"excludes"`
	Excluded     []linker.Exclude    `json:"excluded"`
	Unknown      []linker.Unresolved `json:"unknown"`
	Written      []string            `json:"written"`
	Errors       []string            `json:"errors"`
	Problems     []scan.ScanProblem  `json:"problems"`

	// StateFile names the document, and Recorded is what went into it — one
	// entry per link created, empty on a dry run. Ownership is read from
	// here, never inferred from Written, which says only that a link appeared.
	StateFile string         `json:"state_file"`
	Recorded  []state.Record `json:"recorded"`

	// Undeclined are the markers cleared; UndeclineUnmatched the selectors
	// that named none. The second does not move the exit code.
	Undeclined         []string `json:"undeclined"`
	UndeclineUnmatched []string `json:"undecline_unmatched"`

	// ScanProblems are paths in the scanned projects that could not be
	// classified. They are not link failures, but they mean this run is not
	// proof that every project is provisioned.
	ScanProblems []scan.ScanProblem `json:"scan_problems"`
}

func (r linkRun) emitJSON(w io.Writer) error {
	rep := linkReport{
		Apply:        r.apply,
		ExcludeLocal: r.opts.ExcludeLocal,
		Repo:         r.repoRoot,
		Root:         r.root,
		Projects:     nonNil(r.opts.Projects),
		Agents:       nonNil(r.opts.Agents),
		Items:        r.plan.Items,
		Counts:       make(map[string]int),
		Blocked:      r.plan.Blocked,
		Git:          r.plan.Git,
		Excludes:     r.plan.Excludes,
		Excluded:     nonNilExcludes(r.done.Excludes),
		Unknown:      r.plan.Unknown,
		Written:      make([]string, 0, len(r.done.Links)),
		Errors:       make([]string, 0, len(r.errs)),
		Problems:     r.plan.Problems,
		ScanProblems: r.res.Problems,

		StateFile: r.statePath,
		// Non-nil so the JSON carries empty arrays rather than nulls, the
		// rule every other array this tool emits follows.
		Recorded:           nonNilRecords(r.done.Recorded),
		Undeclined:         keyStrings(r.cleared),
		UndeclineUnmatched: keyStrings(r.unmatched),
	}
	for action, n := range r.plan.Counts() {
		rep.Counts[action.String()] = n
	}
	for _, item := range r.done.Links {
		rep.Written = append(rep.Written, item.Path)
	}
	for _, err := range r.errs {
		rep.Errors = append(rep.Errors, err.Error())
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// nonNil keeps an unset repeatable flag out of the JSON as [] rather than
// null, matching every other array this tool emits.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilExcludes(s []linker.Exclude) []linker.Exclude {
	if s == nil {
		return []linker.Exclude{}
	}
	return s
}

func nonNilRecords(s []state.Record) []state.Record {
	if s == nil {
		return []state.Record{}
	}
	return s
}

// keyStrings renders project/name pairs the way the flag that selects them is
// spelled, so a value in the JSON can be pasted straight back onto a command
// line.
func keyStrings(keys []state.Key) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	return out
}

// linkActionOrder controls report grouping: what needs a human first, what the
// run would do next, what it left alone last.
var linkActionOrder = []linker.Action{
	linker.ActionBroken,
	linker.ActionMismatch,
	linker.ActionMissing,
	// Grouped with the things that explain why a requested agent was not
	// linked: a skip nobody can see is how this becomes a bug report.
	linker.ActionDeclined,
	linker.ActionCreate,
	linker.ActionAdopt,
	linker.ActionOverride,
	linker.ActionUnadopted,
	linker.ActionForeign,
	linker.ActionLinked,
}

var linkActionMarker = map[linker.Action]string{
	linker.ActionBroken:   "!!",
	linker.ActionMismatch: "!!",
	linker.ActionMissing:  "!!",
	// Not "!!": the marker records a decision the user already made, and
	// flagging it as a problem would argue with them on every run.
	linker.ActionDeclined:  " -",
	linker.ActionCreate:    " +",
	linker.ActionAdopt:     " ~",
	linker.ActionOverride:  "  ",
	linker.ActionUnadopted: "  ",
	linker.ActionForeign:   "  ",
	linker.ActionLinked:    " =",
}

var linkActionNote = map[linker.Action]string{
	linker.ActionBroken:    "link into the repo whose target is gone — NOT unlinked",
	linker.ActionMismatch:  "linked to the wrong file in this repo — NOT repointed",
	linker.ActionMissing:   "requested, but this repo has no such agent",
	linker.ActionDeclined:  "deleted on purpose; NOT recreated",
	linker.ActionCreate:    "absent and requested; create the link",
	linker.ActionAdopt:     "identical to its counterpart; replace it with a link",
	linker.ActionOverride:  "customized; left alone",
	linker.ActionUnadopted: "no copy in this repo; left alone",
	linker.ActionForeign:   "points outside this repo — not ours",
	linker.ActionLinked:    "already linked to its counterpart",
}

func (r linkRun) emitText(w io.Writer) {
	fmt.Fprintf(w, "Scanned %s against %s\n", r.root, r.repoRoot)
	fmt.Fprintf(w, "%d agent director%s in scope, %d agent%s considered\n\n",
		len(r.plan.Dirs), plural(len(r.plan.Dirs), "y", "ies"),
		len(r.plan.Items), plural(len(r.plan.Items), "", "s"))

	emitBlocked(w, r.plan.Blocked)
	emitUnknown(w, r.plan.Unknown)
	emitUndeclined(w, r.cleared, r.unmatched, r.apply)
	emitExcludes(w, r.plan.Excludes, r.apply)
	emitProjectGit(w, r.plan.Git)

	if len(r.plan.Items) == 0 {
		fmt.Fprintln(w, "Nothing to link: no agent in scope needs anything doing.")
		fmt.Fprintln(w)
	} else {
		emitLinkGroups(w, r.plan.Items)
	}

	emitImportProblems(w, r.plan.Problems, r.res.Problems)

	fmt.Fprintln(w, strings.Repeat("-", 60))
	r.emitSummary(w)
}

func (r linkRun) emitSummary(w io.Writer) {
	counts := r.plan.Counts()
	pending := len(r.plan.Writes())

	switch {
	case !r.apply && pending > 0:
		fmt.Fprintf(w, "DRY RUN — nothing was written. Pass --apply to link %d file%s.\n",
			pending, plural(pending, "", "s"))
	case !r.apply:
		fmt.Fprintln(w, "DRY RUN — nothing was written, and there is nothing to write.")
	case pending == 0:
		fmt.Fprintf(w, "Nothing was written: there was nothing to link%s.\n", r.linkCaveat())
	default:
		// "n of m" rather than "n": a run where some writes failed must
		// not read like a complete one in its headline sentence.
		fmt.Fprintf(w, "Linked %d of %d file%s%s.\n",
			len(r.done.Links), pending, plural(pending, "", "s"), r.linkCaveat())
	}

	// Keyed off the plan, not off what was written, so the caveat also appears
	// in the dry run — the mode where the user is still deciding whether to
	// pass --apply, and so the only mode where it can change the decision.
	if n := len(r.plan.Excludes); n > 0 {
		if r.apply {
			fmt.Fprintf(w, "%d of %d project%s .git/info/exclude gained an entry so the links stay local to this machine.\n",
				len(r.done.Excludes), n, plural(n, "'s", "s'"))
		} else {
			fmt.Fprintf(w, "%d project%s .git/info/exclude would gain an entry so the links stay local to this machine.\n",
				n, plural(n, "'s", "s'"))
		}
		fmt.Fprintln(w, "Excluding does not untrack anything: a file already committed under that directory stays tracked, so replacing it with a link still shows as a change in that project.")
	}
	if n := len(r.plan.Blocked); n > 0 {
		fmt.Fprintf(w, "%d project%s refused entirely (see above). Nothing in %s was touched.\n",
			n, plural(n, "", "s"), plural(n, "it", "them"))
	}
	if n := counts[linker.ActionBroken]; n > 0 {
		fmt.Fprintf(w, "%d broken link%s left in place. Restore the agent in this repo or remove the link by hand; link never unlinks.\n",
			n, plural(n, "", "s"))
	}
	if n := counts[linker.ActionMismatch]; n > 0 {
		fmt.Fprintf(w, "%d link%s point%s at a repo file that is not this project's counterpart. link does not repoint — a link somebody made deliberately is not ours to swap. To adopt the counterpart, remove the link and re-run with --%s and --%s (see each line above for the exact command).\n",
			n, plural(n, "", "s"), plural(n, "s", ""), linker.ProjectFlag, linker.AgentFlag)
	}
	if n := counts[linker.ActionMissing]; n > 0 {
		fmt.Fprintf(w, "%d requested agent%s not exist in this repo.\n",
			n, plural(n, " does", "s do"))
	}
	if n := counts[linker.ActionDeclined]; n > 0 {
		// Says what to type: a refusal is only reasonable if the user is
		// told what changing it looks like. It does not fail the run.
		fmt.Fprintf(w, "%d requested agent%s declined: a previous reconcile recorded that the link was deleted on purpose, so link did not recreate %s. Pass --%s=<project>/<name> with --apply to clear a marker and link it again.\n",
			n, plural(n, " is", "s are"), plural(n, "it", "them"), linker.UndeclineFlag)
	}
	if n := len(r.cleared); n > 0 {
		if r.apply {
			fmt.Fprintf(w, "%d declined marker%s cleared in %s.\n",
				n, plural(n, " was", "s were"), r.statePath)
		} else {
			fmt.Fprintf(w, "%d declined marker%s be cleared in %s — nothing was written, since clearing one is a write like any other.\n",
				n, plural(n, " would", "s would"), r.statePath)
		}
	}
	if n := len(r.unmatched); n > 0 {
		fmt.Fprintf(w, "%d --%s selector%s named no declined marker (%s); nothing was declined for %s, which is the state you asked for. If one of those is a typo, the agent it meant is still declined and is listed above.\n",
			n, linker.UndeclineFlag, plural(n, "", "s"),
			strings.Join(keyStrings(r.unmatched), ", "), plural(n, "it", "them"))
	}
	if n := len(r.done.Recorded); n > 0 {
		fmt.Fprintf(w, "%d link%s recorded in %s, which is what a later pass reads to tell these links from handmade ones.\n",
			n, plural(n, "", "s"), r.statePath)
	}
	// Separate from the count above, because the gap is the point: a link
	// that exists but was not recorded is unowned until an adopt pass.
	if n := len(r.done.Links) - len(r.done.Recorded); n > 0 {
		fmt.Fprintf(w, "%d link%s created but NOT recorded (see stderr); %s unowned until an adopt pass records %s.\n",
			n, plural(n, " was", "s were"), plural(n, "it is", "they are"), plural(n, "it", "them"))
	}
	if n := len(r.plan.Unknown); n > 0 {
		fmt.Fprintf(w, "%d --%s key%s named no project.\n", n, linker.ProjectFlag, plural(n, "", "s"))
	}
	if n := counts[linker.ActionForeign]; n > 0 {
		fmt.Fprintf(w, "%d foreign link%s untouched; %s belong%s to something else.\n",
			n, plural(n, "", "s"), plural(n, "it", "they"), plural(n, "s", ""))
	}
	if n := len(r.plan.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s could not be judged and %s left untouched.\n",
			n, plural(n, "", "s"), plural(n, "was", "were"))
	}
	if n := len(r.errs); n > 0 {
		fmt.Fprintf(w, "%d write%s FAILED (see stderr); this run did not link everything it planned to.\n",
			n, plural(n, "", "s"))
	}
	if n := len(r.res.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s in the scanned tree could not be classified and may hide uncaptured files. Resolve %s and re-run.\n",
			n, plural(n, "", "s"), plural(n, "it", "them"))
	}
}

// emitBlocked prints the refusals, naming every file that caused one. A count
// alone would tell a user they are blocked without telling them what to fix.
func emitBlocked(w io.Writer, blocked []linker.Blocked) {
	if len(blocked) == 0 {
		return
	}
	fmt.Fprintf(w, "!! blocked (%d) — link refuses to touch these projects at all\n", len(blocked))
	for _, b := range blocked {
		fmt.Fprintf(w, "     %s — %s\n", b.Project, b.Reason)
		for _, e := range b.AtRisk {
			fmt.Fprintf(w, "       %-28s %s — content is protected nowhere\n", e.Name, e.State)
		}
	}
	fmt.Fprintln(w)
}

// emitExcludes prints the local exclusions this run would add, or added. They
// are edits to somebody else's repository, so they are reported in their own
// right rather than folded into a summary line.
func emitExcludes(w io.Writer, excludes []linker.Exclude, applied bool) {
	if len(excludes) == 0 {
		return
	}
	verb := "would be added"
	if applied {
		verb = "added"
	}
	fmt.Fprintf(w, " ~ exclude (%d) — %s to .git/info/exclude so these links stay on this machine\n",
		len(excludes), verb)
	for _, ex := range excludes {
		fmt.Fprintf(w, "     %s\n", ex)
	}
	fmt.Fprintln(w)
}

// emitProjectGit prints what each written-to project's own git said about its
// agents directory. "Your links are invisible to this repo" is a fact about
// somebody's project that they cannot get from the exit code.
func emitProjectGit(w io.Writer, git []linker.ProjectGit) {
	if len(git) == 0 {
		return
	}
	fmt.Fprintf(w, "   git (%d) — what each project's own repository says about its agents directory\n", len(git))
	// Sized to the widest key here rather than shared with the item groups:
	// these are the projects being written into, a different and usually
	// smaller set, and borrowing the other column's width would leave this one
	// either ragged or needlessly wide.
	width := minColumn
	for _, g := range git {
		if n := utf8.RuneCountInString(g.Project); n > width {
			width = n
		}
	}
	for _, g := range git {
		fmt.Fprintf(w, "     %-*s %-8s %s\n", width, g.Project, g.Tracking, g.Note)
	}
	fmt.Fprintln(w)
}

// emitUndeclined prints the markers this run cleared, or would clear, and the
// selectors that named none. Clearing a marker undoes a deliberate decision, so
// it gets its own section rather than a summary line — as the git exclusions do.
func emitUndeclined(w io.Writer, cleared, unmatched []state.Key, applied bool) {
	if len(cleared) > 0 {
		verb := "would be cleared"
		if applied {
			verb = "cleared"
		}
		fmt.Fprintf(w, " ~ undecline (%d) — %s, so these agents can be linked again\n", len(cleared), verb)
		for _, k := range cleared {
			fmt.Fprintf(w, "     %s\n", k)
		}
		fmt.Fprintln(w)
	}
	if len(unmatched) > 0 {
		fmt.Fprintf(w, "   undecline (%d) — no declined marker to clear; nothing was declined for these\n", len(unmatched))
		for _, k := range unmatched {
			fmt.Fprintf(w, "     %s\n", k)
		}
		fmt.Fprintln(w)
	}
}

func emitUnknown(w io.Writer, unknown []linker.Unresolved) {
	if len(unknown) == 0 {
		return
	}
	fmt.Fprintf(w, "!! unknown (%d) — --%s named no project\n", len(unknown), linker.ProjectFlag)
	for _, u := range unknown {
		fmt.Fprintf(w, "     %s\n", u)
	}
	fmt.Fprintln(w)
}

func emitLinkGroups(w io.Writer, items []linker.Item) {
	width := linkProjectColumn(items)

	byAction := make(map[linker.Action][]linker.Item)
	for _, i := range items {
		byAction[i.Action] = append(byAction[i.Action], i)
	}

	for _, action := range linkActionOrder {
		group := byAction[action]
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s %s (%d) — %s\n",
			linkActionMarker[action], action, len(group), linkActionNote[action])
		for _, item := range group {
			// Trimmed rather than formatted exactly: an item with no
			// reason would otherwise leave trailing whitespace in a
			// report people paste into issues and diffs.
			line := fmt.Sprintf("     %-*s %-28s %s",
				width, item.Project, item.Name, item.Reason)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
		fmt.Fprintln(w)
	}
}

// linkProjectColumn sizes the project column to the widest key printed, the
// same way the scan and import reports do: project keys are paths, so a fixed
// width goes ragged the moment a nested project appears.
func linkProjectColumn(items []linker.Item) int {
	width := minColumn
	for _, i := range items {
		if n := utf8.RuneCountInString(i.Project); n > width {
			width = n
		}
	}
	return width
}
