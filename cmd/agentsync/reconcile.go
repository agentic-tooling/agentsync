package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/agentic-tooling/agentsync/internal/doctor"
	"github.com/agentic-tooling/agentsync/internal/reconcile"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// rekeyFlag collects --rekey OLD=NEW, repeatable.
//
// A bad value is held rather than returned, following undeclineFlag: the flag
// package would bury the message under the whole usage block. There is no bare
// form and no inferred form — the pairing of an orphaned key with a live one is
// a heuristic, and reconcile prints its guesses rather than acting on them.
type rekeyFlag struct {
	moves []reconcile.KeyMove
	seen  map[string]bool
	bad   error
}

func (r *rekeyFlag) String() string {
	if r == nil {
		return ""
	}
	out := make([]string, 0, len(r.moves))
	for _, m := range r.moves {
		out = append(out, m.String())
	}
	return strings.Join(out, ",")
}

func (r *rekeyFlag) Set(v string) error {
	m, err := reconcile.ParseKeyMove(v)
	if err != nil {
		if r.bad == nil {
			r.bad = err
		}
		return nil
	}
	if r.seen == nil {
		r.seen = make(map[string]bool)
	}
	if r.seen[m.String()] {
		return nil
	}
	r.seen[m.String()] = true
	r.moves = append(r.moves, m)
	return nil
}

// dropCaptureFlag collects --drop-capture <project>/<name>, repeatable, in the
// same pair form import's --update and link's --undecline take.
//
// Deliberately no bare form: the equivalent would be "delete every stale
// capture", and a capture may be the last copy of something. state.ParseKey
// refuses a value with no interior slash, which is what turns
// `--drop-capture --apply` into a message rather than a swallowed flag.
type dropCaptureFlag struct {
	keys []state.Key
	seen map[string]bool
	bad  error
}

func (d *dropCaptureFlag) String() string {
	if d == nil {
		return ""
	}
	return strings.Join(keyStrings(d.keys), ",")
}

func (d *dropCaptureFlag) Set(v string) error {
	k, err := state.ParseKey(v)
	if err != nil {
		if d.bad == nil {
			d.bad = err
		}
		return nil
	}
	if d.seen == nil {
		d.seen = make(map[string]bool)
	}
	if d.seen[k.String()] {
		return nil
	}
	d.seen[k.String()] = true
	d.keys = append(d.keys, k)
	return nil
}

