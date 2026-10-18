package linker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// --- the git gate -----------------------------------------------------------

// TestNewPlan_VisibleAgentsDirectoryLinksWithoutExcludeLocal: untracked and not
// ignored means the links show in git status, which a user can see and act on.
// Reported, not refused, and nothing is excluded without the flag.
func TestNewPlan_VisibleAgentsDirectoryLinksWithoutExcludeLocal(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	f.gitInit("visible")
	path := f.project("visible", "shared", body)
	exclude := seedExclude(t, f, "visible")

	plan := f.plan(Options{})
	if len(plan.Blocked) != 0 || len(plan.Excludes) != 0 {
		t.Fatalf("blocked %v, excludes %v; want neither", plan.Blocked, plan.Excludes)
	}
	if len(plan.Git) != 1 || plan.Git[0].Tracking != scan.DirVisible.String() {
		t.Fatalf("git = %+v, want the directory reported visible", plan.Git)
	}
	if !strings.Contains(plan.Git[0].Note, ExcludeLocalFlag) {
		t.Errorf("note = %q, want it to name --%s", plan.Git[0].Note, ExcludeLocalFlag)
	}

	applyOK(t, f, plan)
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
	if got := mustRead(t, f.excludeFile("visible")); got != exclude {
		t.Errorf("exclude file edited without --%s:\n%s", ExcludeLocalFlag, got)
	}
}

// gitWithoutGitDir puts a git first on PATH that refuses only
// --absolute-git-dir, which is the one answer --exclude-local cannot proceed
// without. Real git cannot be made to fail that query alone on demand.
func gitWithoutGitDir(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git not on PATH: %v", err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --absolute-git-dir ] && exit 128; done\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestNewPlan_ExcludeLocalWithoutAGitDirIsRefused(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	before := seedExclude(t, f, "tracked")
	gitWithoutGitDir(t)

	plan := f.plan(Options{ExcludeLocal: true})
	if len(plan.Blocked) != 1 || !strings.Contains(plan.Blocked[0].Reason, "git directory") {
		t.Fatalf("blocked = %+v, want the project refused for want of a git directory", plan.Blocked)
	}
	if len(plan.Excludes) != 0 || len(plan.Writes()) != 0 {
		t.Errorf("planned excludes %v and writes %v with nowhere to exclude into", plan.Excludes, plan.Writes())
	}

	applyOK(t, f, plan)
	assertRegular(t, path, "a project that could not be excluded must not be linked into")
	if got := mustRead(t, f.excludeFile("tracked")); got != before {
		t.Errorf("exclude file changed:\n%s", got)
	}
}

// TestNewPlan_UnreadableExcludeFileIsRefused. A directory where the exclude
// file belongs reads as EISDIR, which must not be mistaken for "no rules yet".
func TestNewPlan_UnreadableExcludeFileIsRefused(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	exclude := f.excludeFile("tracked")
	if err := os.Remove(exclude); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(exclude, "kept")
	mustWrite(t, kept, "inside\n")

	plan := f.plan(Options{ExcludeLocal: true})
	if len(plan.Blocked) != 1 || !strings.Contains(plan.Blocked[0].Reason, "reading") {
		t.Fatalf("blocked = %+v, want the project refused for an unreadable exclude file", plan.Blocked)
	}
	if len(plan.Excludes) != 0 || len(plan.Writes()) != 0 {
		t.Errorf("planned excludes %v and writes %v", plan.Excludes, plan.Writes())
	}

	applyOK(t, f, plan)
	assertRegular(t, path, "a refused project must not be linked into")
	if got := mustRead(t, kept); got != "inside\n" {
		t.Errorf("%s = %q, want it untouched", kept, got)
	}
}

