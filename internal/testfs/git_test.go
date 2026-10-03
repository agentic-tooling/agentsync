package testfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Self-tests for the git fixtures.
//
// A fixture is only worth what its claims are worth. Each helper below asserts
// its own premise at construction time, which protects the *caller* — but
// nothing yet protects the assertion itself, and these fixtures encode
// undocumented details of which git command notices which kind of damage.
// These tests pin the table in git.go's package comment against the git that
// is actually installed, so a version that reshuffles those exit codes fails
// here, once, with a message naming the change — rather than in whichever
// downstream test happens to run first, as a confusing assertion about
// Tracked.
//
// This is the same reason TestUnreadableSubtree_IsUnreadableAndStopsTheWalk
// exists next door.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// exitOf runs git and reports its exit code, or -1 if it could not be run.
func exitOf(t *testing.T, dir string, args ...string) int {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv(t)
	if err := cmd.Run(); err != nil {
		if cmd.ProcessState == nil {
			return -1
		}
		return cmd.ProcessState.ExitCode()
	}
	return 0
}

// TestGitRepo_IsHealthy is the baseline every breakage fixture starts from. If
// this is not clean, none of the tests below prove what they say, because they
// all work by contrast with it.
func TestGitRepo_IsHealthy(t *testing.T) {
	requireGit(t)

	dir := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, dir, filepath.Join("sub", "a.md"), "content\n")

	for _, tc := range []struct {
		want int
		args []string
	}{
		{0, []string{"rev-parse", "--show-toplevel"}},
		{0, []string{"rev-parse", "--verify", "--quiet", "HEAD"}},
		{0, []string{"ls-tree", "-r", "--name-only", "HEAD", "--", "sub/a.md"}},
		{0, []string{"diff", "--quiet", "HEAD", "--", "sub/a.md"}},
		{0, []string{"cat-file", "-e", "HEAD:sub/a.md"}},
	} {
		if got := exitOf(t, dir, tc.args...); got != tc.want {
			t.Errorf("git %v in a healthy fixture exited %d, want %d", tc.args, got, tc.want)
		}
	}

	// ls-tree's defining property, and the one gitProber.tracked was fixed
	// to respect: a pathspec that matches nothing is exit 0 and no output,
	// not a non-zero exit. If this ever changes, "not in HEAD" and "git
	// failed" would become genuinely indistinguishable and the fix in
	// internal/scan would need rethinking rather than merely retesting.
	if got := exitOf(t, dir, "ls-tree", "-r", "--name-only", "HEAD", "--", "nosuch.md"); got != 0 {
		t.Errorf("ls-tree over a pathspec matching nothing exited %d, want 0: it has no negative exit, which is what makes any non-zero a failure", got)
	}
	if out := GitOutput(t, dir, "ls-tree", "-r", "--name-only", "HEAD", "--", "nosuch.md"); out != "" {
		t.Errorf("ls-tree over a pathspec matching nothing printed %q, want nothing", out)
	}
}

// TestBreakGitfile_IsIndistinguishableFromNoRepoByExitCode is the fact the
// whole hasGitEntry guard exists for. If git ever gave these two states
// different exit codes, internal/scan could read the code instead of consulting
// the filesystem, and this test is where that would be noticed.
func TestBreakGitfile_IsIndistinguishableFromNoRepoByExitCode(t *testing.T) {
	requireGit(t)

	broken := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, broken, "a.md", "content\n")
	BreakGitfile(t, broken)

	// A directory in no repository at all. Under a temp dir, which is not
	// itself inside one.
	absent := t.TempDir()

	brokenExit := exitOf(t, broken, "rev-parse", "--show-toplevel")
	absentExit := exitOf(t, absent, "rev-parse", "--show-toplevel")

	if brokenExit != 128 || absentExit != 128 {
		t.Fatalf("show-toplevel exited %d for a broken gitfile and %d for no repository, want 128 for both", brokenExit, absentExit)
	}
	if brokenExit != absentExit {
		t.Errorf("the two states are now distinguishable by exit code (%d vs %d); internal/scan's hasGitEntry guard could be replaced by reading the code",
			brokenExit, absentExit)
	}

	// And the filesystem signal the guard actually uses: .git is still
	// present in the broken case and absent in the other. That contrast is
	// the entire disambiguator.
	if _, err := os.Lstat(filepath.Join(broken, ".git")); err != nil {
		t.Errorf("the broken fixture has no .git entry left, so hasGitEntry would not fire: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(absent, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the no-repository fixture has a .git entry, so it is not the contrast case: %v", err)
	}
}

