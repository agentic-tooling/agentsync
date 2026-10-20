package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/reconcile"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// reconcileFixture is linkFixture with the temp dir resolved first.
//
// reconcile compares a link's target against a recorded one as a string, and
// records are keyed by a path whose parent has been resolved — so on macOS,
// where /var is a symlink, an unresolved base makes every assertion here fail
// for a reason that has nothing to do with the code under test.
func reconcileFixture(t *testing.T) (base, repoRoot string) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	repoRoot = filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)
	return base, repoRoot
}

func reconcileArgs(t *testing.T, repoRoot, root string, extra ...string) []string {
	t.Helper()
	inTree(t, repoRoot, root)
	return append([]string{"reconcile", "--" + allFlag}, extra...)
}

// recordInState plants the record link --apply would have written, so a test
// can start from a machine that has already been linked.
func recordInState(t *testing.T, repoRoot, project, name, path, target string) {
	t.Helper()
	repo := mustRepo(t, repoRoot)
	st, err := state.Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hash, err := scan.HashFile(target)
	if err != nil {
		t.Fatalf("hashing %s: %v", target, err)
	}
	if _, err := st.RecordCreated(state.Link{
		Key: state.Key{Project: project, Name: name}, Path: path, Target: target, Hash: hash,
	}, time.Now()); err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if err := st.Save(repo); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func loadState(t *testing.T, repoRoot string) *state.File {
	t.Helper()
	st, err := state.Load(mustRepo(t, repoRoot))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return st
}

// brokenRecordedLink is the fixture the prune path is about: a link this tool
// recorded creating, whose target has since been deleted from the repo.
func brokenRecordedLink(t *testing.T, base, repoRoot, project, name string) (path, target string) {
	t.Helper()
	target = globalAgent(t, repoRoot, name, "body\n")
	path = projectAgentPath(base, project, name)
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	recordInState(t, repoRoot, project, name, path, target)
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}
	return path, target
}

// --- dry run and exit codes -------------------------------------------------

// TestRunReconcile_DryRunIsTheDefault: a command that removes things defaults
// to describing itself. The report must name what it would remove, and the link
// must still be there.
func TestRunReconcile_DryRunIsTheDefault(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "DRY RUN") {
		t.Errorf("report does not say it is a dry run:\n%s", stdout)
	}
	if !strings.Contains(stdout, "prune") {
		t.Errorf("report does not name the removal it would make:\n%s", stdout)
	}
	assertIsLinkPath(t, path, "a dry run removes nothing")
}

// TestRunReconcile_PruneAppliesAndIsIdempotent: the whole prune path end to
// end, including the second run having nothing left to say.
func TestRunReconcile_PruneAppliesAndIsIdempotent(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived the prune (%v)", path, err)
	}
	st := loadState(t, repoRoot)
	if _, ok := st.LookupLink(path); ok {
		t.Error("the record survived the link it described")
	}
	if len(st.AllDeclined()) != 0 {
		t.Errorf("pruning wrote a declined marker: %v", st.AllDeclined())
	}

	code, stdout, _ = runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
	if code != exitOK {
		t.Errorf("second run exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "nothing to do") && !strings.Contains(stdout, "Nothing to reconcile") {
		t.Errorf("the second run is not a no-op:\n%s", stdout)
	}
}

