package importer

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// --- fixtures ---------------------------------------------------------------

// fixture is a throwaway world: an agent repo, and projects beside it under a
// base directory that stands in for $HOME.
//
// Entries come from a real scan rather than being constructed by hand, so
// every test below exercises the classifier and the planner against the same
// on-disk shapes the tool meets in the wild. A hand-made Entry could claim a
// state the filesystem does not actually produce.
type fixture struct {
	t        *testing.T
	base     string
	repoRoot string
	repo     *scan.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	mustMkdir(t, filepath.Join(repoRoot, "project-agents"))

	repo, err := scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	// Key projects relative to the fixture, not the developer's real home.
	// Without this every key lands under _outside-home/ and the tests would
	// be exercising a different namespace than the one they describe.
	repo.Home = base

	return &fixture{t: t, base: base, repoRoot: repoRoot, repo: repo}
}

// project writes a file into a project's .claude/agents directory and returns
// its path.
func (f *fixture) project(project, name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.base, project, ".claude", "agents", name+".md")
	mustWrite(f.t, p, content)
	return p
}

// global writes an agent into the repo's global pool.
func (f *fixture) global(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.repoRoot, "agents", name+".md")
	mustWrite(f.t, p, content)
	return p
}

// backup writes a file where an import would put it, standing in for a
// previous import run.
func (f *fixture) backup(project, name, content string) string {
	f.t.Helper()
	p := f.repo.BackupPath(project, name)
	mustWrite(f.t, p, content)
	return p
}

// commitRepo makes the fixture's agent repo a git repository and commits
// everything currently in it.
//
// Not decoration. --update refuses to replace a capture whose current bytes are
// in no commit here, because those bytes then exist nowhere else — so a fixture
// whose repo is not under git, or whose capture was written after the last
// commit, exercises that refusal rather than the replacement. Every test below
// that expects an update to proceed commits first, which is the same thing a
// user has to do.
func (f *fixture) commitRepo() {
	f.t.Helper()
	requireGit(f.t)
	if _, err := os.Lstat(filepath.Join(f.repoRoot, ".git")); err != nil {
		gitInit(f.t, f.repoRoot)
	}
	gitRun(f.t, f.repoRoot, "add", "-A", ".")
	// --allow-empty so the helper is callable before anything is captured;
	// a fixture that has to know whether it has content yet is a fixture
	// people work around.
	gitRun(f.t, f.repoRoot, "commit", "-q", "--allow-empty", "-m", "captures")
}

// noGit makes git unexecutable for the rest of the test, which is how the
// "could not ask" arm is reached without depending on file permissions.
//
// Permissions are the wrong lever here: CI runs as root, where a
// chmod-based fixture stops being unreadable and the test quietly passes
// without exercising anything. An empty PATH fails the exec itself, identically
// for every user.
func noGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func (f *fixture) entries() []scan.Entry {
	f.t.Helper()
	res, err := f.repo.Scan(f.base)
	if err != nil {
		f.t.Fatalf("Scan: %v", err)
	}
	if len(res.Problems) > 0 {
		f.t.Fatalf("fixture scan reported problems: %v", res.Problems)
	}
	return res.Entries
}

func (f *fixture) plan(opts Options) Plan {
	f.t.Helper()
	return NewPlan(f.repo, f.entries(), opts)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s exists but %s", path, why)
	}
}

// actions maps "project/name" to the action planned for it, which is what
// every selection test actually asserts on.
func actions(p Plan) map[string]Action {
	got := make(map[string]Action, len(p.Items))
	for _, i := range p.Items {
		got[i.Entry.Project+"/"+i.Entry.Name] = i.Action
	}
	return got
}

func assertActions(t *testing.T, p Plan, want map[string]Action) {
	t.Helper()
	got := actions(p)
	for key, wantAction := range want {
		if got[key] != wantAction {
			t.Errorf("%s: action = %v, want %v", key, got[key], wantAction)
		}
	}
	for key, gotAction := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s: unexpected item in the plan (%v)", key, gotAction)
		}
	}
}

func applyOK(t *testing.T, p Plan) []Item {
	t.Helper()
	written, errs := p.Apply()
	for _, err := range errs {
		t.Errorf("Apply: %v", err)
	}
	return written
}

// --- selection --------------------------------------------------------------