// runReconcile acts on the drift doctor diagnoses, gated on what the state
// document says this tool created.
//
// The order of operations carries the safety story, as in import and link:
// scan, diagnose, load the record, plan, home guard, then — only with --apply —
// write. The diagnosis comes before the plan because reconcile may only act on
// something doctor also reports.
func runReconcile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	all := registerAll(fs)
	apply := fs.Bool("apply", false, "make the changes; without it this is a dry run")
	asJSON := fs.Bool("json", false, "emit JSON")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; permit the write anyway")

	adopt := fs.Bool(reconcile.AdoptFlag, false,
		"record currently-healthy links this tool has no record of, with origin adopted")
	prune := fs.Bool(reconcile.PruneFlag, false,
		"remove recorded links that are broken and still point where the record says")
	pruneAdopted := fs.Bool(reconcile.PruneAdoptedFlag, false,
		"widen --"+reconcile.PruneFlag+" to records made by an adopt pass (requires --"+reconcile.PruneFlag+")")

	var rekeys rekeyFlag
	fs.Var(&rekeys, reconcile.RekeyFlag,
		"move project-agents/OLD/ to project-agents/NEW/ and repoint the links, as --"+reconcile.RekeyFlag+"=OLD=NEW (repeatable)")

	var drops dropCaptureFlag
	fs.Var(&drops, reconcile.DropCaptureFlag,
		"delete one capture, named as the <project>/<name> pair the report prints (repeatable; refused unless its bytes are in this repo's HEAD)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr.
		return exitFailure
	}

	if rekeys.bad != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", rekeys.bad)
		return exitFailure
	}
	if drops.bad != nil {
		fmt.Fprintf(stderr, "agentsync: --%s: %v\n", reconcile.DropCaptureFlag, drops.bad)
		return exitFailure
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "agentsync: %s\n", strayArg("reconcile", rest[0], dropCaptureHint))
		return exitFailure
	}

	opts := reconcile.Options{
		Adopt:        *adopt,
		Prune:        *prune,
		PruneAdopted: *pruneAdopted,
		Rekeys:       rekeys.moves,
		Drops:        drops.keys,
	}
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	repo, repoFrom, err := openRepo()
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// Before the scan: a state file that cannot be read must stop the run
	// rather than become a plan built on "this tool created nothing".
	st, err := state.Load(repo)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	root, err := resolveRoot(repo.Home, *all)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	res, err := repo.Scan(root.Path)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	diag := doctor.Diagnose(repo, res)
	plan, err := reconcile.NewPlan(repo, res, diag, st, opts)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// The home guard, before a byte is written. A run that will not write only
	// warns, which is what makes the dry run a preview of the refusal rather
	// than a silent version of it: warnHome prints the sentence RequireHome
	// would fail with.
	//
	// plan.Dirs is what the scan walked, which is NOT everything the plan is
	// about: judgeRecords works from absolute paths in the state document, and
	// those need not fall under the scan root at all. So the guard covers the
	// passes keyed by $HOME — adopt and re-key — and does not cover a decline or
	// a prune for a project the scan never saw. That is the right split anyway:
	// a record's path was resolved when it was written and does not depend on
	// today's home, so a wrong home cannot misdirect it.
	if *apply {
		if err := repo.RequireHome(root.Path, plan.Dirs, *allowOutsideHome); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		warnHome(stderr, repo, root.Path, plan.Dirs, *allowOutsideHome)
	}
	// doctor's second home check, for the reason doctor prints it: the re-key
	// candidates below are derived from orphaned capture directories, which is
	// exactly the finding a wrong home manufactures by the dozen.
	if warning := diag.HomeWarning(repo.Home); warning != "" {
		fmt.Fprintf(stderr, "agentsync: warning: %s\n", warning)
	}

	run := reconcileRun{
		repoRoot:  repo.Root,
		root:      root.Path,
		home:      repo.Home,
		res:       res,
		diag:      diag,
		check:     repo.CheckHome(root.Path, plan.Dirs),
		opts:      opts,
		apply:     *apply,
		plan:      plan,
		statePath: state.Path(repo),
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
	// Last, and only for the op that takes an agent away. Nothing else here
	// changes what a session would load: a re-keyed link is repointed at the
	// same bytes under a new path, and a declined marker, an adopted record and
	// a dropped capture touch no link at all. A notice that fires when nothing
	// changed teaches its reader to skip the one that mattered.
	emitRestartNotice(reportStream(*asJSON, stdout, stderr), len(run.done.Pruned) > 0)

	return run.exitCode()
}

// dropCaptureHint suggests the attached-value form for a stray argument that
// could plausibly have been a selector. Same rule as import's updateHint.
func dropCaptureHint(arg string) string {
	if strings.HasPrefix(arg, "-") || !strings.Contains(arg, "/") {
		return ""
	}
	return fmt.Sprintf(" To drop one capture, attach the value to the flag: --%s=%s",
		reconcile.DropCaptureFlag, arg)
}

// reconcileRun is one invocation: what was planned, what was done, and what
// went wrong. It exists so the report and the exit code are computed from the
// same values rather than from a dozen parameters threaded through both.
type reconcileRun struct {
	repoRoot string
	root     string
	home     string
	res      scan.Result
	diag     doctor.Report
	check    scan.HomeCheck
	opts     reconcile.Options
	apply    bool
	plan     reconcile.Plan
	done     reconcile.Applied
	errs     []error

	// statePath is the document every gate here reads, named in the report
	// because people go and look at this file by hand.
	statePath string
}