// TestRunReconcile_ExitCodes: the CLI contract, from the shapes that produce
// each code.
func TestRunReconcile_ExitCodes(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, base, repoRoot string) []string
		want  int
	}{
		{
			name: "a clean machine exits 0",
			setup: func(t *testing.T, base, repoRoot string) []string {
				target := globalAgent(t, repoRoot, "a", "body\n")
				path := projectAgentPath(base, "Web/proj", "a")
				mustMkdirT(t, filepath.Dir(path))
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				recordInState(t, repoRoot, "Web/proj", "a", path, target)
				return nil
			},
			want: exitOK,
		},
		{
			name: "a removable link left in place exits 1",
			setup: func(t *testing.T, base, repoRoot string) []string {
				brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")
				return nil
			},
			want: exitAtRisk,
		},
		{
			name: "a file edited after diverging exits 1",
			setup: func(t *testing.T, base, repoRoot string) []string {
				target := globalAgent(t, repoRoot, "a", "body\n")
				path := projectAgentPath(base, "Web/proj", "a")
				recordInState(t, repoRoot, "Web/proj", "a", path, target)
				mustWriteT(t, path, "edited by hand\n")
				return nil
			},
			want: exitAtRisk,
		},
		{
			name: "an unrecorded healthy link alone exits 0",
			setup: func(t *testing.T, base, repoRoot string) []string {
				target := globalAgent(t, repoRoot, "a", "body\n")
				path := projectAgentPath(base, "Web/proj", "a")
				mustMkdirT(t, filepath.Dir(path))
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return nil
			},
			want: exitOK,
		},
		{
			name: "a refused selector exits 1",
			setup: func(t *testing.T, base, repoRoot string) []string {
				return []string{"--" + reconcile.DropCaptureFlag + "=Web/proj/nope"}
			},
			want: exitAtRisk,
		},
		{
			name: "--prune-adopted without --prune exits 2",
			setup: func(t *testing.T, base, repoRoot string) []string {
				return []string{"--" + reconcile.PruneAdoptedFlag}
			},
			want: exitFailure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repoRoot := reconcileFixture(t)
			extra := tc.setup(t, base, repoRoot)
			code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, extra...)...)
			if code != tc.want {
				t.Errorf("exit = %d, want %d\n%s\n%s", code, tc.want, stdout, stderr)
			}
		})
	}
}

// --- the declined round trip ------------------------------------------------

// TestRunReconcile_DeclineIsHonouredByLinkAndUndoneByIt: writing the marker and
// clearing it are the two halves of one decision, so they are tested as one.
func TestRunReconcile_DeclineIsHonouredByLinkAndUndoneByIt(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	const project = "Web/proj"

	target := globalAgent(t, repoRoot, "a", "body\n")
	path := projectAgentPath(base, project, "a")
	mustMkdirT(t, filepath.Dir(path))
	recordInState(t, repoRoot, project, "a", path, target)

	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("reconcile exit = %d\n%s\n%s", code, stdout, stderr)
	}
	if !loadState(t, repoRoot).IsDeclined(state.Key{Project: project, Name: "a"}) {
		t.Fatalf("no declined marker was written:\n%s", stdout)
	}

	// link must now refuse to recreate it, and say so rather than skipping.
	code, stdout, _ = runCLI(t, linkArgs(t, repoRoot, base,
		"--project", project, "--agent", "a", "--apply")...)
	if code != exitOK {
		t.Errorf("link exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "declined") {
		t.Errorf("link did not report the marker:\n%s", stdout)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("link recreated a link somebody deleted on purpose")
	}

	// And --undecline is the override, which is a write like any other.
	code, stdout, _ = runCLI(t, linkArgs(t, repoRoot, base,
		"--project", project, "--agent", "a", "--undecline="+project+"/a", "--apply")...)
	if code != exitOK {
		t.Fatalf("link --undecline exit = %d\n%s", code, stdout)
	}
	assertIsLink(t, path, target)
}

// --- selectors --------------------------------------------------------------

// TestRunReconcile_SelectorsRefuseAValueThatIsNotAPair: the hazard of a
// value-taking flag is that it swallows the next flag. Both selectors refuse a
// value that could not be one, which turns that into a message.
func TestRunReconcile_SelectorsRefuseAValueThatIsNotAPair(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		mentions string
	}{
		{
			name:     "--rekey swallowing the next flag",
			args:     []string{"--" + reconcile.RekeyFlag, "--apply"},
			mentions: "OLD=NEW",
		},
		{
			name:     "--rekey with a key climbing out of project-agents",
			args:     []string{"--" + reconcile.RekeyFlag + "=Web/a=../../etc"},
			mentions: "cannot point outside it",
		},
		{
			name:     "--drop-capture swallowing the next flag",
			args:     []string{"--" + reconcile.DropCaptureFlag, "--apply"},
			mentions: "<project>/<name>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repoRoot := reconcileFixture(t)
			code, _, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, tc.args...)...)
			if code != exitFailure {
				t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stderr)
			}
			if !strings.Contains(stderr, tc.mentions) {
				t.Errorf("stderr does not explain the refusal (%q):\n%s", tc.mentions, stderr)
			}
		})
	}
}