// TestNewPlan_CapturesOnlyWhatTheRepoLacks is the core of the command: states 6
// and 7 hold content that exists nowhere else and must be captured; everything
// else already has a copy in the repo and must be left alone.
//
// State 5 is the one that would be easy to get wrong. It is a real file, like
// states 6 and 7, and differs only in that its bytes already match the
// counterpart — so a selection that keyed off "is a regular file" rather than
// off the state would re-import it on every run.
func TestNewPlan_CapturesOnlyWhatTheRepoLacks(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")

	// State 7: no counterpart anywhere in the repo.
	f.project("solo", "bespoke", "exists only here\n")
	// State 6: shares a name with a global agent, differs in content.
	f.project("custom", "shared", "customized for this project\n")
	// State 5: byte-identical to the global agent — already captured.
	f.project("copycat", "shared", "canonical shared agent\n")
	// State 2: a healthy link into the repo owns no content of its own.
	linkDir := filepath.Join(f.base, "linked", ".claude", "agents")
	mustMkdir(t, linkDir)
	if err := os.Symlink(filepath.Join(f.repoRoot, "agents", "shared.md"),
		filepath.Join(linkDir, "shared.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{
		"solo/bespoke":  ActionImport,
		"custom/shared": ActionImport,
	})

	written := applyOK(t, plan)
	if len(written) != 2 {
		t.Fatalf("wrote %d files, want 2", len(written))
	}
	if got := mustRead(t, f.repo.BackupPath("solo", "bespoke")); got != "exists only here\n" {
		t.Errorf("captured content = %q", got)
	}
	if got := mustRead(t, f.repo.BackupPath("custom", "shared")); got != "customized for this project\n" {
		t.Errorf("captured override = %q", got)
	}

	// The states that were not selected must have had nothing written for
	// them — an import that also captured the adoptable copy would look
	// harmless and would quietly make copycat/shared a project-specific
	// counterpart it is not.
	mustNotExist(t, f.repo.BackupPath("copycat", "shared"),
		"an adoptable file is already captured and must not be imported")
	mustNotExist(t, f.repo.BackupPath("linked", "shared"),
		"a symlink owns no content and must not be imported")
}

// TestNewPlan_ApplyIsIdempotent is the second-run guarantee: once a file is
// captured it becomes byte-identical to its own counterpart, so the next scan
// classifies it as adoptable and the next plan has nothing to do.
func TestNewPlan_ApplyIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "exists only here\n")

	first := f.plan(Options{})
	if len(applyOK(t, first)) != 1 {
		t.Fatalf("first run wrote %d files, want 1", len(first.Writes()))
	}
	dest := f.repo.BackupPath("solo", "bespoke")
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat capture: %v", err)
	}

	second := f.plan(Options{})
	assertActions(t, second, map[string]Action{})
	if written := applyOK(t, second); len(written) != 0 {
		t.Errorf("second run wrote %v, want nothing", written)
	}

	after, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat capture after second run: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the capture was rewritten on an idempotent run (mtime %v -> %v)",
			before.ModTime(), after.ModTime())
	}
}

// TestNewPlan_DestinationAlreadyIdenticalIsCaptured covers the plan's own
// destination check rather than the classifier's.
//
// Reached through a scan, an identical destination shows up as state 5 and is
// filtered out earlier — so this asserts the planner is correct on its own
// terms and would not overwrite an identical file even if handed an entry that
// says otherwise.
func TestNewPlan_DestinationAlreadyIdenticalIsCaptured(t *testing.T) {
	f := newFixture(t)
	const body = "same bytes on both sides\n"
	src := f.project("solo", "bespoke", body)
	dest := f.backup("solo", "bespoke", body)

	// A state-7 entry with no hash: the planner must recompute rather than
	// compare the destination against nothing and call it a conflict.
	plan := NewPlan(f.repo, []scan.Entry{{
		Project: "solo",
		Name:    "bespoke",
		Path:    src,
		State:   scan.StateUnadopted,
		Tracked: scan.TrackedNo,
	}}, Options{})

	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionCaptured})
	if written := applyOK(t, plan); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != body {
		t.Errorf("destination content = %q, want it untouched", got)
	}
}

// --- conflicts --------------------------------------------------------------

// TestNewPlan_DifferingCaptureIsAConflict is the data-loss guard. The
// destination is itself an earlier capture of some project's file and may be
// the only copy of an older override, so a differing destination is never
// overwritten by default.
func TestNewPlan_DifferingCaptureIsAConflict(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "an older capture nobody else has\n")

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionConflict})
	if n := len(plan.Conflicts()); n != 1 {
		t.Errorf("Conflicts() = %d, want 1", n)
	}
	if n := len(plan.Writes()); n != 0 {
		t.Errorf("Writes() = %d, want 0: a conflict is a refusal, not a pending write", n)
	}
	if item := plan.Items[0]; item.DestHash == "" {
		t.Error("a conflict must record the destination hash it judged")
	}

	if written := applyOK(t, plan); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != "an older capture nobody else has\n" {
		t.Errorf("destination content = %q — the older capture was destroyed", got)
	}
}

// TestNewPlan_UpdateOptsIntoReplacing is the same fixture with the opt-in, on
// a capture that is committed and so can survive being replaced.
func TestNewPlan_UpdateOptsIntoReplacing(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "an older capture\n")
	f.commitRepo()

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionUpdate})

	if written := applyOK(t, plan); len(written) != 1 {
		t.Fatalf("wrote %d files, want 1", len(written))
	}
	if got := mustRead(t, dest); got != "the current project file\n" {
		t.Errorf("destination content = %q, want the project file", got)
	}
}

// TestNewPlan_UpdateSelectsOneCapture is the per-item form. --update is not a
// verdict on the run, it is a verdict on a file, and the report names files as
// <project>/<name>.
func TestNewPlan_UpdateSelectsOneCapture(t *testing.T) {
	f := newFixture(t)
	f.project("chosen", "bespoke", "the current project file\n")
	f.project("other", "bespoke", "also current\n")
	chosen := f.backup("chosen", "bespoke", "an older capture\n")
	other := f.backup("other", "bespoke", "another older capture\n")
	f.commitRepo()

	set, err := UpdateOnly("chosen/bespoke")
	if err != nil {
		t.Fatalf("UpdateOnly: %v", err)
	}
	plan := f.plan(Options{Update: set})
	assertActions(t, plan, map[string]Action{
		"chosen/bespoke": ActionUpdate,
		// Authorising one capture must not authorise its neighbour: the
		// blanket form is what the per-item form exists to avoid.
		"other/bespoke": ActionConflict,
	})

	applyOK(t, plan)
	if got := mustRead(t, chosen); got != "the current project file\n" {
		t.Errorf("selected capture = %q, want the project file", got)
	}
	if got := mustRead(t, other); got != "another older capture\n" {
		t.Errorf("unselected capture = %q — it was replaced anyway", got)
	}
}