// TestDeleteHeadCommit_IsInvisibleUntilLsTree pins the property that makes this
// fixture worth having: the two probes that run *before* ls-tree still answer
// cleanly, so nothing warns the caller that the repository cannot answer.
func TestDeleteHeadCommit_IsInvisibleUntilLsTree(t *testing.T) {
	requireGit(t)

	dir := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, dir, "a.md", "content\n")
	DeleteHeadCommit(t, dir)

	if got := exitOf(t, dir, "rev-parse", "--show-toplevel"); got != 0 {
		t.Errorf("show-toplevel exited %d, want 0: the fixture is supposed to be invisible to this probe", got)
	}
	if got := exitOf(t, dir, "rev-parse", "--verify", "--quiet", "HEAD"); got != 0 {
		t.Errorf("rev-parse --verify HEAD exited %d, want 0: git resolves a ref without checking the object is there, and that is what makes this fixture isolate ls-tree", got)
	}
	if got := exitOf(t, dir, "ls-tree", "-r", "--name-only", "HEAD"); got != 128 {
		t.Errorf("ls-tree exited %d, want 128", got)
	}
}

// TestDeleteBlob_LeavesThePathAnswerable pins the two facts internal/scan's
// third probe rests on: ls-tree still succeeds, so the path question is clean;
// and `cat-file -e` reports a missing blob with exit **1**, not 128.
//
// The exit code is the load-bearing one. Everywhere else in internal/scan a 1
// is git's documented negative, so a sweep applying that rule uniformly would
// turn a damaged object store into a clean "not committed" — a definite answer
// authorising a write over content that cannot be recovered.
func TestDeleteBlob_LeavesThePathAnswerable(t *testing.T) {
	requireGit(t)

	dir := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, dir, "a.md", "v1\n")
	DeleteBlob(t, dir, "a.md")

	if got := exitOf(t, dir, "ls-tree", "-r", "--name-only", "HEAD", "--", "a.md"); got != 0 {
		t.Errorf("ls-tree exited %d, want 0: the fixture must leave the path question answerable, or it is DeleteHeadCommit by another route", got)
	}
	if got := exitOf(t, dir, "cat-file", "-e", "HEAD:a.md"); got != 1 {
		t.Errorf("cat-file -e on a missing blob exited %d, want 1. internal/scan deliberately does NOT apply its exit-1-is-a-no rule to this probe; if this exit changes, that decision needs revisiting", got)
	}

	// The worktree is untouched, which is what lets a caller reach the
	// state where `git diff` answers from the index stat cache without
	// opening the object at all.
	b, err := os.ReadFile(filepath.Join(dir, "a.md"))
	if err != nil || string(b) != "v1\n" {
		t.Errorf("the working file is %q (err %v), want %q: DeleteBlob must not modify the worktree", b, err, "v1\n")
	}
	if got := exitOf(t, dir, "diff", "--quiet", "HEAD", "--", "a.md"); got != 0 {
		t.Errorf("diff --quiet exited %d, want 0: with the stat cache intact git answers \"unchanged\" without reading the object, which is exactly the blind spot the separate cat-file probe covers", got)
	}
}

