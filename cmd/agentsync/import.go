package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/agentic-tooling/agentsync/internal/importer"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

// runImport captures project-only agent files into the repo.
//
// The order of operations is the whole safety story, and it is fixed: scan,
// then the home guard, then plan, then — only with --apply — write. The guard
// runs before anything touches disk because a wrong $HOME does not fail, it
// succeeds into the wrong place: every project keys under _outside-home/, so
// import writes a second parallel backup tree instead of updating the real one
// and a later correct-$HOME link sees none of it.
func runImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	rootF := registerRoot(fs)
	repoPath := fs.String("repo", "", "canonical agent repo")
	apply := fs.Bool("apply", false, "write the captures; without it this is a dry run")
	var update updateFlag
	fs.Var(&update, "update",
		"replace a captured copy whose content differs; --update=<project>/<name> for one, repeatable")
	includeTracked := fs.Bool("include-tracked", false,
		"also capture files committed in their own project's git repo")
	failOnPending := fs.Bool("fail-on-pending", false,
		"exit 1 if anything is left uncaptured (for CI)")
	asJSON := fs.Bool("json", false, "emit JSON")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; permit the write anyway")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr.
		return exitFailure
	}

	if update.badSelector != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", update.badSelector)
		return exitFailure
	}

	// import has never taken a positional argument, and silently ignoring one
	// became dangerous when --update grew a value. `--update Web/x/y`, written
	// with a space, parses as the bare flag plus a stray argument — so the
	// user asking to replace one capture would instead authorise replacing
	// every differing capture in the run, which is the opposite of what they
	// typed. Refusing here turns that into a message naming the fix.
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "agentsync: unexpected argument %q; import takes flags only.%s\n",
			rest[0], updateHint(rest[0]))
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

	// The guard, on the directories the scan actually found and before a
	// single byte is written. A HomeError here means nothing is written at
	// all, not even the captures that would have been correct: if the keys
	// are wrong then every destination is wrong.
	if err := repo.RequireHome(root.Path, res.Dirs, *allowOutsideHome); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	run := importRun{
		repoRoot:      repo.Root,
		root:          root.Path,
		res:           res,
		apply:         *apply,
		failOnPending: *failOnPending,
		opts: importer.Options{
			IncludeTracked: *includeTracked,
			Update:         update.set,
		},
	}
	run.plan = importer.NewPlan(repo, res.Entries, run.opts)

	if run.apply {
		run.written, run.errs = run.plan.Apply()
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

	return run.exitCode()
}

// updateFlag is --update in both its forms.
//
// Bare --update authorises replacing every differing capture in the run;
// --update=<project>/<name> authorises exactly one and may be repeated. Both
// live on one flag name because the second is a narrowing of the first rather
// than a separate option, and a user who has just read "pass --update to
// replace the captured copy" should not have to learn a second flag name to do
// it to one file.
//
// The flag package offers exactly one way to carry both: IsBoolFlag, which
// makes `--update` legal alone and `--update=value` carry a value. What it
// cannot do is `--update value` with a space — that parses as the bare flag
// plus a positional argument, which would be the blanket authorisation the user
// was trying to avoid. runImport refuses positional arguments for that reason;
// it is the same trade the flag package forces on every stdlib bool flag, made
// loud instead of silent.
//
// Neither form is deprecated. Bare --update stays the blanket authorisation it
// has always been, because a run that has already been reviewed as a whole is a
// real workflow; the selector form is documented as the one to reach for after
// a diff, which is how the decision is actually made.
type updateFlag struct {
	set importer.UpdateSet

	// badSelector is the first selector that would not parse, held rather
	// than returned. See Set.
	badSelector error
}

// String is what the flag package prints as the default. The zero value
// authorises nothing, and "false" is how a bool flag spells that — which is
// what this flag looks like when it is not given a value.
func (u *updateFlag) String() string {
	if u == nil || !u.set.Enabled() {
		return "false"
	}
	if u.set.Blanket() {
		return "true"
	}
	return strings.Join(u.set.Keys(), ",")
}