func TestNewPlan_ExclusionAlreadyPresentIsNotPlannedAgain(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	const rules = "*.local\n  /.claude/agents/  \n"
	mustWrite(t, f.excludeFile("tracked"), rules)

	plan := f.plan(Options{ExcludeLocal: true})
	if len(plan.Blocked) != 0 || len(plan.Excludes) != 0 {
		t.Fatalf("blocked %v, excludes %v; want neither, the pattern is already there", plan.Blocked, plan.Excludes)
	}
	if len(plan.Git) != 1 || !strings.Contains(plan.Git[0].Note, "already excluded") {
		t.Errorf("git = %+v, want it reported as already excluded", plan.Git)
	}

	applyOK(t, f, plan)
	if got := mustRead(t, f.excludeFile("tracked")); got != rules {
		t.Errorf("exclude file = %q, want it byte-identical", got)
	}
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// --- judging ----------------------------------------------------------------

// TestNewPlan_LinkToARepoFileThatIsNotItsCounterpart: a link into the repo
// whose name has no counterpart. The report must name where it actually points.
func TestNewPlan_LinkToARepoFileThatIsNotItsCounterpart(t *testing.T) {
	f := newFixture(t)
	shared := f.global("shared", "canonical shared agent\n")
	path := f.symlink("proj", "alias", shared)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"proj/alias": ActionMismatch})
	item := plan.Items[0]
	if item.Target != shared {
		t.Errorf("target = %q, want the link's own target %q", item.Target, shared)
	}
	if !strings.Contains(item.Reason, "no file this project has as a counterpart") {
		t.Errorf("reason = %q", item.Reason)
	}
	if len(plan.Unresolved()) != 1 {
		t.Errorf("unresolved = %v, want the mismatch", plan.Unresolved())
	}

	applyOK(t, f, plan)
	if got, err := os.Readlink(path); err != nil || got != shared {
		t.Errorf("readlink = %q, %v; link does not repoint", got, err)
	}
}

// TestPointsAt_EmptyTargetNeverMatches: an entry with no link target would
// otherwise compare equal to an empty counterpart and read as linked.
func TestPointsAt_EmptyTargetNeverMatches(t *testing.T) {
	if pointsAt(scan.Entry{}, "") {
		t.Error("an empty target matched an entry with no link target")
	}
}

// TestJudge_ZeroStateIsAProblem: the zero value is the refusing arm.
func TestJudge_ZeroStateIsAProblem(t *testing.T) {
	f := newFixture(t)
	item, prob := Judge(f.repo, scan.Entry{Project: "proj", Name: "shared", Path: "/nowhere/shared.md"})
	if prob == nil {
		t.Fatalf("judged a StateInvalid entry as %v", item.Action)
	}
	if prob.Path != "/nowhere/shared.md" || !strings.Contains(prob.Err, "refusing") {
		t.Errorf("problem = %+v", prob)
	}
	if item.Action.Writes() {
		t.Errorf("action = %v, want no write", item.Action)
	}
}

// adoptableEntry returns the classified entry for an adoptable proj/shared,
// with the classifier's hash cleared as a hand-made entry would have it.
func adoptableEntry(t *testing.T, f *fixture) (scan.Result, int) {
	t.Helper()
	const body = "canonical shared agent\n"
	f.global("shared", body)
	f.project("proj", "shared", body)
	res := f.result()
	for i, e := range res.Entries {
		if e.State == scan.StateAdoptable {
			res.Entries[i].Hash = ""
			return res, i
		}
	}
	t.Fatalf("no adoptable entry in %+v", res.Entries)
	return res, -1
}

func TestJudge_HashesAnEntryThatCarriesNone(t *testing.T) {
	f := newFixture(t)
	res, i := adoptableEntry(t, f)
	want, err := scan.HashFile(res.Entries[i].Path)
	if err != nil {
		t.Fatal(err)
	}

	item, prob := Judge(f.repo, res.Entries[i])
	if prob != nil {
		t.Fatalf("problem: %+v", prob)
	}
	if item.Action != ActionAdopt || item.Hash != want {
		t.Errorf("action %v hash %q, want adopt with %q", item.Action, item.Hash, want)
	}
}

// TestNewPlan_UnhashableSourceIsAProblemNotAnItem: a file that could not be
// read is not a file anybody may replace.
func TestNewPlan_UnhashableSourceIsAProblemNotAnItem(t *testing.T) {
	f := newFixture(t)
	res, i := adoptableEntry(t, f)
	path := res.Entries[i].Path
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(f.repo, res, f.state, Options{})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if len(plan.Problems) != 1 || plan.Problems[0].Path != path || !strings.Contains(plan.Problems[0].Err, "hashing") {
		t.Fatalf("problems = %+v, want the unhashable file", plan.Problems)
	}
	if len(plan.Items) != 0 {
		t.Errorf("items = %v, want the file dropped", plan.Items)
	}
}