// TestDubiousOwnership_RefusesAHealthyRepository is the highest-value fixture
// and the most fragile, because GIT_TEST_ASSUME_DIFFERENT_OWNER is an
// undocumented hook. This is where its disappearance gets reported.
//
// It is also the only fixture whose repository is not damaged at all: the
// refusal is about who is asking, not about what is there. That is what makes
// it the realistic trigger — sudo, cron, a container over a bind mount, a
// shared checkout — and what makes reading its 128 as "no" switch the gate off
// for every project at once.
func TestDubiousOwnership_RefusesAHealthyRepository(t *testing.T) {
	requireGit(t)

	dir := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, dir, "a.md", "content\n")
	DubiousOwnership(t, dir)

	for _, args := range [][]string{
		{"rev-parse", "--show-toplevel"},
		{"rev-parse", "--verify", "--quiet", "HEAD"},
		{"ls-tree", "-r", "--name-only", "HEAD"},
		{"check-ignore", "-q", "--", "a.md"},
	} {
		if got := exitOf(t, dir, args...); got != 128 {
			t.Errorf("git %v exited %d under dubious ownership, want 128: every probe has to refuse, or the fixture only covers some of them", args, got)
		}
	}

	// The repository itself is fine. Proving that is the point: the state
	// is not damage, so nothing about it justifies a "no".
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	cmd.Env = append(GitEnv(t), "GIT_TEST_ASSUME_DIFFERENT_OWNER=")
	if err := cmd.Run(); err != nil {
		t.Errorf("with the hook cleared the repository still fails, so it was not healthy to begin with: %v", err)
	}
}

// TestUnreadableAgentsDir_IsFoundByTheWalkButNotReadable pins the three
// properties that make this fixture different from UnreadableSubtree, and the
// reason it had to be tuned rather than merely made long.
func TestUnreadableAgentsDir_IsFoundByTheWalkButNotReadable(t *testing.T) {
	agents := UnreadableAgentsDir(t, t.TempDir())
	claude := filepath.Dir(agents)

	// One: the parent is readable, so a walk gets far enough to identify
	// the agents directory from its dirent. UnreadableSubtree fails here
	// instead, in the walk's own error branch, which is why it cannot cover
	// this gap.
	ents, err := os.ReadDir(claude)
	if err != nil {
		t.Fatalf("the .claude directory must stay readable: %v", err)
	}
	var sawAgents bool
	for _, e := range ents {
		if e.Name() == "agents" && e.IsDir() {
			sawAgents = true
		}
	}
	if !sawAgents {
		t.Fatalf("%s reports no \"agents\" directory", claude)
	}

	// Two: the agents directory itself cannot be read, and for the reason
	// that has no privileged bypass. A permission bit would be inert for
	// the root CI runs as, which is the trap this package exists to avoid.
	_, err = os.ReadDir(agents)
	if err == nil {
		t.Fatalf("a %d-byte path opened; the fixture reproduces nothing", len(agents))
	}
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("unreadable for the wrong reason, so the fixture proves nothing about root: %v", err)
	}

	// Three: the seven-byte window was actually hit. If .claude were also
	// unreadable this would be UnreadableSubtree with extra steps, and the
	// test above would be covering the walk's error branch rather than the
	// read that follows a successful walk.
	if len(agents)-len(claude) != len("/agents") {
		t.Fatalf("agents is not directly under claude: %q vs %q", agents, claude)
	}
}