// Set takes "true"/"false" from the bare form and a selector from --update=x.
//
// "true" and "false" are the flag package's own spelling for a bool flag given
// no explicit value, so they can never be selectors: neither has a slash, and
// Add would reject them anyway.
//
// A bad selector is recorded and reported by runImport instead of being
// returned. The flag package wraps any error from a bool flag's Set as
// `invalid boolean value "Web/x" for -update: ...`, which is both misleading —
// nothing here was meant to be a boolean — and followed by the whole usage
// block, burying the one line that says what to type. Holding the error lets
// the caller print it plainly. Parsing continues, but nothing is written: the
// caller refuses the run before it plans anything.
func (u *updateFlag) Set(v string) error {
	switch v {
	case "true":
		u.set.SetBlanket()
		return nil
	case "false":
		// --update=false means "no updates", and it has to be able to
		// cancel an --update inherited from somewhere else — a CI wrapper
		// appending it to a command line it did not write is the case that
		// matters. So it resets the whole set, blanket bit and selectors
		// alike, on the last-wins reading a bool flag already has.
		//
		// The alternative, recording nothing, leaves blanket authorisation
		// standing after the user asked for none, which is the one
		// direction this flag must never fail in. Resetting can only ever
		// narrow what is authorised, and anything it narrows away still
		// shows up as a conflict and exits 1 rather than passing silently.
		u.set = importer.UpdateSet{}
		return nil
	}
	if err := u.set.Add(v); err != nil && u.badSelector == nil {
		u.badSelector = err
	}
	return nil
}

// IsBoolFlag is what makes bare --update parse. See the type comment for the
// cost it carries and how runImport covers it.
func (u *updateFlag) IsBoolFlag() bool { return true }

// updateHint suggests the attached-value form, but only for an argument that
// could plausibly be a selector.
//
// Echoing the argument back unconditionally produces nonsense for the cases
// that are not near-misses: `-- --update` would advise `--update=--update`, and
// an empty argument would advise `--update=`, which is the one spelling that
// errors. A hint that has to be ignored teaches the reader to ignore hints.
func updateHint(arg string) string {
	if strings.HasPrefix(arg, "-") || !strings.Contains(arg, "/") {
		return ""
	}
	return fmt.Sprintf(" To select one capture, attach the value to the flag: --update=%s", arg)
}

// importRun is one invocation: what was planned, what was written, and what
// went wrong. It exists so the report and the exit code are computed from the
// same values rather than from a dozen parameters threaded through both.
type importRun struct {
	repoRoot      string
	root          string
	res           scan.Result
	opts          importer.Options
	apply         bool
	failOnPending bool
	plan          importer.Plan
	written       []importer.Item
	errs          []error
}

// exitCode maps the run onto the CLI contract.
//
// A write failure and an unjudgeable destination both mean the tool could not
// do its job (2). A conflict means it did its job and found something only a
// human can resolve (1).
//
// The scan's own problems — paths out in the projects that could not be
// classified — are a conflict, not a clean run. scan and import differ here on
// purpose, because their exit codes assert different things. scan's 0 asserts
// nothing; it is a report, and a caller who wants an assertion out of it passes
// --fail-on-risk. import's 0 IS the assertion, that everything unprotected is
// now captured, and it is the precondition link gets gated on. An unreadable
// subtree may hide a state-6/7 file behind it, so that assertion cannot be
// made, and an unreadable subtree is exactly the kind of thing only a human can
// resolve. Deferring to "link will notice" is circular: link's own scan meets
// the same subtree and faces the same decision with no more information.
func (r importRun) exitCode() int {
	if len(r.errs) > 0 || len(r.plan.Problems) > 0 {
		return exitFailure
	}
	if len(r.plan.Conflicts()) > 0 || len(r.res.Problems) > 0 {
		return exitConflict
	}
	if r.failOnPending && r.pending() > 0 {
		return exitAtRisk
	}
	return exitOK
}

// pending counts the writes this run planned and did not perform.
//
// A dry run has not performed any of them, which is the whole reason
// --fail-on-pending exists: without it a dry run over a tree full of
// uncaptured overrides exits 0, so there was no way to assert "everything is
// captured" from CI without also authorising the writes. With --apply the count
// is the failures, and those already exit 2 by the branch above — so the flag
// changes nothing about a run that wrote everything it planned to, which is
// what makes it safe to leave on in a pipeline that does both.
//
// Conflicts are deliberately not counted here. They are not pending writes —
// a conflict is a refusal — and they already exit 1 on their own.
func (r importRun) pending() int {
	return len(r.plan.Writes()) - len(r.written)
}

