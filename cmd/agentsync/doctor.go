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
	"github.com/agentic-tooling/agentsync/internal/scan"
)

// runDoctor reports the ways this repo and the projects it serves have drifted
// apart, and writes nothing.
//
// The order of operations is shorter than import's and link's because there is
// no write to guard: scan, diagnose, report. The home check is a warning rather
// than a refusal for the same reason it is in scan — a read-only command with a
// wrong $HOME over-reports (captures look orphaned because their keys resolve
// under the wrong home), which fails safe and is loudly visible, while
// RequireHome exists to stop a write landing in the wrong namespace. There is no
// write here to stop.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	rootF := registerRoot(fs)
	repoPath := fs.String("repo", "", "canonical agent repo")
	asJSON := fs.Bool("json", false, "emit JSON")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; suppress the home warning")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr.
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

	run := doctorRun{
		repoRoot: repo.Root,
		root:     root.Path,
		home:     repo.Home,
		res:      res,
		check:    repo.CheckHome(root.Path, res.Dirs),
		report:   doctor.Diagnose(repo, res),
	}
	findings, problems := overrideFindings(repo.Home, res.Dirs)
	run.report.Findings = append(run.report.Findings, findings...)
	run.report.Problems = append(run.report.Problems, problems...)

	// Both home diagnostics, after the diagnosis and before the report, so a
	// terminal user reads them before the numbers they apply to.
	//
	// The diagnosis has to come first now, which is a change from scan: the
	// second warning is computed from the findings themselves. The two are
	// complementary and neither subsumes the other — warnHome judges the
	// projects the scan walked, HomeWarning judges the capture keys nothing
	// walked, and the reproduction that made this necessary is precisely the
	// run where the first has nothing to say and the second has everything.
	warnHome(stderr, repo, root.Path, res.Dirs, *allowOutsideHome)
	if warning := run.report.HomeWarning(repo.Home); warning != "" {
		fmt.Fprintf(stderr, "agentsync: warning: %s\n", warning)
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

	return run.exitCode()
}

// doctorRun is one invocation. It exists so the report and the exit code are
// computed from the same values rather than from parameters threaded through
// both.
type doctorRun struct {
	repoRoot string
	root     string
	home     string
	res      scan.Result
	check    scan.HomeCheck
	report   doctor.Report
}

// exitCode maps the run onto the CLI contract.
//
// A path doctor could not judge means the tool could not do its job (2), the
// same verdict link and import give their own unjudgeable paths. A broken link,
// a mismatch, a stale capture, and an orphaned capture directory all mean it did
// its job and found something only a human can settle (1).
//
// The scan's own problems are a 1 for the reason they are in import and link: an
// unread path may hide the very drift being asked about, and an incomplete
// diagnosis must never exit the same way a clean one does. doctor's 0 is an
// assertion — "this repo and these projects line up" — and an assertion cannot
// be made about a tree that was not fully read.
//
// A foreign link never affects the code, in either direction. It is reported and
// it is not a fault.
func (r doctorRun) exitCode() int {
	if len(r.report.Problems) > 0 {
		return exitFailure
	}
	if len(r.report.NeedsHuman()) > 0 || len(r.res.Problems) > 0 {
		return exitAtRisk
	}
	return exitOK
}

// incomplete reports whether the scan failed to look at part of the tree, which
// disqualifies the summary from claiming everything lines up.
func (r doctorRun) incomplete() bool {
	return len(r.res.Problems) > 0 || len(r.report.Problems) > 0
}

// doctorHome is the home-directory signal in machine-readable form.
//
// It is in the JSON because both warnings go to stderr, which a machine
// consumer routinely discards — leaving fifteen orphaned capture directories
// indistinguishable from fifteen genuinely deleted projects. A consumer acting
// on orphaned-captures must be able to see, in the same document, that the run
// doubts its own premise.
type doctorHome struct {
	// Status is scan.HomeCheck's verdict on the projects the scan walked.
	//
	// It is NOT a summary of this block, and a consumer must not key on it
	// alone: "ok" alongside suspect:true is the normal shape of the very
	// problem this block exists to surface. The two answer different
	// questions from different evidence — status judges scanned project
	// directories, of which there may be none, while suspect judges capture
	// keys nothing walked — so a run with a wrong home and an empty root
	// reports status "ok" and suspect true together. Read both.
	Status string `json:"status"`

	// Path is the resolved home directory used for keying, empty when none
	// could be determined.
	Path string `json:"path"`

	// Suspect is doctor's own judgement, from the capture keys the scan
	// never walked: every capture directory names a project that is not
	// there. Warning carries the sentence explaining it, empty when false.
	Suspect bool   `json:"suspect"`
	Warning string `json:"warning,omitempty"`
}