// TestUpdateSet covers the selector's own rules, which decide whether a typed
// key authorises anything at all.
func TestUpdateSet(t *testing.T) {
	t.Run("accepts either spelling of the name", func(t *testing.T) {
		// The report prints "planner"; the file on disk is "planner.md".
		// People copy from whichever they are looking at, and a selector
		// that silently matches nothing is the worst possible answer.
		for _, key := range []string{"Web/example/planner", "Web/example/planner.md"} {
			set, err := UpdateOnly(key)
			if err != nil {
				t.Fatalf("UpdateOnly(%q): %v", key, err)
			}
			if !set.Allows("Web/example", "planner") {
				t.Errorf("%q does not authorise Web/example/planner", key)
			}
		}
	})

	t.Run("rejects a key that names no project", func(t *testing.T) {
		for _, key := range []string{"planner", "", "  ", "/planner", "Web/example/"} {
			if _, err := UpdateOnly(key); err == nil {
				t.Errorf("UpdateOnly(%q) was accepted, want an error naming the expected form", key)
			}
		}
	})

	t.Run("the zero value authorises nothing", func(t *testing.T) {
		var u UpdateSet
		if u.Enabled() || u.Blanket() || u.Allows("any", "thing") {
			t.Error("the zero UpdateSet authorises something")
		}
	})

	t.Run("blanket authorises everything", func(t *testing.T) {
		u := UpdateAll()
		if !u.Blanket() || !u.Enabled() || !u.Allows("any", "thing") {
			t.Error("UpdateAll does not authorise everything")
		}
		if len(u.Keys()) != 0 {
			t.Errorf("Keys() = %v, want none for the blanket form", u.Keys())
		}
	})

	t.Run("unmatched selectors are reported, matched ones are not", func(t *testing.T) {
		set, err := UpdateOnly("real/agent", "typo/agnet")
		if err != nil {
			t.Fatalf("UpdateOnly: %v", err)
		}
		items := []Item{{Entry: scan.Entry{Project: "real", Name: "agent"}, Action: ActionCaptured}}
		// "real/agent" matched an item that is already captured rather
		// than an update. That must still count as matched, or the
		// command that just succeeded would report a typo on its own
		// second run.
		got := set.Unmatched(items)
		if len(got) != 1 || got[0] != "typo/agnet" {
			t.Errorf("Unmatched() = %v, want only typo/agnet", got)
		}
	})
}

// TestNewPlan_NonRegularDestinationIsNeverReplaced covers what --update does
// NOT cover. "The capture is stale, replace it" is not "delete whatever is in
// the way", and a directory or a symlink at the destination is something this
// tool did not put there.
func TestNewPlan_NonRegularDestinationIsNeverReplaced(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")
	dest := f.repo.BackupPath("solo", "bespoke")
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(dest, "inside.txt"), "a directory in the way\n")

	for _, opts := range []Options{{}, {Update: UpdateAll()}} {
		plan := f.plan(opts)
		assertActions(t, plan, map[string]Action{"solo/bespoke": ActionConflict})
		if written := applyOK(t, plan); len(written) != 0 {
			t.Errorf("update=%v: wrote %v, want nothing", opts.Update.Enabled(), written)
		}
		if got := mustRead(t, filepath.Join(dest, "inside.txt")); got == "" {
			t.Errorf("update=%v: the directory at the destination was destroyed", opts.Update.Enabled())
		}
	}
}

// --- the commit gate --------------------------------------------------------

// TestNewPlan_UpdateRefusesAnUntrackedCapture is the data-loss bug --update
// carried from the day it was added, in its simplest form: the capture is not
// in HEAD at all.
//
// Its whole safety argument is that project-agents/ is version controlled:
// replacing a capture is survivable because the replaced bytes stay recoverable
// from a commit. Nothing verified that. A capture written by one run and
// replaced by the next --update before anyone committed is gone permanently,
// with no copy anywhere — the exact loss this package exists to prevent,
// carried out by the command meant to prevent it.
func TestNewPlan_UpdateRefusesAnUntrackedCapture(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	// Committed first, so the repo has a HEAD. The capture below lands
	// after it and is therefore untracked — which is precisely the state a
	// previous import run leaves behind before anyone commits.
	f.commitRepo()
	dest := f.backup("solo", "bespoke", "an older capture nobody has committed\n")

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionConflict})

	item := plan.Items[0]
	if item.Refusal != RefusalUncommitted {
		t.Errorf("refusal = %q, want %q", item.Refusal, RefusalUncommitted)
	}
	// The refusal has to say what it is protecting and how to proceed. A
	// bare "refused" on a flag the user just passed reads as a bug.
	for _, want := range []string{"in no commit", "commit project-agents"} {
		if !strings.Contains(item.Reason, want) {
			t.Errorf("reason %q does not mention %q", item.Reason, want)
		}
	}
	if n := len(plan.Refused(RefusalUncommitted)); n != 1 {
		t.Errorf("Refused(RefusalUncommitted) = %d items, want 1", n)
	}

	if written := applyOK(t, plan); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != "an older capture nobody has committed\n" {
		t.Errorf("destination = %q — the only copy of the older capture was destroyed", got)
	}

	// Committing it is the documented fix, and it has to actually work.
	f.commitRepo()
	after := f.plan(Options{Update: UpdateAll()})
	assertActions(t, after, map[string]Action{"solo/bespoke": ActionUpdate})
	applyOK(t, after)
	if got := mustRead(t, dest); got != "the current project file\n" {
		t.Errorf("destination after committing = %q, want the project file", got)
	}
}