// TestRunReconcile_RefusesAPositionalArgument, and points at the attached-value
// form only when the stray argument could plausibly have been a selector.
func TestRunReconcile_RefusesAPositionalArgument(t *testing.T) {
	cases := []struct {
		name, arg, wantHint string
	}{
		{name: "a stray selector", arg: "Web/proj/a", wantHint: "--" + reconcile.DropCaptureFlag + "=Web/proj/a"},
		{name: "something that is not one", arg: "nonsense"},
		{name: "a flag after the terminator", arg: "-x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repoRoot := reconcileFixture(t)
			code, _, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--", tc.arg)...)
			if code != exitFailure {
				t.Fatalf("exit = %d, want %d\n%s", code, exitFailure, stderr)
			}
			hinted := strings.Contains(stderr, "attach the value to the flag")
			if tc.wantHint != "" {
				if !hinted || !strings.Contains(stderr, tc.wantHint) {
					t.Errorf("stderr does not suggest the attached form:\n%s", stderr)
				}
				return
			}
			if hinted {
				t.Errorf("stderr suggests a selector form for %q, which is not one:\n%s", tc.arg, stderr)
			}
		})
	}
}

// TestRunReconcile_RepeatedSelectorsAreDeduplicated: naming the same thing
// twice must report one outcome, not one done and one unmatched — the second
// would read as a typo.
func TestRunReconcile_RepeatedSelectorsAreDeduplicated(t *testing.T) {
	base, repoRoot := reconcileFixture(t)

	_, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base,
		"--"+reconcile.DropCaptureFlag+"=Web/proj/nope",
		"--"+reconcile.DropCaptureFlag+"=Web/proj/nope",
		"--"+reconcile.RekeyFlag+"=Web/a=Web/b",
		"--"+reconcile.RekeyFlag+"=Web/a=Web/b",
		"--json")...)

	var rep struct {
		Refused  []reconcile.Refused `json:"refused"`
		Rekey    []string            `json:"rekey"`
		DropKeys []string            `json:"drop_capture"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("parsing the JSON report: %v\n%s", err, stdout)
	}
	if len(rep.Rekey) != 1 || len(rep.DropKeys) != 1 {
		t.Errorf("selectors were not deduplicated: rekey=%v drop=%v", rep.Rekey, rep.DropKeys)
	}
	if len(rep.Refused) != 2 {
		t.Errorf("want one refusal per distinct selector, got %v", rep.Refused)
	}
}

// --- re-key through the CLI -------------------------------------------------

// TestRunReconcile_RekeyMovesCapturesAndRepointsLinks: the compound operation
// end to end, dry run first. It is the one command here that rewrites a link in
// somebody else's project, so the report has to show every link it touches.
func TestRunReconcile_RekeyMovesCapturesAndRepointsLinks(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)
	base, repoRoot := reconcileFixture(t)

	oldCapture := filepath.Join(repoRoot, "project-agents", "Web", "old-name", "a.md")
	mustWriteT(t, oldCapture, "captured\n")
	path := projectAgentPath(base, newKey, "a")
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(oldCapture, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	recordInState(t, repoRoot, newKey, "a", path, oldCapture)

	move := "--" + reconcile.RekeyFlag + "=" + oldKey + "=" + newKey
	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, move)...)
	if code != exitOK {
		t.Fatalf("dry run exit = %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "would be moved") || !strings.Contains(stdout, path) {
		t.Errorf("the dry run does not show the link it would rewrite:\n%s", stdout)
	}
	assertIsRegular(t, oldCapture, "a dry run moves nothing")

	code, stdout, stderr = runCLI(t, reconcileArgs(t, repoRoot, base, move, "--apply")...)
	if code != exitOK {
		t.Fatalf("apply exit = %d\n%s\n%s", code, stdout, stderr)
	}
	newCapture := filepath.Join(repoRoot, "project-agents", "Web", "new-name", "a.md")
	if _, err := os.Lstat(oldCapture); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived the move (%v)", oldCapture, err)
	}
	assertIsLink(t, path, newCapture)

	rec, ok := loadState(t, repoRoot).LookupLink(path)
	if !ok {
		t.Fatal("the record vanished with the move")
	}
	if rec.Target != newCapture {
		t.Errorf("record target = %s, want %s", rec.Target, newCapture)
	}
	if rec.Origin != state.OriginCreated {
		t.Errorf("origin = %s, want %s; re-filing a record must not widen what may remove it", rec.Origin, state.OriginCreated)
	}
}

// TestRunReconcile_RekeyIsProposedNotApplied: the proposal appears in the
// report with the command that would carry it out, and the summary says why it
// was not carried out here.
func TestRunReconcile_RekeyIsProposedNotApplied(t *testing.T) {
	base, repoRoot := reconcileFixture(t)

	oldCapture := filepath.Join(repoRoot, "project-agents", "Nested", "proj", "a.md")
	mustWriteT(t, oldCapture, "captured\n")
	mustWriteT(t, projectAgentPath(base, "Web/proj", "a"), "captured\n")

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "--"+reconcile.RekeyFlag+" Nested/proj=Web/proj") {
		t.Errorf("the proposal does not carry the command that applies it:\n%s", stdout)
	}
	if !strings.Contains(stdout, "heuristic") {
		t.Errorf("the summary does not say why it was not applied:\n%s", stdout)
	}
	assertIsRegular(t, oldCapture, "a proposal moves nothing")
}

// --- adopt through the CLI --------------------------------------------------

// TestRunReconcile_AdoptRecordsUnownedLinksAndSaysWhatItCannotKnow.
func TestRunReconcile_AdoptRecordsUnownedLinksAndSaysWhatItCannotKnow(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	const project = "Web/proj"

	target := globalAgent(t, repoRoot, "a", "body\n")
	path := projectAgentPath(base, project, "a")
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "--"+reconcile.AdoptFlag) {
		t.Errorf("the report does not say how to record an unowned link:\n%s", stdout)
	}

	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.AdoptFlag, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "CANNOT tell") {
		t.Errorf("the report does not admit what an adopt pass cannot know:\n%s", stdout)
	}
	rec, ok := loadState(t, repoRoot).LookupLink(path)
	if !ok {
		t.Fatal("the link was not recorded")
	}
	if rec.Origin != state.OriginAdopted {
		t.Errorf("origin = %s, want %s", rec.Origin, state.OriginAdopted)
	}
}

// TestRunReconcile_AnUnjudgeablePathIsExitTwo: a recorded path holding
// something that is neither a link nor a file cannot be judged, and a run that
// could not judge everything must not exit like one that did.
func TestRunReconcile_AnUnjudgeablePathIsExitTwo(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	const project = "Web/proj"

	target := globalAgent(t, repoRoot, "a", "body\n")
	path := projectAgentPath(base, project, "a")
	recordInState(t, repoRoot, project, "a", path, target)
	mustMkdirT(t, path)

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stdout)
	}
	if !strings.Contains(stdout, "could not be judged") {
		t.Errorf("the summary does not say the run is incomplete:\n%s", stdout)
	}
}

// --- drop-capture through the CLI -------------------------------------------

// TestRunReconcile_DropCaptureNeedsTheBytesInHEAD. The uncommitted case is the
// one that matters: the report must refuse, say why, and leave the file.
func TestRunReconcile_DropCaptureNeedsTheBytesInHEAD(t *testing.T) {
	requireGit(t)
	const project = "Web/proj"

	t.Run("uncommitted refuses", func(t *testing.T) {
		base, repoRoot := reconcileFixture(t)
		testfs.GitRepo(t, repoRoot, "README.md", "hello\n")
		capture := filepath.Join(repoRoot, "project-agents", "Web", "proj", "cap.md")
		mustWriteT(t, capture, "never committed\n")

		code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base,
			"--"+reconcile.DropCaptureFlag+"="+project+"/cap", "--apply")...)
		if code != exitAtRisk {
			t.Errorf("exit = %d, want %d\n%s", code, exitAtRisk, stdout)
		}
		if !strings.Contains(stdout, string(reconcile.GuardUncommitted)) {
			t.Errorf("the report does not name the gate:\n%s", stdout)
		}
		assertIsRegular(t, capture, "an uncommitted capture may be the last copy of something")
	})

	t.Run("committed is deleted", func(t *testing.T) {
		base, repoRoot := reconcileFixture(t)
		testfs.GitRepo(t, repoRoot, "project-agents/Web/proj/cap.md", "captured\n")
		capture := filepath.Join(repoRoot, "project-agents", "Web", "proj", "cap.md")

		code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base,
			"--"+reconcile.DropCaptureFlag+"="+project+"/cap", "--apply")...)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		if _, err := os.Lstat(capture); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived (%v)", capture, err)
		}
	})
}

// --- home guard -------------------------------------------------------------

// TestRunReconcile_WrongHomeWritesNothing: a home that is not the one these
// projects live under makes every project key wrong, and the capture
// directories a re-key is computed from are keyed by exactly that. The dry run
// warns with the same sentence the apply refuses with.
func TestRunReconcile_WrongHomeWritesNothing(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

	// A home that exists and is simply not where these projects are, which
	// is what sudo, cron, and containers all produce.
	elsewhere := filepath.Join(base, "not-the-home")
	mustMkdirT(t, elsewhere)
	t.Setenv("HOME", elsewhere)

	code, _, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stderr)
	}
	assertIsLinkPath(t, path, "a run refused by the home guard writes nothing")

	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag)...)
	if code == exitFailure {
		t.Errorf("the dry run failed rather than warning\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "warning") {
		t.Errorf("the dry run did not warn about the home:\n%s", stderr)
	}
}

// --- reports ----------------------------------------------------------------

// TestRunReconcile_JSONReport: the machine-readable form carries the whole plan
// and what actually happened, not just one of the two.
func TestRunReconcile_JSONReport(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base,
		"--"+reconcile.PruneFlag, "--apply", "--json")...)
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}

	var rep struct {
		Apply     bool                `json:"apply"`
		Items     []reconcile.Item    `json:"items"`
		Counts    map[string]int      `json:"counts"`
		Rekeys    []reconcile.Rekey   `json:"rekeys"`
		Refused   []reconcile.Refused `json:"refused"`
		Pruned    []string            `json:"pruned"`
		Declined  []state.Declined    `json:"declined"`
		Adopted   []state.Record      `json:"adopted"`
		Repointed []reconcile.Repoint `json:"repointed"`
		Dropped   []string            `json:"dropped"`
		StateFile string              `json:"state_file"`
		Findings  []map[string]any    `json:"findings"`
		Problems  []scan.ScanProblem  `json:"problems"`
		Errors    []string            `json:"errors"`
		Examined  map[string]int      `json:"examined"`
		Home      map[string]any      `json:"home"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("parsing the JSON report: %v\n%s", err, stdout)
	}

	if !rep.Apply {
		t.Error("apply is false in a run that applied")
	}
	if len(rep.Pruned) != 1 || rep.Pruned[0] != path {
		t.Errorf("pruned = %v, want [%s]", rep.Pruned, path)
	}
	if rep.Counts[reconcile.OpPrune.String()] != 1 {
		t.Errorf("counts = %v", rep.Counts)
	}
	if len(rep.Items) != 1 || rep.Items[0].OpName != reconcile.OpPrune.String() {
		t.Errorf("items = %v", rep.Items)
	}
	if rep.StateFile == "" {
		t.Error("the report does not name the state file every gate reads")
	}
	// Every array is present rather than null, the rule each report here
	// follows: a consumer must be able to range over them unconditionally.
	for name, got := range map[string]bool{
		"rekeys": rep.Rekeys == nil, "refused": rep.Refused == nil,
		"declined": rep.Declined == nil, "adopted": rep.Adopted == nil,
		"repointed": rep.Repointed == nil, "dropped": rep.Dropped == nil,
		"findings": rep.Findings == nil, "problems": rep.Problems == nil,
		"errors": rep.Errors == nil,
	} {
		if got {
			t.Errorf("%s marshalled as null rather than an empty array", name)
		}
	}
}