// doctorReport is the --json form.
type doctorReport struct {
	Repo     string             `json:"repo"`
	Root     string             `json:"root"`
	Home     doctorHome         `json:"home"`
	Findings []doctor.Finding   `json:"findings"`
	Counts   map[string]int     `json:"counts"`
	Examined doctor.Examined    `json:"examined"`
	Problems []scan.ScanProblem `json:"problems"`

	// ScanProblems are paths in the scanned projects that could not be
	// classified. They are not doctor failures, but they mean this run is not
	// proof that nothing has drifted.
	ScanProblems []scan.ScanProblem `json:"scan_problems"`
}

func (r doctorRun) emitJSON(w io.Writer) error {
	rep := doctorReport{
		Repo: r.repoRoot,
		Root: r.root,
		Home: doctorHome{
			Status:  r.check.Status.String(),
			Path:    r.check.Home,
			Suspect: r.report.HomeSuspect(),
			Warning: r.report.HomeWarning(r.home),
		},
		Findings:     r.report.Findings,
		Counts:       make(map[string]int),
		Examined:     r.report.Examined,
		Problems:     r.report.Problems,
		ScanProblems: r.res.Problems,
	}
	for kind, n := range r.report.Counts() {
		rep.Counts[kind.String()] = n
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// doctorKindOrder controls report grouping: what needs a human first, what is
// merely informational last.
var doctorKindOrder = []doctor.Kind{
	doctor.KindBrokenLink,
	doctor.KindMismatch,
	doctor.KindStaleCapture,
	doctor.KindOrphanedCaptures,
	doctor.KindForeignLink,
	doctor.KindOverrideCannotRun,
}

var doctorKindMarker = map[doctor.Kind]string{
	doctor.KindBrokenLink:        "!!",
	doctor.KindMismatch:          "!!",
	doctor.KindStaleCapture:      "!!",
	doctor.KindOrphanedCaptures:  "!!",
	doctor.KindForeignLink:       "  ",
	doctor.KindOverrideCannotRun: "!!",
}

var doctorKindNote = map[doctor.Kind]string{
	doctor.KindBrokenLink:        "link into this repo whose target is gone — NOT unlinked",
	doctor.KindMismatch:          "linked to the wrong file in this repo — NOT repointed",
	doctor.KindStaleCapture:      "captured here, but no live file in the project — NOT deleted",
	doctor.KindOrphanedCaptures:  "captured here, but the whole project is gone — NOT deleted",
	doctor.KindForeignLink:       "points outside this repo — not ours, and not a fault",
	doctor.KindOverrideCannotRun: "override hook that cannot run — the overrides do nothing",
}

func (r doctorRun) emitText(w io.Writer) {
	ex := r.report.Examined
	fmt.Fprintf(w, "Diagnosed %s against %s\n", r.root, r.repoRoot)
	fmt.Fprintf(w, "%d agent director%s, %d project agent%s, %d capture%s in %d capture director%s\n\n",
		ex.Projects, plural(ex.Projects, "y", "ies"),
		ex.Entries, plural(ex.Entries, "", "s"),
		ex.Captures, plural(ex.Captures, "", "s"),
		ex.CaptureDirs, plural(ex.CaptureDirs, "y", "ies"))

	if len(r.report.Findings) == 0 {
		fmt.Fprintln(w, "Nothing to report: every link resolves to its counterpart and every capture has a live file.")
		fmt.Fprintln(w)
	} else {
		emitFindingGroups(w, r.report)
	}

	emitDoctorProblems(w, r.report.Problems, r.res.Problems)

	fmt.Fprintln(w, strings.Repeat("-", 60))
	r.emitSummary(w)
}

func (r doctorRun) emitSummary(w io.Writer) {
	counts := r.report.Counts()

	// The headline is about what needs a human, never about the raw finding
	// count: a report whose only finding is a foreign link is a clean report,
	// and must read as one.
	// "Nothing has drifted", never "nothing needs attention". doctor reports
	// drift and only drift, so its exit 0 does not imply link's: link also
	// refuses on the import-before-link invariant, on an agents directory its
	// project's own git tracks, and on a --project key that names nothing.
	// None of those is drift, none is doctor's to find, and a summary that
	// read like an all-clear for the whole toolchain would be relied on as one.
	switch n := len(r.report.NeedsHuman()); {
	case n == 0 && !r.incomplete():
		fmt.Fprintln(w, "Nothing has drifted: every link resolves to its counterpart and every capture has a live file.")
		fmt.Fprintln(w, "That is a statement about drift only — whether `link` would run clean is its own question, and `agentsync link` answers it.")
	case n == 0:
		fmt.Fprintln(w, "Nothing has drifted among what could be read — but this run could not check everything (see above).")
	default:
		fmt.Fprintf(w, "%d finding%s need%s a human. doctor is read-only: it changed nothing, and it never will.\n",
			n, plural(n, "", "s"), plural(n, "s", ""))
	}

	if n := counts[doctor.KindBrokenLink]; n > 0 {
		fmt.Fprintf(w, "%d broken link%s left in place. Restore the agent in this repo or remove the link by hand.\n",
			n, plural(n, "", "s"))
	}
	if n := counts[doctor.KindMismatch]; n > 0 {
		fmt.Fprintf(w, "%d link%s point%s at a repo file that is not that project's counterpart, so its captured copy sits unused. Each line above carries the exact command to repoint it.\n",
			n, plural(n, "", "s"), plural(n, "s", ""))
	}
	if n := counts[doctor.KindStaleCapture]; n > 0 {
		fmt.Fprintf(w, "%d capture%s no longer %s a live file in %s project. Do NOT reach for rm: a stale capture may be the only surviving copy, which is the entire point of %s/. Decide one at a time.\n",
			n, plural(n, "", "s"), plural(n, "has", "have"), plural(n, "its", "their"), "project-agents")
	}
	if n := counts[doctor.KindOrphanedCaptures]; n > 0 {
		fmt.Fprintf(w, "%d capture director%s name%s a project that is gone. If the project moved, its key moved with it and the directory can be renamed to match; if it was deleted, these are the last copies.\n",
			n, plural(n, "y", "ies"), plural(n, "s", ""))
		// Directly under the line it qualifies. That line's own framing —
		// "these are the last copies" — is what points a reader at rm, so
		// the reason not to trust it yet has to sit beside it rather than
		// only on stderr, which a piped run discards.
		if warning := r.report.HomeWarning(r.home); warning != "" {
			fmt.Fprintf(w, "  BUT: %s\n", warning)
		}
	}
	if n := counts[doctor.KindForeignLink]; n > 0 {
		fmt.Fprintf(w, "%d foreign link%s reported for completeness; %s belong%s to something else and %s not affect this exit code.\n",
			n, plural(n, "", "s"), plural(n, "it", "they"), plural(n, "s", ""), plural(n, "does", "do"))
	}
	if n := counts[doctor.KindOverrideCannotRun]; n > 0 {
		fmt.Fprintf(w, "%d project%s override%s agents through a hook that cannot run here. Put a current agentsync on PATH, or remove the overrides with agentsync override --clear.\n",
			n, plural(n, "", "s"), plural(n, "s", ""))
	}
	if n := len(r.report.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s could not be judged, so this diagnosis is incomplete.\n",
			n, plural(n, "", "s"))
	}
	if n := len(r.res.Problems); n > 0 {
		fmt.Fprintf(w, "%d path%s in the scanned tree could not be classified and may hide more drift. Resolve %s and re-run.\n",
			n, plural(n, "", "s"), plural(n, "it", "them"))
	}
}

func emitFindingGroups(w io.Writer, rep doctor.Report) {
	width := doctorProjectColumn(rep.Findings)

	for _, kind := range doctorKindOrder {
		group := rep.OfKind(kind)
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s %s (%d) — %s\n",
			doctorKindMarker[kind], kind, len(group), doctorKindNote[kind])
		for _, f := range group {
			// Trimmed rather than formatted exactly: an orphaned capture
			// directory has no agent name, and the blank column would
			// otherwise leave trailing whitespace in a report people paste
			// into issues.
			line := fmt.Sprintf("     %-*s %-28s %s", width, f.Project, f.Name, f.Detail)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
		fmt.Fprintln(w)
	}
}

// doctorProjectColumn sizes the project column to the widest key printed, the
// same way every other report here does: project keys are paths, so a fixed
// width goes ragged the moment a nested project appears.
func doctorProjectColumn(findings []doctor.Finding) int {
	width := minColumn
	for _, f := range findings {
		if n := utf8.RuneCountInString(f.Project); n > width {
			width = n
		}
	}
	return width
}

// emitDoctorProblems prints both problem kinds under one header, labelled by
// which side they came from. They mean different things — a repo-side problem is
// a capture doctor could not judge, a scan-side one is a project path that was
// never classified — and a reader has to be able to tell which they are looking
// at.
func emitDoctorProblems(w io.Writer, repoSide, scanned []scan.ScanProblem) {
	if len(repoSide) == 0 && len(scanned) == 0 {
		return
	}
	fmt.Fprintf(w, "?? problems (%d)\n", len(repoSide)+len(scanned))
	for _, p := range sortedProblems(repoSide) {
		fmt.Fprintf(w, "     unjudged: %s\n", p)
	}
	for _, p := range sortedProblems(scanned) {
		fmt.Fprintf(w, "     unclassified: %s\n", p)
	}
	fmt.Fprintln(w)
}