// TestNewPlan_UpdateRefusesATrackedButModifiedCapture is the gap a path-only
// gate leaves open, and the reason the check is on the bytes.
//
// `ls-tree HEAD` proves the path is in HEAD. It proves nothing about the file
// on disk: a capture committed as v1 and edited to v2 passes that check while
// v2 exists in no commit, so replacing it destroys v2 permanently.
func TestNewPlan_UpdateRefusesATrackedButModifiedCapture(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.project("solo", "bespoke", "v3, the project file now\n")
	f.backup("solo", "bespoke", "v1, committed\n")
	f.commitRepo()

	// Edited after the commit. The path is still in HEAD; these bytes are
	// not, and they are the ones --update would destroy.
	dest := f.backup("solo", "bespoke", "v2, edited since the commit and held nowhere else\n")

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionConflict})
	if got := plan.Items[0].Refusal; got != RefusalUncommitted {
		t.Errorf("refusal = %q, want %q", got, RefusalUncommitted)
	}

	if written := applyOK(t, plan); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != "v2, edited since the commit and held nowhere else\n" {
		t.Fatalf("destination = %q — v2 was destroyed and is in no commit", got)
	}
}

// TestApply_ASecondUpdateDoesNotDestroyTheFirstOne is the same gap reached
// through the tool's own normal use, with no hand-editing at all.
//
// --update writes the project file over the capture, which leaves the capture
// modified against HEAD. Under a path-only gate the very next --update — after
// the project file changes again, which is the whole reason anyone runs this
// twice — would replace those bytes, and they are in no commit. The loss class
// N4 exists to prevent, reachable in two ordinary commands.
func TestApply_ASecondUpdateDoesNotDestroyTheFirstOne(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	src := f.project("solo", "bespoke", "v2\n")
	dest := f.backup("solo", "bespoke", "v1\n")
	f.commitRepo()

	// First --update: authorised, because v1 is committed and unmodified.
	first := f.plan(Options{Update: UpdateAll()})
	assertActions(t, first, map[string]Action{"solo/bespoke": ActionUpdate})
	applyOK(t, first)
	if got := mustRead(t, dest); got != "v2\n" {
		t.Fatalf("after the first update the capture is %q, want v2", got)
	}

	// The project moves on. Nobody has committed the capture of v2.
	mustWrite(t, src, "v3\n")

	second := f.plan(Options{Update: UpdateAll()})
	assertActions(t, second, map[string]Action{"solo/bespoke": ActionConflict})
	if got := second.Items[0].Refusal; got != RefusalUncommitted {
		t.Errorf("refusal = %q, want %q", got, RefusalUncommitted)
	}
	if written := applyOK(t, second); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != "v2\n" {
		t.Fatalf("capture = %q — the first update's result was destroyed by the second", got)
	}

	// Committing v2 is what makes the third state safe to replace.
	f.commitRepo()
	third := f.plan(Options{Update: UpdateAll()})
	assertActions(t, third, map[string]Action{"solo/bespoke": ActionUpdate})
	applyOK(t, third)
	if got := mustRead(t, dest); got != "v3\n" {
		t.Errorf("capture = %q, want v3 once v2 was committed", got)
	}
}

// TestApply_UpdateRefusesWhenHEADMovedAfterThePlan puts the byte check on the
// write path rather than the planning path.
//
// Reaching it takes a little arranging, because the DestHash check upstream
// already refuses a capture whose *file* changed since the plan. What it does
// not see is HEAD moving underneath an unchanged file: the bytes on disk are
// exactly what the plan judged, and are now in no commit. Only re-asking git at
// write time catches that, which is why the gate is not trusted from the plan.
func TestApply_UpdateRefusesWhenHEADMovedAfterThePlan(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "v1\n")
	f.commitRepo()

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionUpdate})

	// HEAD moves on to hold something else, and the file is then put back
	// to the bytes the plan judged. DestHash therefore still matches, and
	// the commit gate is the only thing between v1 and a rename.
	f.backup("solo", "bespoke", "v9\n")
	f.commitRepo()
	mustWrite(t, dest, "v1\n")

	written, errs := plan.Apply()
	if len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "refusing to replace") {
		t.Errorf("error = %v, want a refusal to replace", errs[0])
	}
	if got := mustRead(t, dest); got != "v1\n" {
		t.Errorf("destination = %q — bytes held in no commit were replaced", got)
	}
}