// exitCode maps the run onto the CLI contract.
//
// A write failure and an unjudgeable path both mean the tool could not do its
// job (2). A refused selector, a conflict, a diverged link, a prunable link left
// in place, and a dereferenced one all mean it did its job and found something
// only a human can settle (1).
//
// It deliberately does NOT inherit doctor's verdict on the diagnosis it was
// handed. reconcile's 0 asserts that there is nothing here for reconcile to do
// or settle, which is narrower than "nothing has drifted" — a stale capture is
// real drift that no flag here removes, and charging reconcile's exit code for
// it would make the code un-clearable. The summary says so and points at
// doctor, whose exit code does assert it.
func (r reconcileRun) exitCode() int {
	if len(r.errs) > 0 || len(r.plan.Problems) > 0 {
		return exitFailure
	}
	if len(r.plan.Refused) > 0 || len(r.plan.Unresolved()) > 0 || len(r.res.Problems) > 0 {
		return exitAtRisk
	}
	return exitOK
}

// incomplete reports whether the scan failed to look at part of the tree, which
// disqualifies the summary from claiming anything about what it did not read.
func (r reconcileRun) incomplete() bool {
	return len(r.res.Problems) > 0 || len(r.plan.Problems) > 0
}

// reconcileReport is the --json form. It carries the whole plan, not just the
// writes: what was refused and why is the part a user cannot reconstruct.
type reconcileReport struct {
	Apply        bool     `json:"apply"`
	Repo         string   `json:"repo"`
	Root         string   `json:"root"`
	Adopt        bool     `json:"adopt"`
	Prune        bool     `json:"prune"`
	PruneAdopted bool     `json:"prune_adopted"`
	Rekey        []string `json:"rekey"`
	DropKeys     []string `json:"drop_capture"`

	Items    []reconcile.Item    `json:"items"`
	Counts   map[string]int      `json:"counts"`
	Rekeys   []reconcile.Rekey   `json:"rekeys"`
	Refused  []reconcile.Refused `json:"refused"`
	Examined reconcile.Examined  `json:"examined"`
	Problems []scan.ScanProblem  `json:"problems"`
	Findings []doctor.Finding    `json:"findings"`
	Home     doctorHome          `json:"home"`

	// Pruned, Declined, Adopted, Repointed, and Dropped are what actually
	// happened, empty on a dry run. Ownership is read from Adopted and
	// Declined — the document's own words — never inferred from the plan.
	Pruned    []string            `json:"pruned"`
	Declined  []state.Declined    `json:"declined"`
	Adopted   []state.Record      `json:"adopted"`
	Rekeyed   []reconcile.Rekey   `json:"rekeyed"`
	Repointed []reconcile.Repoint `json:"repointed"`
	Dropped   []string            `json:"dropped"`

	StateFile string   `json:"state_file"`
	Errors    []string `json:"errors"`

	// ScanProblems are paths in the scanned projects that could not be
	// classified. They are not reconcile failures, but they mean this run
	// looked at less than the whole tree.
	ScanProblems []scan.ScanProblem `json:"scan_problems"`
}

func (r reconcileRun) emitJSON(w io.Writer) error {
	rep := reconcileReport{
		Apply:        r.apply,
		Repo:         r.repoRoot,
		Root:         r.root,
		Adopt:        r.opts.Adopt,
		Prune:        r.opts.Prune,
		PruneAdopted: r.opts.PruneAdopted,
		Rekey:        moveStrings(r.opts.Rekeys),
		DropKeys:     keyStrings(r.opts.Drops),

		Items:    r.plan.Items,
		Counts:   make(map[string]int),
		Rekeys:   r.plan.Rekeys,
		Refused:  r.plan.Refused,
		Examined: r.plan.Examined,
		Problems: r.plan.Problems,
		Findings: r.diag.Findings,
		Home: doctorHome{
			Status:  r.check.Status.String(),
			Path:    r.check.Home,
			Suspect: r.diag.HomeSuspect(),
			Warning: r.diag.HomeWarning(r.home),
		},

		Pruned:    pathsOf(r.done.Pruned),
		Declined:  nonNilDeclined(r.done.Declined),
		Adopted:   nonNilRecords(r.done.Adopted),
		Rekeyed:   nonNilRekeys(r.done.Rekeyed),
		Repointed: nonNilRepoints(r.done.Repointed),
		Dropped:   pathsOf(r.done.Dropped),

		StateFile:    r.statePath,
		Errors:       make([]string, 0, len(r.errs)),
		ScanProblems: r.res.Problems,
	}
	for op, n := range r.plan.Counts() {
		rep.Counts[op.String()] = n
	}
	for _, err := range r.errs {
		rep.Errors = append(rep.Errors, err.Error())
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func pathsOf(items []reconcile.Item) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Path)
	}
	return out
}