// TestLongestPad_RefusesRatherThanGuesses covers the tuning search's failure
// arm. It has to return "not found" rather than a pad that satisfies only half
// the window, because half a window is a fixture that silently does not
// reproduce.
func TestLongestPad_RefusesRatherThanGuesses(t *testing.T) {
	// A short base leaves the whole pad range comfortably inside PATH_MAX,
	// so no pad can put agents outside it. The correct answer is "no".
	if pad, ok := longestPad(t.TempDir()); ok {
		t.Errorf("longestPad on a short base returned %q; there is no window there and reporting one would build a readable directory the caller believes is unreadable", pad)
	}

	// And the tuned result really is a boundary: one byte more and .claude
	// itself goes out of reach.
	//
	// The base is grown until the *longest* permitted pad already overshoots,
	// which is the condition that guarantees a window exists somewhere in the
	// pad range — rather than to a fixed length, which would depend on this
	// platform's PATH_MAX and on how long the temp dir happens to be. The
	// earlier version of this test picked 700 bytes and skipped on macOS,
	// where the limit is 1024; a skip here is a silent pass, which is the
	// failure mode this package was created to eliminate.
	//
	// Nothing is created. tooLong is a pure length test — the kernel rejects
	// an over-long path in its resolver before consulting any inode — which
	// is what lets the search run without building a directory tree.
	deep := t.TempDir()
	for i := 0; ; i++ {
		if tooLong(filepath.Join(deep, strings.Repeat("p", padMax), ".claude")) {
			break
		}
		if i >= maxDepth {
			t.Fatalf("a base of %d bytes plus the longest pad still fits; this platform's PATH_MAX is larger than the fixture assumes", len(deep))
		}
		deep = filepath.Join(deep, strings.Repeat("d", 100))
	}

	pad, ok := longestPad(deep)
	if !ok {
		t.Fatalf("no window from a %d-byte base, but the longest pad was just shown to overshoot it, so one must exist", len(deep))
	}
	if tooLong(filepath.Join(deep, pad, ".claude")) {
		t.Errorf("the winning pad already puts .claude out of reach")
	}
	if !tooLong(filepath.Join(deep, pad+"p", ".claude")) {
		t.Errorf("one byte more still fits, so the search did not find the longest pad")
	}
	if !tooLong(filepath.Join(deep, pad, ".claude", "agents")) {
		t.Errorf("agents is still reachable, so the window was not hit")
	}
}

// TestCorruptPackedRefs_SplitsTheTwoRevParseProbes pins the property that makes
// this fixture the only way to reach one arm of internal/scan's probeGitRoot.
//
// Every other structural corruption takes `--show-toplevel` down with it, so
// scan's hasGitEntry guard fires first and the `--verify HEAD` probe below is
// never reached with a 128. Here --show-toplevel answers cleanly and --verify
// is the first thing to fail. If a future git ever reported this through
// --show-toplevel too, the fixture would silently collapse into BreakGitfile
// and the regression test resting on it would start covering the guard instead
// of the probe — passing, and proving nothing.
func TestCorruptPackedRefs_SplitsTheTwoRevParseProbes(t *testing.T) {
	requireGit(t)

	dir := filepath.Join(t.TempDir(), "repo")
	GitRepo(t, dir, "a.md", "content\n")

	// The contrast: before corruption both probes agree.
	if got := exitOf(t, dir, "rev-parse", "--verify", "--quiet", "HEAD"); got != 0 {
		t.Fatalf("the fixture is not healthy to begin with: --verify exited %d", got)
	}

	CorruptPackedRefs(t, dir)

	if got := exitOf(t, dir, "rev-parse", "--show-toplevel"); got != 0 {
		t.Errorf("show-toplevel exited %d, want 0: the whole point of this fixture is that the probe above --verify still answers, so scan's hasGitEntry guard does not fire", got)
	}
	if got := exitOf(t, dir, "rev-parse", "--verify", "--quiet", "HEAD"); got != 128 {
		t.Errorf("rev-parse --verify HEAD exited %d, want 128", got)
	}

	// 128 and not 1 is the load-bearing half. Exit 1 is git's documented
	// "unborn HEAD" and internal/scan reads it as a real negative; if this
	// corruption produced a 1, the fix it guards would be wrong rather than
	// merely untested.
	if got := exitOf(t, dir, "rev-parse", "--verify", "--quiet", "HEAD"); got == 1 {
		t.Error("an unparseable packed-refs reports as git's documented negative, which internal/scan reads as \"no commits yet\"; the exit code can no longer separate the two")
	}
}