// TestNewPlan_UpdateRefusesWhenGitCannotAnswer is the fail-closed property, and
// the one most likely to regress.
//
// "Not committed" and "git could not be asked" are different facts, but neither
// permits replacing a capture. Only scan.CommittedYes may
// authorise replacing a capture; every other answer, including one the probe
// could not produce at all, refuses.
//
// CommittedYes, not TrackedYes, and the distinction is the point of the gate
// rather than a detail of it: TrackedYes says the destination's *path* is in
// HEAD, which is true of a capture committed as v1 and since edited to v2, and
// replacing it would destroy v2. The byte-level question exists precisely so
// that TrackedYes cannot authorise a write.
func TestNewPlan_UpdateRefusesWhenGitCannotAnswer(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "an older capture\n")
	// Committed and unmodified, so the answer under a working git would be
	// CommittedYes and the update would proceed. The only thing this test
	// changes is git's ability to say so.
	f.commitRepo()
	noGit(t)

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionConflict})
	if got := plan.Items[0].Refusal; got != RefusalCommitUnknown {
		t.Errorf("refusal = %q, want %q — an unanswerable question must not read as a plain no", got, RefusalCommitUnknown)
	}

	if written := applyOK(t, plan); len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if got := mustRead(t, dest); got != "an older capture\n" {
		t.Errorf("destination = %q — it was replaced on an answer git never gave", got)
	}
}

// TestApply_UpdateRefusesWhenGitCannotAnswerAtWriteTime is the same property one
// layer down.
//
// The plan's gate is what a dry run shows the user; this one is what stands
// between an authorised update and the syscall. They are separate on purpose —
// the plan's verdict was formed before a scan that may have walked a hundred
// repositories — so the write path has to refuse on its own, not because the
// planner already did.
func TestApply_UpdateRefusesWhenGitCannotAnswerAtWriteTime(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "an older capture\n")
	f.commitRepo()

	// Planned while git could answer, so this really is an authorised
	// update rather than a conflict the planner already rejected.
	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionUpdate})

	noGit(t)

	written, errs := plan.Apply()
	if len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "refusing to replace") {
		t.Errorf("error = %v, want a refusal to replace", errs[0])
	}
	if got := mustRead(t, dest); got != "an older capture\n" {
		t.Errorf("destination = %q — it was replaced without the gate being satisfied", got)
	}
}

// TestApply_ImportIsUnaffectedByTheCommitGate: the gate guards replacement, and
// an import replaces nothing. Requiring a commit to create a file that is not
// there would refuse every first capture in a fresh repo, which is the command's
// entire purpose.
func TestApply_ImportIsUnaffectedByTheCommitGate(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	f.commitRepo()
	f.project("solo", "bespoke", "only here\n")
	noGit(t)

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionImport})
	if written := applyOK(t, plan); len(written) != 1 {
		t.Fatalf("wrote %d files, want 1", len(written))
	}
	if got := mustRead(t, f.repo.BackupPath("solo", "bespoke")); got != "only here\n" {
		t.Errorf("captured content = %q", got)
	}
}

// --- tracked-in-its-own-repo ------------------------------------------------

// TestNewPlan_TrackedInItsOwnRepoIsSkippedByDefault is the case of a project with four
// state-7 agents with no copy in this repo, all committed in the project's own
// repository and therefore not at risk.
//
// The default is to leave them alone. Capturing them would give the content two
// owners — the project's repo stays the place it is edited — and the copy here
// would start diverging immediately. The flag exists because that is a
// judgement about someone else's repository.
func TestNewPlan_TrackedInItsOwnRepoIsSkippedByDefault(t *testing.T) {
	requireGit(t)

	f := newFixture(t)
	// Committed in its own repo: safe.
	tracked := filepath.Join(f.base, "example")
	gitInit(t, tracked)
	f.project("example", "planner", "committed in example\n")
	gitRun(t, tracked, "add", ".")
	gitRun(t, tracked, "commit", "-qm", "agents")

	// Not committed anywhere: at risk.
	f.project("loose", "orphan", "protected nowhere\n")

	t.Run("skipped by default", func(t *testing.T) {
		plan := f.plan(Options{})
		assertActions(t, plan, map[string]Action{
			"example/planner": ActionSkipTracked,
			"loose/orphan":    ActionImport,
		})
		// Reported, not silently dropped: a user has to be able to see
		// what was skipped and why.
		if plan.Items[1].Reason == "" {
			t.Error("a skipped item must carry its reason")
		}
		if written := applyOK(t, plan); len(written) != 1 {
			t.Fatalf("wrote %d files, want 1 (only the unprotected one)", len(written))
		}
		mustNotExist(t, f.repo.BackupPath("example", "planner"),
			"a file committed in its own repo is not captured by default")
	})

	t.Run("captured with --include-tracked", func(t *testing.T) {
		plan := f.plan(Options{IncludeTracked: true})
		// loose/orphan is absent entirely: the previous subtest captured
		// it, so the scan now classifies it as adoptable and it never
		// reaches the planner.
		assertActions(t, plan, map[string]Action{"example/planner": ActionImport})
		applyOK(t, plan)
		if got := mustRead(t, f.repo.BackupPath("example", "planner")); got != "committed in example\n" {
			t.Errorf("captured content = %q", got)
		}
	})
}

// --- writing ----------------------------------------------------------------