// unmatchedUpdates are the --update selectors that named nothing in this run.
func (r importRun) unmatchedUpdates() []string {
	return r.opts.Update.Unmatched(r.plan.Items)
}

// incomplete reports whether the scan failed to look at part of the tree.
//
// It is what disqualifies the summary from claiming a complete capture: an
// unclassifiable path may be an unreadable directory with a state-6/7 file
// behind it, and no amount of successful writing elsewhere says otherwise.
func (r importRun) incomplete() bool {
	return len(r.res.Problems) > 0
}

// captureCaveat qualifies a summary sentence that would otherwise read as an
// assertion that the repo now holds everything.
func (r importRun) captureCaveat() string {
	if !r.incomplete() {
		return ""
	}
	return ", but the scan could not read every path, so this is NOT proof that everything is captured"
}

// importReport is the --json form. It reports the whole plan, not just the
// writes: what was skipped and why is the part a user cannot reconstruct.
type importReport struct {
	Apply bool `json:"apply"`

	// Update says only that --update was given in some form. It is not the
	// scope of what it authorised: UpdateBlanket is.
	Update bool `json:"update"`

	// UpdateBlanket is true when bare --update was given, which authorises
	// replacing every differing capture in the run regardless of what
	// UpdateItems holds.
	//
	// It is a separate field rather than "UpdateItems is empty" because the
	// two forms combine: `--update --update=nope/typo` reports one selector
	// and authorises everything, so an auditor reading UpdateItems alone
	// would understate the scope of a safety flag — the direction a report
	// about a destructive option must never be wrong in.
	UpdateBlanket bool `json:"update_blanket"`

	// UpdateItems are the explicit --update=<project>/<name> selectors, and
	// UpdateUnmatched the ones that named nothing in this run. The second is
	// reported and does not move the exit code — see UpdateSet.Unmatched.
	UpdateItems     []string `json:"update_items"`
	UpdateUnmatched []string `json:"update_unmatched"`

	Tracked  bool               `json:"include_tracked"`
	Repo     string             `json:"repo"`
	Root     string             `json:"root"`
	Items    []importer.Item    `json:"items"`
	Counts   map[string]int     `json:"counts"`
	Written  []string           `json:"written"`
	Errors   []string           `json:"errors"`
	Problems []scan.ScanProblem `json:"problems"`

	// ScanProblems are paths in the scanned projects that could not be
	// classified. They are not import failures, but they mean this run is
	// not proof that everything is captured.
	ScanProblems []scan.ScanProblem `json:"scan_problems"`
}