func moveStrings(moves []reconcile.KeyMove) []string {
	out := make([]string, 0, len(moves))
	for _, m := range moves {
		out = append(out, m.String())
	}
	return out
}

func nonNilDeclined(s []state.Declined) []state.Declined {
	if s == nil {
		return []state.Declined{}
	}
	return s
}

func nonNilRekeys(s []reconcile.Rekey) []reconcile.Rekey {
	if s == nil {
		return []reconcile.Rekey{}
	}
	return s
}

func nonNilRepoints(s []reconcile.Repoint) []reconcile.Repoint {
	if s == nil {
		return []reconcile.Repoint{}
	}
	return s
}

// reconcileOpOrder controls report grouping: what needs a human first, what
// this run would do next, what it left alone last.
var reconcileOpOrder = []reconcile.Op{
	reconcile.OpConflict,
	reconcile.OpDiverged,
	reconcile.OpPrunable,
	reconcile.OpDereferenced,
	reconcile.OpPrune,
	reconcile.OpDecline,
	reconcile.OpDropCapture,
	reconcile.OpAdopt,
	reconcile.OpRekeyCandidate,
	reconcile.OpProjectGone,
	reconcile.OpUnrecorded,
	reconcile.OpUnchanged,
}

var reconcileOpMarker = map[reconcile.Op]string{
	reconcile.OpConflict:     "!!",
	reconcile.OpDiverged:     "!!",
	reconcile.OpPrunable:     "!!",
	reconcile.OpDereferenced: "!!",
	reconcile.OpPrune:        " -",
	reconcile.OpDecline:      " ~",
	reconcile.OpDropCapture:  " -",
	reconcile.OpAdopt:        " +",
	// Not "!!": a proposal is not a fault, and doctor already fails on the
	// orphaned capture directory this one is derived from.
	reconcile.OpRekeyCandidate: "  ",
	reconcile.OpProjectGone:    "  ",
	reconcile.OpUnrecorded:     "  ",
	reconcile.OpUnchanged:      " =",
}

var reconcileOpNote = map[reconcile.Op]string{
	reconcile.OpConflict:       "edited after it stopped being a link — NOT resolved here",
	reconcile.OpDiverged:       "the link here is not the one recorded — NOT removed",
	reconcile.OpPrunable:       "recorded, broken, and removable — NOT removed without the flag",
	reconcile.OpDereferenced:   "the link became a real file holding the recorded content",
	reconcile.OpPrune:          "recorded by this tool and broken; remove the link",
	reconcile.OpDecline:        "deleted on purpose; record that so link never recreates it",
	reconcile.OpDropCapture:    "committed in this repo's HEAD; delete the working copy",
	reconcile.OpAdopt:          "healthy and unrecorded; record it as adopted",
	reconcile.OpRekeyCandidate: "a move would explain this — proposed only, never applied",
	reconcile.OpProjectGone:    "the agents directory is gone — the project moved or was deleted; NOT declined",
	reconcile.OpUnrecorded:     "healthy but unowned; nothing may ever remove it until --" + reconcile.AdoptFlag + " records it",
	reconcile.OpUnchanged:      "still the link this tool recorded",
}

