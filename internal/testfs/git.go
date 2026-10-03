package testfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git fixtures that force one arm of the three-way gitYes / gitNo /
// gitUnavailable answer that internal/scan is built on.
//
// Issue #12 is an audit of one bug class: a probe's "no" and its "could not
// answer" share a return value, and the caller reads the ambiguous value as
// the definite one. Seven members have now been found in this repo. Every one
// of them was invisible to the tests, because the tests only ever ran git
// against repositories that worked — and a probe that conflates "no" with
// "broken" is indistinguishable from a correct one until something is broken.
//
// The two fixtures the earlier fixes needed were non-obvious enough to be
// worth not re-deriving, and the third — dubious ownership — is the realistic
// trigger for the worst instance and had never been reproduced anywhere. They
// live here, together, so that covering a new probe costs one line rather than
// an afternoon of finding out which corruption makes git exit which code.
//
// The measured behaviour of git 2.50, which is what these encode:
//
//	fixture                     show-toplevel  verify HEAD  ls-tree  check-ignore
//	healthy repo                      0             0          0          1/0
//	no repository at all            128             -          -            -
//	unborn HEAD (init, no commit)     0             1          -            -
//	BreakGitfile                    128           128        128          128
//	DeleteHeadCommit                  0             0        128            1
//	DubiousOwnership                128           128        128          128
//
// Two things in that table are the whole point. "No repository at all" and
// both genuine breakages produce the *same* 128 from show-toplevel, so the
// exit code cannot separate them and only the filesystem can — which is what
// scan.hasGitEntry is for. And DeleteHeadCommit passes the first two probes
// and fails only the third, so a caller that trusts an early yes and then
// mis-reads a late failure has nothing to warn it.
//
// Every fixture below asserts that it actually reproduced. A fixture that
// silently stops working turns a regression test into a test that passes
// because nothing happens, which is the failure mode this package was created
// to eliminate — see the note in testfs.go about the five chmod-000 tests that
// skipped in CI.