// TestRunReconcile_ReportPointsAtDoctorForWhatItCannotSettle: reconcile's exit
// code is narrower than doctor's on purpose, and a reader has to be told so
// rather than inferring that a 0 here means nothing has drifted.
func TestRunReconcile_ReportPointsAtDoctorForWhatItCannotSettle(t *testing.T) {
	base, repoRoot := reconcileFixture(t)

	// A stale capture: real drift, no flag here removes it, and doctor is
	// where the assertion about it lives.
	mustMkdirT(t, projectAgentPathDir(base, "Web/proj"))
	mustWriteT(t, filepath.Join(repoRoot, "project-agents", "Web", "proj", "gone.md"), "captured\n")

	code, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d — a stale capture is doctor's to fail on\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "doctor") {
		t.Errorf("the summary does not point at doctor:\n%s", stdout)
	}
}

// TestRunReconcile_AnUnreadableStateFileStopsTheRun: every gate here reads that
// document, so a run that could not read it must not proceed as though it said
// nothing.
func TestRunReconcile_AnUnreadableStateFileStopsTheRun(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")
	mustWriteT(t, filepath.Join(repoRoot, state.FileName), `{"schema": 99, "links": []}`)

	code, _, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "schema") {
		t.Errorf("stderr does not explain the refusal:\n%s", stderr)
	}
	assertIsLinkPath(t, path, "a run that could not read its own record must change nothing")
}