func (r importRun) emitJSON(w io.Writer) error {
	rep := importReport{
		Apply:         r.apply,
		Update:        r.opts.Update.Enabled(),
		UpdateBlanket: r.opts.Update.Blanket(),
		// Non-nil so the JSON carries empty arrays rather than nulls, the
		// same rule the plan follows for Items and Problems.
		UpdateItems:     append([]string{}, r.opts.Update.Keys()...),
		UpdateUnmatched: append([]string{}, r.unmatchedUpdates()...),
		Tracked:         r.opts.IncludeTracked,
		Repo:            r.repoRoot,
		Root:            r.root,
		Items:           r.plan.Items,
		Counts:          make(map[string]int),
		Written:         make([]string, 0, len(r.written)),
		Errors:          make([]string, 0, len(r.errs)),
		Problems:        r.plan.Problems,
		ScanProblems:    r.res.Problems,
	}
	for action, n := range r.plan.Counts() {
		rep.Counts[action.String()] = n
	}
	for _, item := range r.written {
		rep.Written = append(rep.Written, item.Dest)
	}
	for _, err := range r.errs {
		rep.Errors = append(rep.Errors, err.Error())
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// actionOrder controls report grouping: what blocks the run first, what it
// would do next, what it left alone last.
var actionOrder = []importer.Action{
	importer.ActionConflict,
	importer.ActionImport,
	importer.ActionUpdate,
	importer.ActionSkipTracked,
	importer.ActionCaptured,
}

var actionMarker = map[importer.Action]string{
	importer.ActionConflict:    "!!",
	importer.ActionImport:      " +",
	importer.ActionUpdate:      " ~",
	importer.ActionSkipTracked: "  ",
	importer.ActionCaptured:    " =",
}

var actionNote = map[importer.Action]string{
	importer.ActionConflict:    "cannot be captured safely — NOT written",
	importer.ActionImport:      "exists only in the project; capture it",
	importer.ActionUpdate:      "captured copy is stale; replace it",
	importer.ActionSkipTracked: "no repo copy, but committed in its own repo",
	importer.ActionCaptured:    "already captured, identical content",
}

func (r importRun) emitText(w io.Writer) {
	fmt.Fprintf(w, "Scanned %s against %s\n", r.root, r.repoRoot)
	// The third number is every state-6/7 entry considered, not the number
	// that will be written: skipped and conflicting files are part of the
	// answer to "what does the repo not hold", and hiding them here would
	// make the groups below look like they came from nowhere.
	fmt.Fprintf(w, "%d agent director%s, %d file%s, %d without a copy in the repo\n\n",
		len(r.res.Dirs), plural(len(r.res.Dirs), "y", "ies"),
		len(r.res.Entries), plural(len(r.res.Entries), "", "s"),
		len(r.plan.Items))

	if len(r.plan.Items) == 0 {
		// Qualified when the scan hit something it could not read: "every
		// project agent" is a claim about the whole tree, and part of the
		// tree was never looked at.
		if r.incomplete() {
			fmt.Fprintln(w, "Nothing to import: every project agent the scan could classify is already captured in the repo.")
		} else {
			fmt.Fprintln(w, "Nothing to import: every project agent is already captured in the repo.")
		}
		fmt.Fprintln(w)
	} else {
		emitImportGroups(w, r.plan.Items)
	}

	emitImportProblems(w, r.plan.Problems, r.res.Problems)

	fmt.Fprintln(w, strings.Repeat("-", 60))
	r.emitSummary(w)
}

func (r importRun) emitSummary(w io.Writer) {
	counts := r.plan.Counts()
	pending := len(r.plan.Writes())

	switch {
	case !r.apply && pending > 0:
		fmt.Fprintf(w, "DRY RUN — nothing was written. Pass --apply to capture %d file%s.\n",
			pending, plural(pending, "", "s"))
	case !r.apply:
		fmt.Fprintln(w, "DRY RUN — nothing was written, and there is nothing to write.")
	case pending == 0:
		fmt.Fprintf(w, "Nothing was written: there was nothing to capture%s.\n", r.captureCaveat())
	default:
		// "n of m" rather than "n": a run where some writes failed must
		// not read like a complete one in its headline sentence. The
		// caveat is there for the same reason one step further out —
		// every planned write succeeding still says nothing about a
		// subtree the scan never got into.
		fmt.Fprintf(w, "Imported %d of %d file%s into %s%s.\n",
			len(r.written), pending, plural(pending, "", "s"),
			filepath.Join(r.repoRoot, "project-agents"), r.captureCaveat())
	}

	if n := counts[importer.ActionConflict]; n > 0 {
		fmt.Fprintf(w, "%d conflict%s NOT written. Compare each with its capture, then pass --update=<project>/<name> to replace that one (bare --update replaces every differing capture in the run).\n",
			n, plural(n, " was", "s were"))
	}
	r.emitUpdateRefusals(w)
	if n := counts[importer.ActionSkipTracked]; n > 0 {
		fmt.Fprintf(w, "%d file%s skipped: committed in %s own repo (--include-tracked captures %s anyway).\n",
			n, plural(n, "", "s"), plural(n, "its", "their"), plural(n, "it", "them"))
	}
	if n := len(r.plan.Problems); n > 0 {
		fmt.Fprintf(w, "%d destination%s could not be judged and %s left untouched.\n",
			n, plural(n, "", "s"), plural(n, "was", "were"))
	}
	if n := len(r.errs); n > 0 {
		fmt.Fprintf(w, "%d write%s FAILED (see stderr); this run did not capture everything it planned to.\n",
			n, plural(n, "", "s"))
	}
	if n := len(r.res.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s in the scanned tree could not be classified and may hide uncaptured files. Resolve %s and re-run: until then this repo cannot be said to hold everything, which is what link depends on.\n",
			n, plural(n, "", "s"), plural(n, "it", "them"))
	}
	if r.failOnPending && r.pending() > 0 {
		fmt.Fprintf(w, "--fail-on-pending: %d file%s still uncaptured, so this run exits %d.\n",
			r.pending(), plural(r.pending(), " is", "s are"), exitAtRisk)
	}
}

// emitUpdateRefusals explains the two ways an authorised --update was turned
// down anyway, and what to do about each.
//
// They are separated because only one of them has a fix the user can carry
// out. "The capture is not committed" is resolved by committing it; "git could
// not be asked" is resolved by finding out why git will not answer, which is a
// different afternoon. Folding both into one sentence would send someone to
// commit files in a repository git is already refusing to talk about.
//
// The unmatched-selector line sits here too, because it is the third way a
// --update the user typed produced no write. It does not change the exit code:
// see UpdateSet.Unmatched.
func (r importRun) emitUpdateRefusals(w io.Writer) {
	if n := len(r.plan.Refused(importer.RefusalUncommitted)); n > 0 {
		fmt.Fprintf(w, "%d capture%s NOT replaced because the bytes %s hold%s right now are in no commit in this repo — untracked, or edited since %s last committed — so replacing %s would leave that content nowhere at all. Commit %s (git add project-agents && git commit) in %s, then re-run.\n",
			n, plural(n, "", "s"), plural(n, "it", "they"), plural(n, "s", ""),
			plural(n, "it was", "they were"), plural(n, "it", "them"),
			plural(n, "it", "them"), r.repoRoot)
	}
	if n := len(r.plan.Refused(importer.RefusalCommitUnknown)); n > 0 {
		fmt.Fprintf(w, "%d capture%s NOT replaced because git could not say whether the bytes %s hold%s are committed. --update is refused rather than guessed at; fix the git error and re-run.\n",
			n, plural(n, " was", "s were"), plural(n, "it", "they"), plural(n, "s", ""))
	}
	if unmatched := r.unmatchedUpdates(); len(unmatched) > 0 {
		fmt.Fprintf(w, "%d --update selector%s matched nothing in this run (%s). Check the project key and agent name against the columns above.\n",
			len(unmatched), plural(len(unmatched), "", "s"), strings.Join(unmatched, ", "))
	}
}

func emitImportGroups(w io.Writer, items []importer.Item) {
	width := importProjectColumn(items)

	byAction := make(map[importer.Action][]importer.Item)
	for _, i := range items {
		byAction[i.Action] = append(byAction[i.Action], i)
	}

	for _, action := range actionOrder {
		group := byAction[action]
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s %s (%d) — %s\n",
			actionMarker[action], action, len(group), actionNote[action])
		for _, item := range group {
			// Trimmed rather than formatted exactly: an item with no
			// reason would otherwise leave trailing whitespace in a
			// report people paste into issues and diffs.
			line := fmt.Sprintf("     %-*s %-28s %s",
				width, item.Entry.Project, item.Entry.Name, item.Reason)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
		fmt.Fprintln(w)
	}
}

// importProjectColumn sizes the project column to the widest key printed, the
// same way the scan report does: project keys are paths, so a fixed width goes
// ragged the moment a nested project appears.
func importProjectColumn(items []importer.Item) int {
	const minWidth = 24
	width := minWidth
	for _, i := range items {
		if n := utf8.RuneCountInString(i.Entry.Project); n > width {
			width = n
		}
	}
	return width
}

// emitImportProblems prints both problem kinds under one header, labelled by
// which side they came from. They mean different things — a repo-side problem
// blocked a write, a scan-side one means something may not have been seen at
// all — and a reader has to be able to tell which they are looking at.
func emitImportProblems(w io.Writer, dest, scanned []scan.ScanProblem) {
	if len(dest) == 0 && len(scanned) == 0 {
		return
	}
	fmt.Fprintf(w, "?? problems (%d)\n", len(dest)+len(scanned))
	for _, p := range sortedProblems(dest) {
		fmt.Fprintf(w, "     destination: %s\n", p)
	}
	for _, p := range sortedProblems(scanned) {
		fmt.Fprintf(w, "     unclassified: %s\n", p)
	}
	fmt.Fprintln(w)
}

func sortedProblems(problems []scan.ScanProblem) []scan.ScanProblem {
	sorted := make([]scan.ScanProblem, len(problems))
	copy(sorted, problems)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return sorted
}