// TestApply_LeavesNoTemporaryBehind guards the atomic write. A stray .tmp in
// project-agents/ would be committed as if it were a captured agent.
func TestApply_LeavesNoTemporaryBehind(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")
	applyOK(t, f.plan(Options{}))

	dir := filepath.Dir(f.repo.BackupPath("solo", "bespoke"))
	names := lsNames(t, dir)
	if len(names) != 1 || names[0] != "bespoke.md" {
		t.Errorf("destination directory holds %v, want only bespoke.md", names)
	}
}

// TestCopyAtomic_FailedCopyLeavesNothing checks the failure path of the same
// guard: a copy that dies partway must leave neither a temporary nor a
// half-written destination.
func TestCopyAtomic_FailedCopyLeavesNothing(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "captures", "agent.md")

	// A directory as the source: Open succeeds on macOS and Linux, and the
	// read fails, so the copy dies after the temporary exists.
	src := filepath.Join(base, "not-a-file")
	mustMkdir(t, src)

	if err := copyAtomic(src, dest, "", ActionImport); err == nil {
		t.Fatal("copyAtomic from a directory succeeded, want an error")
	}
	mustNotExist(t, dest, "a failed copy must not leave a destination behind")
	if names := lsNames(t, filepath.Dir(dest)); len(names) != 0 {
		t.Errorf("failed copy left %v behind", names)
	}
}

// TestCopyAtomic_ImportRefusesADestinationThatAppeared is the window recheckDest
// cannot close, tested where it actually is.
//
// The gap between the recheck and the write spans the whole copy — io.Copy plus
// Sync — not the microseconds the old comment claimed, and a destination that
// appears inside it was never examined by this run. Going through Apply would
// only re-test the recheck, because the recheck runs first and catches anything
// already there; calling copyAtomic directly puts the destination exactly where
// the race would leave it. That is the deterministic form of a race that cannot
// be scheduled.
//
// os.Link is what closes it: it fails with EEXIST instead of replacing, so the
// check and the write are one syscall. os.Rename, which this used
// unconditionally, would have destroyed the file.
func TestCopyAtomic_ImportRefusesADestinationThatAppeared(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "project", "agent.md")
	mustWrite(t, src, "the file being captured\n")

	dest := filepath.Join(base, "captures", "agent.md")
	mustWrite(t, dest, "appeared mid-copy; the only copy of something\n")

	err := copyAtomic(src, dest, "", ActionImport)
	if err == nil {
		t.Fatal("an import replaced a destination that was already there")
	}
	if !strings.Contains(err.Error(), "refusing to replace") {
		t.Errorf("error = %v, want a refusal to replace", err)
	}
	if got := mustRead(t, dest); got != "appeared mid-copy; the only copy of something\n" {
		t.Errorf("destination = %q — it was overwritten", got)
	}
	if names := lsNames(t, filepath.Dir(dest)); len(names) != 1 || names[0] != "agent.md" {
		t.Errorf("the refused capture left %v behind, want only the destination", names)
	}
}

// TestCopyAtomic_UpdateReplacesTheDestination is the other half: replacement is
// the point of an update, so it must still happen. If this and the test above
// both passed with one code path, the code path would be wrong for one of them.
func TestCopyAtomic_UpdateReplacesTheDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "project", "agent.md")
	mustWrite(t, src, "the file being captured\n")

	dest := filepath.Join(base, "captures", "agent.md")
	mustWrite(t, dest, "the stale capture\n")

	if err := copyAtomic(src, dest, "", ActionUpdate); err != nil {
		t.Fatalf("copyAtomic: %v", err)
	}
	if got := mustRead(t, dest); got != "the file being captured\n" {
		t.Errorf("destination = %q, want the source content", got)
	}
	if names := lsNames(t, filepath.Dir(dest)); len(names) != 1 || names[0] != "agent.md" {
		t.Errorf("the update left %v behind, want only the destination", names)
	}
}

// TestCopyAtomic_ImportLeavesOneNameForTheCapture: link creates a second name
// for the temporary's inode, so the temporary has to be unlinked afterwards. A
// missed Remove would leave a .tmp beside every capture, one per import.
func TestCopyAtomic_ImportLeavesOneNameForTheCapture(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "project", "agent.md")
	mustWrite(t, src, "only here\n")
	dest := filepath.Join(base, "captures", "agent.md")

	if err := copyAtomic(src, dest, "", ActionImport); err != nil {
		t.Fatalf("copyAtomic: %v", err)
	}
	if names := lsNames(t, filepath.Dir(dest)); len(names) != 1 || names[0] != "agent.md" {
		t.Errorf("directory holds %v, want only agent.md", names)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat capture: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("capture mode = %v, want a regular file", fi.Mode())
	}
	if got := mustRead(t, dest); got != "only here\n" {
		t.Errorf("capture = %q", got)
	}
}

// TestApply_RefusesContentThatAppearedAfterPlanning covers the window between
// planning and writing. The plan authorised an import because nothing was at
// the destination; if something is there by the time we write, it is content
// this run never examined.
func TestApply_RefusesContentThatAppearedAfterPlanning(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")
	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionImport})

	dest := f.backup("solo", "bespoke", "appeared in the meantime\n")

	written, errs := plan.Apply()
	if len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want a refusal to overwrite", errs[0])
	}
	if got := mustRead(t, dest); got != "appeared in the meantime\n" {
		t.Errorf("destination content = %q — it was overwritten", got)
	}
}