// projectAgentPathDir is the .claude/agents directory of a project.
func projectAgentPathDir(base, project string) string {
	return filepath.Join(base, filepath.FromSlash(project), ".claude", "agents")
}

// assertIsLinkPath checks only that a symlink is still there, without caring
// where it points — the prune tests are about a link whose target is gone, so
// assertIsLink's resolution would fail for the wrong reason.
func assertIsLinkPath(t *testing.T, path, why string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s is gone (%v); %s", path, err, why)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (%v); %s", path, fi.Mode(), why)
	}
}

// TestRunReconcile_AMovedProjectWritesNoMarkers: finding 2 through the CLI,
// which is where it would actually have bitten — `mv` a project, run the
// command that needs no flag to write, and every agent in it comes back
// declined.
func TestRunReconcile_AMovedProjectWritesNoMarkers(t *testing.T) {
	base, repoRoot := reconcileFixture(t)
	const project = "Web/proj"

	for _, name := range []string{"a", "b"} {
		target := globalAgent(t, repoRoot, name, "body for "+name+"\n")
		path := projectAgentPath(base, project, name)
		mustMkdirT(t, filepath.Dir(path))
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		recordInState(t, repoRoot, project, name, path, target)
	}

	if err := os.Rename(filepath.Join(base, "Web", "proj"), filepath.Join(base, "Web", "proj2")); err != nil {
		t.Fatalf("moving the project: %v", err)
	}

	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "project-gone") {
		t.Errorf("the report does not name what it found:\n%s", stdout)
	}

	st := loadState(t, repoRoot)
	if n := len(st.AllDeclined()); n != 0 {
		t.Errorf("%d declined markers written for a project that merely moved: %v", n, st.AllDeclined())
	}
	if n := len(st.Records()); n != 2 {
		t.Errorf("records = %d, want both kept — dropping them makes the links at the new path unownable", n)
	}
}