func (r reconcileRun) emitText(w io.Writer) {
	ex := r.plan.Examined
	fmt.Fprintf(w, "Reconciled %s against %s\n", r.root, r.repoRoot)
	fmt.Fprintf(w, "%d recorded link%s, %d declined marker%s, %d project agent%s classified\n\n",
		ex.Records, plural(ex.Records, "", "s"),
		ex.Declined, plural(ex.Declined, "", "s"),
		ex.Entries, plural(ex.Entries, "", "s"))

	emitRefused(w, r.plan.Refused)
	emitRekeys(w, r.plan.Rekeys, r.apply)

	if len(r.plan.Items) == 0 {
		fmt.Fprintln(w, "Nothing to reconcile: every recorded link is still the link that was recorded.")
		fmt.Fprintln(w)
	} else {
		emitReconcileGroups(w, r.plan.Items)
	}

	emitDoctorProblems(w, r.plan.Problems, r.res.Problems)

	fmt.Fprintln(w, strings.Repeat("-", 60))
	r.emitSummary(w)
}

func (r reconcileRun) emitSummary(w io.Writer) {
	counts := r.plan.Counts()
	pending := len(r.plan.Writes()) + len(r.plan.Rekeys)

	switch {
	case !r.apply && pending > 0:
		fmt.Fprintf(w, "DRY RUN — nothing was changed. Pass --apply to carry out %d change%s.\n",
			pending, plural(pending, "", "s"))
	case !r.apply:
		fmt.Fprintln(w, "DRY RUN — nothing was changed, and there is nothing to change.")
	case pending == 0:
		fmt.Fprintln(w, "Nothing was changed: there was nothing to do.")
	default:
		done := len(r.done.Pruned) + len(r.done.Declined) + len(r.done.Adopted) +
			len(r.done.Dropped) + len(r.done.Rekeyed)
		// "n of m" rather than "n": a run where some changes failed must not
		// read like a complete one in its headline sentence.
		fmt.Fprintf(w, "Carried out %d of %d change%s.\n", done, pending, plural(pending, "", "s"))
	}

	if n := counts[reconcile.OpConflict]; n > 0 {
		fmt.Fprintf(w, "%d recorded link%s now a real file that has been edited since. reconcile never resolves this: capture it with `agentsync import --apply` so the edit is an override this repo holds, then decide.\n",
			n, plural(n, " is", "s are"))
	}
	if n := counts[reconcile.OpDereferenced]; n > 0 {
		fmt.Fprintf(w, "%d recorded link%s a real file holding exactly the recorded content — nothing was lost, and `agentsync link --apply` will link %s again.\n",
			n, plural(n, " became", "s became"), plural(n, "it", "them"))
	}
	if n := counts[reconcile.OpDiverged]; n > 0 {
		fmt.Fprintf(w, "%d recorded path%s hold a link this tool did not put there. The record does not describe what is on disk, so nothing was removed; look before acting.\n",
			n, plural(n, "", "s"))
	}
	if n := counts[reconcile.OpPrunable]; n > 0 {
		fmt.Fprintf(w, "%d broken link%s recorded and removable but left in place. Pass --%s (and --%s for records an adopt pass made) with --apply.\n",
			n, plural(n, " is", "s are"), reconcile.PruneFlag, reconcile.PruneAdoptedFlag)
	}
	if n := counts[reconcile.OpUnrecorded]; n > 0 {
		fmt.Fprintf(w, "%d healthy link%s no record in %s, so nothing may ever remove %s. --%s records %s as adopted — that pass CANNOT tell a link an older agentsync made from one made by hand, which is why removing an adopted record later needs --%s.\n",
			n, plural(n, " has", "s have"), r.statePath, plural(n, "it", "them"),
			reconcile.AdoptFlag, plural(n, "it", "them"), reconcile.PruneAdoptedFlag)
	}
	if n := len(r.done.Adopted); n > 0 {
		fmt.Fprintf(w, "%d link%s recorded as adopted in %s.\n", n, plural(n, " was", "s were"), r.statePath)
	}
	if n := len(r.done.Declined); n > 0 {
		fmt.Fprintf(w, "%d declined marker%s written in %s; `agentsync link --undecline=<project>/<name> --apply` clears one.\n",
			n, plural(n, " was", "s were"), r.statePath)
	}
	if n := counts[reconcile.OpProjectGone]; n > 0 {
		fmt.Fprintf(w, "%d record%s name an agents directory that is no longer there, so %s describe a project that moved or was deleted. Nothing was declined for %s: an absent directory is evidence about the project, never about one agent in it. If it moved, re-run `agentsync link --apply` at the new path; these records name the old one.\n",
			n, plural(n, "", "s"), plural(n, "it", "they"), plural(n, "it", "them"))
	}
	if n := counts[reconcile.OpRekeyCandidate]; n > 0 {
		fmt.Fprintf(w, "%d re-key%s proposed and none applied: correlating an orphaned key with a live one is a heuristic, and a wrong one files one project's captures under another project's key. Name the move to apply it.\n",
			n, plural(n, " is", "s are"))
	}
	if n := len(r.plan.Refused); n > 0 {
		fmt.Fprintf(w, "%d selector%s refused by a gate (see above); nothing was done for %s.\n",
			n, plural(n, " was", "s were"), plural(n, "it", "them"))
	}
	if n := len(r.diag.NeedsHuman()); n > 0 {
		fmt.Fprintf(w, "doctor's diagnosis of this same tree holds %d finding%s reconcile has no flag for — stale captures above all. They do NOT move this exit code: `agentsync doctor` is where that assertion lives.\n",
			n, plural(n, "", "s"))
	}
	if n := len(r.plan.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s could not be judged and %s left untouched.\n",
			n, plural(n, "", "s"), plural(n, "was", "were"))
	}
	if n := len(r.errs); n > 0 {
		fmt.Fprintf(w, "%d change%s FAILED (see stderr); this run did not do everything it planned to.\n",
			n, plural(n, "", "s"))
	}
	if r.incomplete() {
		fmt.Fprintln(w, "This run could not read every path, so it is NOT proof that everything is accounted for.")
	}
}