// GitEnv is the environment every git command in a fixture must run under.
//
// A developer's global hooks, commit templates, gpg signing config, or
// init.defaultBranch must not decide whether an assertion passes. The two
// GIT_CONFIG_* variables point at files that do not exist, which git treats as
// empty rather than as an error.
//
// It is exported because a test that builds its own repository has to use the
// same environment as the helpers here, or the two will disagree about what a
// clean repository looks like.
func GitEnv(t testing.TB) []string {
	t.Helper()
	dir := t.TempDir()
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(dir, "gitconfig-system"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
}

// Git runs git in dir and fails the test if it does not exit 0.
func Git(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// GitOutput runs git in dir for its trimmed stdout.
func GitOutput(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// GitRepo creates a repository at dir holding one file, committed.
//
// It is the baseline the breakage helpers below start from: a repository every
// probe in internal/scan answers cleanly, so that when one of them stops
// answering cleanly the fixture is the only thing that changed.
func GitRepo(t testing.TB, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	Git(t, dir, "init", "-q", "-b", "main")

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	Git(t, dir, "add", "--", name)
	Git(t, dir, "commit", "-qm", "fixture")
}

// BreakGitfile replaces dir's .git with a gitfile naming a gitdir that is not
// there — the shape a moved worktree, a relocated submodule, or a half-deleted
// checkout leaves behind.
//
// It forces the arm that matters most: `rev-parse --show-toplevel` exits 128,
// exactly as it does for a directory that is in no repository at all, while a
// .git entry is still sitting there for an ancestor walk to find. That
// contradiction — git will not name a repository, and a repository is plainly
// present — is the only available signal that the 128 meant "I refuse" rather
// than "there is nothing here".
//
// dir must already be a repository; the caller gets no useful fixture from
// breaking something that was never working.
func BreakGitfile(t testing.TB, dir string) {
	t.Helper()

	gitPath := filepath.Join(dir, ".git")
	if _, err := os.Lstat(gitPath); err != nil {
		t.Fatalf("breaking %s: it has no .git to break: %v", dir, err)
	}
	if err := os.RemoveAll(gitPath); err != nil {
		t.Fatalf("removing %s: %v", gitPath, err)
	}
	// An absolute path under the test's own temp dir, so it names somewhere
	// that genuinely does not exist rather than somewhere that might.
	missing := filepath.Join(t.TempDir(), "gitdir-that-is-not-there")
	if err := os.WriteFile(gitPath, []byte("gitdir: "+missing+"\n"), 0o644); err != nil {
		t.Fatalf("writing the broken gitfile: %v", err)
	}

	assertGitExit(t, dir, 128, "rev-parse", "--show-toplevel")
}

// DeleteHeadCommit removes the commit object HEAD names, leaving every ref and
// the index intact.
//
// This is the quiet one. `rev-parse --show-toplevel` still exits 0, and so
// does `rev-parse --verify --quiet HEAD` — git resolves a ref to an object id
// without checking that the object is there. Only ls-tree, which has to read
// the commit to walk its tree, exits 128.
//
// So it forces an arm no earlier probe can warn about: a caller that has
// already been told "yes, a repository, with commits" then gets a failure from
// the one query whose non-zero exit is easiest to mistake for a clean negative
// — because ls-tree exits 0 and prints nothing when a pathspec matches
// nothing, so its "not found" and its "I failed" do not even share an exit
// code, and a caller that folds them together is discarding information git
// went to the trouble of giving it.
func DeleteHeadCommit(t testing.TB, dir string) {
	t.Helper()

	head := GitOutput(t, dir, "rev-parse", "HEAD")
	if len(head) < 3 {
		t.Fatalf("rev-parse HEAD in %s returned %q, which is not an object id", dir, head)
	}
	object := filepath.Join(dir, ".git", "objects", head[:2], head[2:])
	if err := os.Remove(object); err != nil {
		// A packed repository keeps no loose object here. The helper
		// creates repositories that have never been gc-ed, so this means
		// the caller built the fixture some other way, and the test would
		// otherwise assert against a repository that is perfectly fine.
		t.Fatalf("removing the HEAD commit object %s: %v (is this repository packed?)", object, err)
	}

	// The premise, asserted rather than assumed. If a future git started
	// verifying object presence in rev-parse, this fixture would quietly
	// become an ordinary broken repo that the earlier probes already catch,
	// and the test using it would no longer cover what it says it covers.
	assertGitExit(t, dir, 0, "rev-parse", "--show-toplevel")
	assertGitExit(t, dir, 0, "rev-parse", "--verify", "--quiet", "HEAD")
	assertGitExit(t, dir, 128, "ls-tree", "-r", "--name-only", "HEAD")
}

// DeleteBlob removes the blob HEAD holds for rel, leaving the commit and tree
// objects — and the working file — untouched.
//
// It forces the arm where the damage is in the object store rather than in the
// repository's structure. `ls-tree` still exits 0, so the *path* question is
// perfectly answerable and every probe that asks it gets a clean yes; only a
// query that needs the bytes can notice anything is wrong.
//
// The deliberate omission is the important part, and it is why this does not
// also modify the working file. `git diff` is *defined* to answer from the
// index stat cache: while the working file's stat data still matches the index
// entry, git reports "unchanged" from the recorded object id without opening
// the object at all. Invalidating the stat data does not help — git then
// re-hashes the working file, gets the same id, and again concludes
// "unchanged" without a read. So a fixture that only deletes the blob leaves a
// repository where the content question *cannot* be answered by diffing, which
// is exactly the state scan.CommittedInHEAD's third probe exists to catch.
// A caller that wants the other state, where the content genuinely differs and
// git is forced to read the missing blob to compare, writes different bytes to
// the file itself; doing it here would deny every caller the first state.
//
// It also pins one measured fact the production code depends on: `cat-file -e`
// on a missing blob exits **1**, not 128. Everywhere else in internal/scan a 1
// is git's documented negative, and reading it as one here would turn a
// damaged repository into a clean "not committed". If a future git changes
// that exit, this fails rather than letting the assumption rot.
func DeleteBlob(t testing.TB, dir, rel string) {
	t.Helper()

	blob := GitOutput(t, dir, "rev-parse", "HEAD:"+rel)
	if len(blob) < 3 {
		t.Fatalf("rev-parse HEAD:%s in %s returned %q, which is not an object id", rel, dir, blob)
	}
	object := filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])
	if err := os.Remove(object); err != nil {
		t.Fatalf("removing the blob for %s (%s): %v (is this repository packed?)", rel, object, err)
	}

	// The premise: the path is still answerable, and only the object is
	// gone. If ls-tree started failing here the fixture would have become
	// DeleteHeadCommit by another route, and a caller aiming at the object
	// store would be testing the repository's structure instead.
	assertGitExit(t, dir, 0, "ls-tree", "-r", "--name-only", "HEAD", "--", rel)
	assertGitExit(t, dir, 1, "cat-file", "-e", "HEAD:"+rel)
}

// CorruptPackedRefs packs the repository's refs and then overwrites
// .git/packed-refs with a line git cannot parse.
//
// It is the only known corruption that splits `rev-parse --show-toplevel` from
// `rev-parse --verify HEAD` in the *same* directory:
//
//	rev-parse --show-toplevel       -> 0
//	rev-parse --verify --quiet HEAD -> 128
//
// That combination is what makes it worth having. Every other way of breaking
// a repository's structure — a gitfile pointing nowhere, a garbage HEAD, a
// missing object store, dubious ownership — takes --show-toplevel down with
// it, so scan's hasGitEntry guard fires first and the --verify probe is never
// reached with a 128. Here --show-toplevel answers cleanly, the guard stays
// silent, and --verify is the first thing to fail. A caller that reads its
// non-zero exit as "unborn HEAD, nothing committed" reports a repository with
// unreadable refs as one with nothing in it.
//
// It is also not an exotic state to be in. `git pack-refs --all` is ordinary
// maintenance and `git gc` runs it on its own schedule, so having a
// packed-refs file at all is the normal condition of any repository that has
// been in use for a while; only its contents are damaged here.
func CorruptPackedRefs(t testing.TB, dir string) {
	t.Helper()

	Git(t, dir, "pack-refs", "--all")

	packed := filepath.Join(dir, ".git", "packed-refs")
	if _, err := os.Stat(packed); err != nil {
		// Without a packed-refs file there is nothing to corrupt, and the
		// fixture would silently degrade into an ordinary healthy repo.
		t.Fatalf("pack-refs produced no %s: %v", packed, err)
	}
	if err := os.WriteFile(packed, []byte("this is not a valid packed-refs line\n"), 0o644); err != nil {
		t.Fatalf("corrupting %s: %v", packed, err)
	}

	// The split, asserted rather than assumed. If a future git started
	// reporting this through --show-toplevel too, the fixture would collapse
	// into BreakGitfile and the test using it would be covering the
	// hasGitEntry guard instead of the probe below it.
	assertGitExit(t, dir, 0, "rev-parse", "--show-toplevel")
	assertGitExit(t, dir, 128, "rev-parse", "--verify", "--quiet", "HEAD")
}

// DubiousOwnership makes git refuse every command in every repository, for the
// rest of the test, with the same fatal it produces when a checkout is owned
// by another user.
//
// This is the fixture issue #12 singles out as the highest-value one and the
// realistic trigger for the worst instance of the bug class. It is not exotic:
// git refuses a repository whose worktree is owned by someone other than the
// caller, which is what sudo, cron, a container running as root over a
// bind-mounted host checkout, and a shared checkout on a build agent all
// produce. Every probe then exits 128 in a repository that is completely
// healthy — so a caller that reads 128 as "no" has its gate silently switched
// off in precisely the environments a gate is for.
//
// Actually chown-ing the fixture would need root, and would then be inert for
// the root that CI runs as, which is the trap testfs exists to avoid: the test
// would pass locally and cover nothing in the pipeline. GIT_TEST_ASSUME_DIFFERENT_OWNER
// is git's own hook for this, added alongside the CVE-2022-24765 fix and used
// by git's test suite, and it takes the same branch the ownership check takes
// — so it behaves identically for root and non-root, which is the property
// that matters here.
//
// It is an undocumented test hook, so its presence is verified rather than
// assumed: if a future git drops it, this fails loudly instead of leaving
// every caller asserting against a working repository.
//
// dir must be a healthy repository, which is what the verification needs in
// order to mean anything: a 128 from a directory that was already broken would
// prove nothing about ownership.
func DubiousOwnership(t testing.TB, dir string) {
	t.Helper()

	// Asserted before the variable is set, so a fixture that was never
	// healthy is reported as such rather than as a missing git hook.
	assertGitExit(t, dir, 0, "rev-parse", "--show-toplevel")

	// t.Setenv, so it is restored at the end of the test and cannot leak
	// into another. It also marks the test as non-parallel, which is
	// correct: this changes the behaviour of every git in the process.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	// Deliberately NOT run under GitEnv, unlike every other check in this
	// file. The code under test is scan.runGitCode, which sets no Env and
	// therefore inherits the developer's real gitconfig — so the fixture has
	// to be verified in the same conditions the production probe will meet,
	// or it could confirm a refusal the probe never sees.
	//
	// The consequence is that this one check is sensitive to the developer's
	// own config, and exactly one setting defeats it: a global
	// `safe.directory = *` suppresses the ownership check outright and the
	// hook then exits 0 (measured). That is a real configuration people have.
	// It is named in the failure below so the message does not blame the git
	// version for something the machine is doing.
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("GIT_TEST_ASSUME_DIFFERENT_OWNER did not take effect: %s still answers rev-parse cleanly, so the fixture reproduces nothing and any test using it would pass vacuously.\n"+
			"Two known causes:\n"+
			"  1. this machine's git config sets safe.directory = * (check: git config --global --get-all safe.directory), which suppresses the ownership check the hook drives;\n"+
			"  2. this git (%s) no longer honours the hook.\n"+
			"The first is a local setting and the second is a code change; they need different fixes.",
			dir, gitVersion(t))
	}
	if code := cmd.ProcessState.ExitCode(); code != 128 {
		t.Fatalf("git exited %d under GIT_TEST_ASSUME_DIFFERENT_OWNER, want 128; the fixture is not producing the refusal it claims to.\n%s", code, out)
	}
	// The exit code alone would also match "not a repository", which is a
	// different fact and the one this fixture must not be confused with.
	if !strings.Contains(string(out), "dubious ownership") {
		t.Fatalf("git exited 128 under GIT_TEST_ASSUME_DIFFERENT_OWNER but not for ownership; the fixture is reproducing something else:\n%s", out)
	}
}

// assertGitExit fails the test unless git exits with exactly code.
//
// Every fixture here checks its own premise through this, because each one
// depends on undocumented details of which git command notices which kind of
// damage. Those details are stable in practice and guaranteed nowhere, and a
// fixture that quietly stops reproducing leaves a test that passes for the
// wrong reason — worse than no test, because it reads as coverage.
func assertGitExit(t testing.TB, dir string, want int, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv(t)
	out, err := cmd.CombinedOutput()

	got := 0
	if err != nil {
		if cmd.ProcessState == nil {
			t.Fatalf("git %v in %s could not be run at all: %v", args, dir, err)
		}
		got = cmd.ProcessState.ExitCode()
	}
	if got != want {
		t.Fatalf("fixture check: git %v in %s exited %d, want %d.\n"+
			"The fixture is not reproducing what it claims, so a test built on it would assert against the wrong repository state.\ngit %s said:\n%s",
			args, dir, got, want, gitVersion(t), out)
	}
}

func gitVersion(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return "(version unavailable)"
	}
	return strings.TrimSpace(string(out))
}