// TestApply_ImportIsSatisfiedByADestinationHoldingTheSameBytes is the guard for
// the short-circuit in importOne, and specifically for the regression that
// short-circuit was written to prevent.
//
// The sibling test above is the *different*-content case, which must refuse.
// This is the identical-content case, which must not: something else — a
// concurrent run, a hand copy — has already captured exactly what this item
// would write, so the file is captured, nothing needs writing, and the item
// counts as done.
//
// It is a regression guard rather than a feature test. ActionImport places its
// file with os.Link, a create-if-absent that fails with EEXIST rather than
// replacing; before recheckDest reported this case as satisfied, that EEXIST
// surfaced as a write error and the run exited 2 where it had previously exited
// 0 — an outcome that reads as "the import failed" for a destination that holds
// precisely the right bytes. The satisfied path was otherwise exercised only
// through ActionUpdate, which takes the os.Rename branch and so never meets the
// EEXIST at all.
//
// It also pins the upstream invariant placeCapture's own comment asserts: that
// this case is caught before it gets there, so placeCapture's EEXIST message —
// "content this run never examined" — is only ever printed about content the
// run genuinely has not examined.
func TestApply_ImportIsSatisfiedByADestinationHoldingTheSameBytes(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")
	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionImport})

	// The exact bytes the plan would have written, put there after it was
	// made. No commit gate applies: nothing is being destroyed.
	dest := f.backup("solo", "bespoke", "only here\n")

	written, errs := plan.Apply()
	if len(errs) != 0 {
		t.Fatalf("errors = %v, want none: the destination already holds what we would have written", errs)
	}
	if len(written) != 1 {
		t.Errorf("written = %v, want the item counted as captured", written)
	}
	if got := mustRead(t, dest); got != "only here\n" {
		t.Errorf("destination content = %q", got)
	}

	// Nothing was written, so nothing can have been left half-written. A
	// stray temporary here would mean the copy ran and then failed to place
	// its result — the very path this short-circuit exists to skip.
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(dest), ".*"))
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporaries left behind: %v", leftovers)
	}
}

// TestApply_RefusesASourceThatChangedAfterPlanning is the other half of the
// planning window. The destination check answers "may I write here"; nothing
// answered "is this still the file I classified" — os.Open follows symlinks and
// reads whatever is there now, so a project file that changed between the plan
// and the write would be captured as if the classifier had judged it.
//
// It is worst on --update, where the run would replace a capture — possibly the
// only copy of an override — with content it never examined.
func TestApply_RefusesASourceThatChangedAfterPlanning(t *testing.T) {
	f := newFixture(t)
	src := f.project("solo", "bespoke", "what the scan classified\n")
	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionImport})
	if plan.Items[0].SrcHash == "" {
		t.Fatal("the plan recorded no source hash, so there is nothing to verify against")
	}

	mustWrite(t, src, "swapped in after the plan was made\n")

	written, errs := plan.Apply()
	if len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "changed while being captured") {
		t.Errorf("error = %v, want a refusal naming the changed source", errs[0])
	}

	dest := f.repo.BackupPath("solo", "bespoke")
	mustNotExist(t, dest, "a refused capture must not leave the unverified content behind")
	if names := lsNames(t, filepath.Dir(dest)); len(names) != 0 {
		t.Errorf("a refused capture left %v behind", names)
	}
}

// TestApply_RefusesAnUpdateWhoseDestinationChanged: --update authorises
// replacing the stale capture the plan showed the user, identified by
// Item.DestHash. It is not a licence to replace whatever occupies the path by
// the time the write runs — that content is the only copy of something, and
// this run never examined it.
func TestApply_RefusesAnUpdateWhoseDestinationChanged(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "the current project file\n")
	dest := f.backup("solo", "bespoke", "the stale capture the plan judged\n")
	f.commitRepo()

	plan := f.plan(Options{Update: UpdateAll()})
	assertActions(t, plan, map[string]Action{"solo/bespoke": ActionUpdate})
	if plan.Items[0].DestHash == "" {
		t.Fatal("an update must record the destination hash it was authorised against")
	}

	mustWrite(t, dest, "something else entirely, written after the plan\n")

	written, errs := plan.Apply()
	if len(written) != 0 {
		t.Errorf("wrote %v, want nothing", written)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "--update does not authorise") {
		t.Errorf("error = %v, want a refusal explaining the limit of --update", errs[0])
	}
	if got := mustRead(t, dest); got != "something else entirely, written after the plan\n" {
		t.Errorf("destination content = %q — it was overwritten", got)
	}
}

// TestApply_UpdateProceedsWhenTheDestinationIsUntouched is the same guard from
// the other side: verifying the destination must not make the ordinary --update
// path refuse, and a destination someone already brought to the right content is
// the outcome we wanted rather than a conflict.
func TestApply_UpdateProceedsWhenTheDestinationIsUntouched(t *testing.T) {
	const current = "the current project file\n"

	t.Run("unchanged since planning", func(t *testing.T) {
		f := newFixture(t)
		f.project("solo", "bespoke", current)
		dest := f.backup("solo", "bespoke", "the stale capture\n")
		f.commitRepo()

		if written := applyOK(t, f.plan(Options{Update: UpdateAll()})); len(written) != 1 {
			t.Fatalf("wrote %d files, want 1", len(written))
		}
		if got := mustRead(t, dest); got != current {
			t.Errorf("destination content = %q, want the project file", got)
		}
	})

	t.Run("already holds the source content", func(t *testing.T) {
		f := newFixture(t)
		f.project("solo", "bespoke", current)
		dest := f.backup("solo", "bespoke", "the stale capture\n")
		f.commitRepo()

		plan := f.plan(Options{Update: UpdateAll()})
		// Someone captured the same file between the plan and the write.
		mustWrite(t, dest, current)

		if written := applyOK(t, plan); len(written) != 1 {
			t.Fatalf("wrote %d files, want 1", len(written))
		}
		if got := mustRead(t, dest); got != current {
			t.Errorf("destination content = %q, want the project file", got)
		}
	})
}