// emitRefused prints the selectors a gate turned down, one per line with the
// gate that fired. A count alone would tell a user they were refused without
// telling them what to fix.
func emitRefused(w io.Writer, refused []reconcile.Refused) {
	if len(refused) == 0 {
		return
	}
	fmt.Fprintf(w, "!! refused (%d) — named on the command line, stopped by a gate\n", len(refused))
	for _, ref := range refused {
		fmt.Fprintf(w, "     %s\n", ref)
	}
	fmt.Fprintln(w)
}

// emitRekeys prints the authorised moves and every link that follows one. They
// rewrite links in somebody else's project, so they are reported in their own
// right rather than folded into a summary line.
func emitRekeys(w io.Writer, rekeys []reconcile.Rekey, applied bool) {
	if len(rekeys) == 0 {
		return
	}
	verb := "would be moved"
	if applied {
		verb = "moved"
	}
	fmt.Fprintf(w, " ~ rekey (%d) — captures %s, and the links pointing at them repointed\n", len(rekeys), verb)
	for _, rk := range rekeys {
		fmt.Fprintf(w, "     %s\n", rk)
		for _, rp := range rk.Repoint {
			fmt.Fprintf(w, "       %s\n", rp)
		}
	}
	fmt.Fprintln(w)
}

func emitReconcileGroups(w io.Writer, items []reconcile.Item) {
	width := reconcileProjectColumn(items)

	byOp := make(map[reconcile.Op][]reconcile.Item)
	for _, i := range items {
		byOp[i.Op] = append(byOp[i.Op], i)
	}

	for _, op := range reconcileOpOrder {
		group := byOp[op]
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s %s (%d) — %s\n",
			reconcileOpMarker[op], op, len(group), reconcileOpNote[op])
		for _, item := range group {
			// Trimmed rather than formatted exactly: a capture-directory op
			// has no agent name, and the blank column would otherwise leave
			// trailing whitespace in a report people paste into issues.
			line := fmt.Sprintf("     %-*s %-28s %s", width, item.Project, item.Name, item.Reason)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
		fmt.Fprintln(w)
	}
}

// reconcileProjectColumn sizes the project column to the widest key printed,
// the same way every other report here does.
func reconcileProjectColumn(items []reconcile.Item) int {
	width := minColumn
	for _, i := range items {
		if n := utf8.RuneCountInString(i.Project); n > width {
			width = n
		}
	}
	return width
}