// TestApply_OneFailureDoesNotAbandonTheRest: the items are independent files in
// independent projects. Capturing nine of ten is strictly better than
// capturing none because the tenth was in the way.
func TestApply_OneFailureDoesNotAbandonTheRest(t *testing.T) {
	f := newFixture(t)
	f.project("aaa", "doomed", "will fail\n")
	f.project("zzz", "fine", "will succeed\n")

	plan := f.plan(Options{})
	if len(plan.Writes()) != 2 {
		t.Fatalf("planned %d writes, want 2", len(plan.Writes()))
	}
	// Put a directory where the first import wants to write, after the plan
	// was made, so exactly one write fails.
	mustMkdir(t, f.repo.BackupPath("aaa", "doomed"))

	written, errs := plan.Apply()
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one", errs)
	}
	if len(written) != 1 || written[0].Entry.Name != "fine" {
		t.Errorf("written = %v, want the one item that could be written", written)
	}
	if got := mustRead(t, f.repo.BackupPath("zzz", "fine")); got != "will succeed\n" {
		t.Errorf("surviving capture = %q", got)
	}
}

// TestApply_CapturedFileIsReadable: CreateTemp makes 0600 files, and a capture
// is an ordinary file in a git repository like every other file here.
func TestApply_CapturedFileIsReadable(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")
	applyOK(t, f.plan(Options{}))

	fi, err := os.Lstat(f.repo.BackupPath("solo", "bespoke"))
	if err != nil {
		t.Fatalf("lstat capture: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("capture mode = %v, want 0644", fi.Mode().Perm())
	}
}

// --- plan bookkeeping -------------------------------------------------------

func TestPlan_CountsAndActionNames(t *testing.T) {
	f := newFixture(t)
	f.project("aaa", "one", "only here\n")
	f.project("bbb", "two", "only here too\n")
	f.backup("bbb", "two", "a differing older capture\n")

	plan := f.plan(Options{})
	counts := plan.Counts()
	if counts[ActionImport] != 1 || counts[ActionConflict] != 1 {
		t.Errorf("counts = %v, want one import and one conflict", counts)
	}

	// Items are sorted so a report and its JSON are deterministic.
	if plan.Items[0].Entry.Project != "aaa" || plan.Items[1].Entry.Project != "bbb" {
		t.Errorf("items are not sorted by project: %v", plan.Items)
	}
	for _, item := range plan.Items {
		if item.ActionName != item.Action.String() {
			t.Errorf("%s: ActionName = %q, want %q", item, item.ActionName, item.Action.String())
		}
		if item.Dest != f.repo.BackupPath(item.Entry.Project, item.Entry.Name) {
			t.Errorf("%s: dest = %q, want the repo's BackupPath", item, item.Dest)
		}
	}
}

// TestPlan_UnjudgeableDestinationBecomesAProblem: a destination whose safety
// cannot be determined is dropped from the plan rather than written blind.
func TestPlan_UnjudgeableDestinationBecomesAProblem(t *testing.T) {
	f := newFixture(t)
	f.project("solo", "bespoke", "only here\n")

	// A regular file where the project's capture directory belongs. Lstat on
	// the destination beneath it fails with ENOTDIR, which is neither "it is
	// not there" nor an answer we may act on.
	mustWrite(t, filepath.Join(f.repoRoot, "project-agents", "solo"), "not a directory\n")

	plan := f.plan(Options{})
	if len(plan.Items) != 0 {
		t.Errorf("items = %v, want none: an unjudgeable destination is not a write", plan.Items)
	}
	if len(plan.Problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", plan.Problems)
	}
	if !strings.Contains(plan.Problems[0].Path, "solo") {
		t.Errorf("problem does not name the destination: %v", plan.Problems[0])
	}
}

func TestAction_StringAndWrites(t *testing.T) {
	cases := []struct {
		action Action
		name   string
		writes bool
	}{
		{ActionImport, "import", true},
		{ActionUpdate, "update", true},
		{ActionCaptured, "captured", false},
		{ActionConflict, "conflict", false},
		{ActionSkipTracked, "skipped", false},
		{ActionInvalid, "invalid", false},
	}
	for _, tc := range cases {
		if got := tc.action.String(); got != tc.name {
			t.Errorf("String() = %q, want %q", got, tc.name)
		}
		if got := tc.action.Writes(); got != tc.writes {
			t.Errorf("%s.Writes() = %v, want %v", tc.name, got, tc.writes)
		}
	}
}

// --- local helpers ----------------------------------------------------------

func lsNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A developer's global hooks, templates, or gpg config must not decide
	// whether these assertions pass.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig-system"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	mustMkdir(t, dir)
	gitRun(t, dir, "init", "-q", "-b", "main")
}
