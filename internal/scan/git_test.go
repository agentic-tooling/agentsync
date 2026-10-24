package scan

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// requireGit skips the test when git is not usable, so the suite still runs in
// a container without it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// Git helpers live in testfs alongside the breakage fixtures so every test
// uses the same isolated environment and fixture self-checks. Keeping that
// setup in one place prevents git-probe tests from drifting apart.

// initGitRepo makes a repository at dir with no commits yet.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	mustMkdir(t, dir)
	testfs.Git(t, dir, "init", "-q", "-b", "main")
}

// TestGitTracked_MeansCommittedNotStaged is the regression guard for the
// blocker: the check used `git ls-files --error-unmatch`, which answers "is
// this in the index". A file that was `git add`-ed but never committed passed
// that check, so a genuinely losable override was reported safe — the blob is
// unreachable after `git reset --hard` and collectable by `git gc`.
func TestGitTracked_MeansCommittedNotStaged(t *testing.T) {
	requireGit(t)

	repo := filepath.Join(t.TempDir(), "project")
	initGitRepo(t, repo)

	// Committed, at the root and in a subdirectory: the relative-path
	// handling has to be right in both.
	committed := filepath.Join(repo, "committed.md")
	mustWrite(t, committed, "safe\n")
	nested := filepath.Join(repo, ".claude", "agents", "nested.md")
	mustWrite(t, nested, "also safe\n")
	testfs.Git(t, repo, "add", "committed.md", ".claude/agents/nested.md")
	testfs.Git(t, repo, "commit", "-qm", "initial")

	staged := filepath.Join(repo, ".claude", "agents", "staged.md")
	mustWrite(t, staged, "added but never committed\n")
	testfs.Git(t, repo, "add", ".claude/agents/staged.md")

	untracked := filepath.Join(repo, ".claude", "agents", "untracked.md")
	mustWrite(t, untracked, "never added\n")

	// Committed, then edited in the working tree: the committed blob still
	// protects the name, so this stays "yes".
	modified := filepath.Join(repo, "committed.md")

	cases := []struct {
		name string
		path string
		want Tracked
	}{
		{"committed at repo root", committed, TrackedYes},
		{"committed in a subdirectory", nested, TrackedYes},
		{"staged but never committed", staged, TrackedNo},
		{"untracked", untracked, TrackedNo},
		{"committed then modified in the worktree", modified, TrackedYes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitTracked(tc.path); got != tc.want {
				t.Errorf("gitTracked(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestGitTracked_UnbornHead covers a freshly initialised repository. Nothing is
// committed, so nothing is protected — and that must read as a plain "no", not
// as an error that would surface as TrackedUnknown.
func TestGitTracked_UnbornHead(t *testing.T) {
	requireGit(t)

	repo := filepath.Join(t.TempDir(), "fresh")
	initGitRepo(t, repo)

	p := filepath.Join(repo, "agent.md")
	mustWrite(t, p, "no commits exist yet\n")

	if got := gitTracked(p); got != TrackedNo {
		t.Errorf("gitTracked in a repo with no commits = %v, want %v", got, TrackedNo)
	}

	// Staging it changes nothing: still no commit anywhere.
	testfs.Git(t, repo, "add", "agent.md")
	if got := gitTracked(p); got != TrackedNo {
		t.Errorf("gitTracked for a staged file in an unborn repo = %v, want %v", got, TrackedNo)
	}
}

// TestGitTracked_OutsideRepo confirms a file in no repository reads as
// untracked rather than unknown.
func TestGitTracked_OutsideRepo(t *testing.T) {
	requireGit(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "loose.md")
	mustWrite(t, p, "not in any repo\n")

	if got := gitTracked(p); got != TrackedNo {
		t.Errorf("gitTracked = %v, want %v", got, TrackedNo)
	}
}

// TestGitTracked_GitUnavailableIsUnknown proves the tri-state contract holds
// when git cannot be executed at all. TrackedUnknown, not TrackedNo — and
// Entry.AtRisk treats unknown as unprotected, so the warning stands.
func TestGitTracked_GitUnavailableIsUnknown(t *testing.T) {
	requireGit(t)

	// An empty PATH makes exec fail to find git, which is not an ExitError.
	t.Setenv("PATH", t.TempDir())

	repo := filepath.Join(t.TempDir(), "project")
	mustMkdir(t, repo)
	p := filepath.Join(repo, "agent.md")
	mustWrite(t, p, "content\n")

	if got := gitTracked(p); got != TrackedUnknown {
		t.Errorf("gitTracked with git unavailable = %v, want %v", got, TrackedUnknown)
	}
	if !(Entry{State: StateUnadopted, Tracked: TrackedUnknown}).AtRisk() {
		t.Error("an unknown tracking answer must not downgrade the warning")
	}
}

// TestGitProber_CachesRepoLookup checks the cache returns consistent answers
// and does not leak state between directories.
func TestGitProber_CachesRepoLookup(t *testing.T) {
	requireGit(t)

	base := t.TempDir()
	repo := filepath.Join(base, "project")
	initGitRepo(t, repo)
	committed := filepath.Join(repo, "a.md")
	mustWrite(t, committed, "x\n")
	testfs.Git(t, repo, "add", "a.md")
	testfs.Git(t, repo, "commit", "-qm", "one")
	untracked := filepath.Join(repo, "b.md")
	mustWrite(t, untracked, "y\n")

	outside := filepath.Join(base, "loose", "c.md")
	mustWrite(t, outside, "z\n")

	var g gitProber
	for i := 0; i < 3; i++ {
		if got := g.tracked(committed); got != TrackedYes {
			t.Fatalf("pass %d: committed = %v, want %v", i, got, TrackedYes)
		}
		if got := g.tracked(untracked); got != TrackedNo {
			t.Fatalf("pass %d: untracked = %v, want %v", i, got, TrackedNo)
		}
		if got := g.tracked(outside); got != TrackedNo {
			t.Fatalf("pass %d: outside any repo = %v, want %v", i, got, TrackedNo)
		}
	}
}

// TestScan_UsesCommitPresenceForRisk ties the git fix to the thing that
// actually matters: a staged-only override must still be reported at risk.
func TestScan_UsesCommitPresenceForRisk(t *testing.T) {
	requireGit(t)

	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	mustWrite(t, filepath.Join(repoRoot, "agents", "shared.md"), "canonical\n")

	project := filepath.Join(base, "proj")
	initGitRepo(t, project)
	agentsDir := filepath.Join(project, ".claude", "agents")

	// An override that is committed: safe.
	mustWrite(t, filepath.Join(agentsDir, "shared.md"), "customized\n")
	testfs.Git(t, project, "add", ".claude/agents/shared.md")
	testfs.Git(t, project, "commit", "-qm", "keep the override")

	// An unadopted file that is only staged: NOT safe.
	mustWrite(t, filepath.Join(agentsDir, "stagedonly.md"), "only in the index\n")
	testfs.Git(t, project, "add", ".claude/agents/stagedonly.md")

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byName := make(map[string]Entry, len(res.Entries))
	for _, e := range res.Entries {
		byName[e.Name] = e
	}

	shared, ok := byName["shared"]
	if !ok {
		t.Fatal("the committed override was not classified")
	}
	if shared.State != StateOverride || shared.AtRisk() {
		t.Errorf("committed override: state=%v atRisk=%v, want override and not at risk",
			shared.State, shared.AtRisk())
	}

	stagedOnly, ok := byName["stagedonly"]
	if !ok {
		t.Fatal("the staged-only file was not classified")
	}
	if !stagedOnly.AtRisk() {
		t.Error("a staged-but-uncommitted file is unreachable after a hard reset; it must be at risk")
	}
}

// TestInspectDir_ClassifiesTheDirectoryAWriteWouldTouch covers the check link
// makes before writing a symlink into a project.
//
// The question is not whether a file's content is protected — that is
// gitTracked — but whether a write would be visible to the project's own
// repository. A symlink into the agent repo names an absolute path, so one
// committed by accident hands every other clone a link into a directory that
// is not there.
func TestInspectDir_ClassifiesTheDirectoryAWriteWouldTouch(t *testing.T) {
	requireGit(t)

	t.Run("tracked", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		agents := filepath.Join(repo, ".claude", "agents")
		mustWrite(t, filepath.Join(agents, "committed.md"), "an agent\n")
		testfs.Git(t, repo, "add", ".")
		testfs.Git(t, repo, "commit", "-qm", "agents")

		got := InspectDir(agents)
		if got.Tracking != DirTracked {
			t.Errorf("tracking = %v, want %v", got.Tracking, DirTracked)
		}
		if got.Rel != ".claude/agents" {
			t.Errorf("rel = %q, want %q", got.Rel, ".claude/agents")
		}
		// The git directory is what a local exclusion is written into, so an
		// empty one means the caller cannot honour --exclude-local.
		if got.GitDir == "" {
			t.Error("no git directory was reported")
		}
	})

	t.Run("ignored", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		mustWrite(t, filepath.Join(repo, ".gitignore"), ".claude/\n")
		testfs.Git(t, repo, "add", ".")
		testfs.Git(t, repo, "commit", "-qm", "ignore .claude")

		agents := filepath.Join(repo, ".claude", "agents")
		mustMkdir(t, agents)
		if got := InspectDir(agents).Tracking; got != DirIgnored {
			t.Errorf("tracking = %v, want %v", got, DirIgnored)
		}
	})

	t.Run("visible but not tracked", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		mustWrite(t, filepath.Join(repo, "README.md"), "hello\n")
		testfs.Git(t, repo, "add", ".")
		testfs.Git(t, repo, "commit", "-qm", "readme")

		agents := filepath.Join(repo, ".claude", "agents")
		mustMkdir(t, agents)
		if got := InspectDir(agents).Tracking; got != DirVisible {
			t.Errorf("tracking = %v, want %v", got, DirVisible)
		}
	})

	t.Run("no repository", func(t *testing.T) {
		agents := filepath.Join(t.TempDir(), "project", ".claude", "agents")
		mustMkdir(t, agents)
		if got := InspectDir(agents).Tracking; got != DirNoRepo {
			t.Errorf("tracking = %v, want %v", got, DirNoRepo)
		}
	})

	// The directory link would create does not exist yet, which is exactly the
	// state-1 case. The answer has to be available before the directory is.
	t.Run("directory that does not exist yet", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		mustWrite(t, filepath.Join(repo, ".gitignore"), ".claude/\n")
		testfs.Git(t, repo, "add", ".")
		testfs.Git(t, repo, "commit", "-qm", "ignore .claude")

		agents := filepath.Join(repo, ".claude", "agents")
		got := InspectDir(agents)
		if got.Tracking != DirIgnored {
			t.Errorf("tracking = %v, want %v", got.Tracking, DirIgnored)
		}
		if got.Rel != ".claude/agents" {
			t.Errorf("rel = %q, want %q", got.Rel, ".claude/agents")
		}
	})

	// An unborn HEAD is a repository that has committed nothing. Collapsing
	// that into "could not ask" would refuse a freshly-initialised project for
	// no reason.
	t.Run("repository with no commits", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		agents := filepath.Join(repo, ".claude", "agents")
		mustMkdir(t, agents)
		if got := InspectDir(agents).Tracking; got != DirVisible {
			t.Errorf("tracking = %v, want %v", got, DirVisible)
		}
	})
}

// TestInspectDir_UnavailableGitIsNotACleanAnswer is the direction that matters
// on a write path: "we could not ask" must never read as "nothing is tracked",
// or the one condition that disables the check is also the one that lets the
// damaging write through.
func TestInspectDir_UnavailableGitIsNotACleanAnswer(t *testing.T) {
	requireGit(t)

	repo := filepath.Join(t.TempDir(), "project")
	initGitRepo(t, repo)
	agents := filepath.Join(repo, ".claude", "agents")
	mustMkdir(t, agents)

	t.Setenv("PATH", "")
	if got := InspectDir(agents).Tracking; got != DirUnknown {
		t.Errorf("tracking = %v, want %v when git cannot be run", got, DirUnknown)
	}
}

// TestInspectDir_FailedProbeIsNotACleanAnswer is the regression guard for the
// gate being silently off.
//
// PATH="" only exercises the arm where git cannot be executed. The arm that
// actually breaks is git running and exiting non-zero, because "not a git
// repository" and "I could not tell you" are the same exit code — so a failed
// probe fell through to DirNoRepo, the most permissive verdict there is, and a
// tracked directory linked with exit 0 and no opt-in.
//
// The realistic trigger is safe.directory refusing a dubiously-owned checkout:
// sudo, cron, containers, bind-mounted or shared clones. Those cannot be
// simulated portably, so each subtest reproduces the same shape — git exits
// non-zero while a repository demonstrably exists.
func TestInspectDir_FailedProbeIsNotACleanAnswer(t *testing.T) {
	requireGit(t)

	// A .git file pointing at a gitdir that is not there. rev-parse
	// --show-toplevel exits 128 and every ancestor walk still finds .git.
	t.Run("rev-parse fails with a repository present", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		agents := filepath.Join(project, ".claude", "agents")
		mustMkdir(t, agents)
		testfs.Git(t, project, "init", "-q", "-b", "main")
		testfs.BreakGitfile(t, project)

		if got := InspectDir(agents).Tracking; got != DirUnknown {
			t.Errorf("tracking = %v, want %v: a failed rev-parse must never read as %v",
				got, DirUnknown, DirNoRepo)
		}
	})

	// A repository whose HEAD commit object is gone. rev-parse --show-toplevel
	// and --verify HEAD both still succeed, so only ls-tree fails — and it
	// fails over a directory that is genuinely tracked.
	t.Run("ls-tree fails over a tracked directory", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)
		agents := filepath.Join(repo, ".claude", "agents")
		mustWrite(t, filepath.Join(agents, "committed.md"), "an agent\n")
		testfs.Git(t, repo, "add", ".")
		testfs.Git(t, repo, "commit", "-qm", "agents")

		if got := InspectDir(agents).Tracking; got != DirTracked {
			t.Fatalf("the fixture is not tracked to begin with: %v", got)
		}

		testfs.DeleteHeadCommit(t, repo)

		if got := InspectDir(agents).Tracking; got != DirUnknown {
			t.Errorf("tracking = %v, want %v: a failed ls-tree must not fall through to the ignore check",
				got, DirUnknown)
		}
	})
}

// TestCommittedInHEAD_IsAboutBytesNotThePath is the distinction the whole type
// exists for, and the one a path-only check gets wrong.
//
// A file committed as v1 and edited to v2 is tracked — its path is in HEAD —
// while v2 is in no commit at all. A write path that reads "tracked" as "safe
// to overwrite" destroys v2 permanently.
func TestCommittedInHEAD_IsAboutBytesNotThePath(t *testing.T) {
	requireGit(t)

	repo := filepath.Join(t.TempDir(), "project")
	initGitRepo(t, repo)

	committed := filepath.Join(repo, "committed.md")
	mustWrite(t, committed, "v1\n")
	nested := filepath.Join(repo, "sub", "nested.md")
	mustWrite(t, nested, "v1 nested\n")
	testfs.Git(t, repo, "add", "committed.md", "sub/nested.md")
	testfs.Git(t, repo, "commit", "-qm", "initial")

	modified := filepath.Join(repo, "modified.md")
	mustWrite(t, modified, "v1\n")
	testfs.Git(t, repo, "add", "modified.md")
	testfs.Git(t, repo, "commit", "-qm", "second")
	mustWrite(t, modified, "v2, in no commit\n")

	// Staged but not committed is the same story one step along: the blob is
	// unreachable after `git reset --hard`, so it is not protection.
	staged := filepath.Join(repo, "staged.md")
	mustWrite(t, staged, "added, never committed\n")
	testfs.Git(t, repo, "add", "staged.md")

	untracked := filepath.Join(repo, "untracked.md")
	mustWrite(t, untracked, "never added\n")

	cases := []struct {
		name string
		path string
		want Committed
	}{
		{"committed and unmodified", committed, CommittedYes},
		{"committed and unmodified, in a subdirectory", nested, CommittedYes},
		{"committed then edited in the worktree", modified, CommittedNo},
		{"staged but never committed", staged, CommittedNo},
		{"untracked", untracked, CommittedNo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommittedInHEAD(tc.path); got != tc.want {
				t.Errorf("CommittedInHEAD(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}

	// Tracked says yes to the two that are not recoverable, which is why the
	// write path cannot use it. Asserting the difference here keeps the two
	// answers from being quietly merged later.
	if got := gitTracked(modified); got != TrackedYes {
		t.Errorf("gitTracked(modified) = %v, want %v: the path is in HEAD even though the bytes are not", got, TrackedYes)
	}

	// Committing the edit is what makes it recoverable.
	testfs.Git(t, repo, "add", "modified.md")
	testfs.Git(t, repo, "commit", "-qm", "third")
	if got := CommittedInHEAD(modified); got != CommittedYes {
		t.Errorf("after committing, CommittedInHEAD = %v, want %v", got, CommittedYes)
	}
}

// TestCommittedInHEAD_FailsClosed covers the arms where git does not produce a
// clean yes. On a write path every one of them must refuse.
func TestCommittedInHEAD_FailsClosed(t *testing.T) {
	requireGit(t)

	t.Run("no repository at all", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "loose.md")
		mustWrite(t, p, "not in any repo\n")
		if got := CommittedInHEAD(p); got != CommittedNo {
			t.Errorf("CommittedInHEAD = %v, want %v", got, CommittedNo)
		}
	})

	t.Run("unborn HEAD", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "fresh")
		initGitRepo(t, repo)
		p := filepath.Join(repo, "agent.md")
		mustWrite(t, p, "no commits exist yet\n")
		if got := CommittedInHEAD(p); got != CommittedNo {
			t.Errorf("CommittedInHEAD in a repo with no commits = %v, want %v", got, CommittedNo)
		}
	})

	t.Run("git unavailable is unknown, never a no", func(t *testing.T) {
		// The two answers mean different things to a caller: one has a fix
		// the user can carry out and the other does not. An unavailable git
		// command must return unknown rather than claim the file is uncommitted.
		repo := filepath.Join(t.TempDir(), "project")
		mustMkdir(t, repo)
		p := filepath.Join(repo, "agent.md")
		mustWrite(t, p, "content\n")

		t.Setenv("PATH", t.TempDir())
		if got := CommittedInHEAD(p); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD with git unavailable = %v, want %v", got, CommittedUnknown)
		}
	})

	t.Run("the zero value is the refusing one", func(t *testing.T) {
		// A caller that forgets a case gets an answer that refuses rather
		// than one that permits.
		var c Committed
		if c != CommittedUnknown {
			t.Errorf("zero Committed = %v, want %v", c, CommittedUnknown)
		}
	})
}

// TestCommittedInHEAD_DamagedObjectStoreIsNeverAYes builds the one repository
// shape that separates "git says the bytes are safe" from "the bytes are
// actually there": HEAD names a blob that is no longer in the object store.
//
// It covers both halves of the hazard in one fixture, because they are the same
// damage seen from two sides:
//
//   - Working file unchanged. ls-tree finds the path, and `diff --quiet` exits
//     0 from the index stat cache without ever opening the blob. That was a
//     CommittedYes for content that cannot be recovered — a false positive in
//     the one direction the gate exists to prevent, and the reason
//     CommittedInHEAD now also asks `cat-file -e`.
//   - Working file modified. Now git must read the blob to compare, cannot,
//     and exits 128. That is git failing, not git answering "different", and it
//     must not collapse into CommittedNo — the exit-code arm this fixture
//     exists to reach.
//
// The recipe is deliberate rather than incidental: `ls-tree` reads only tree
// objects, so deleting the blob leaves it answering perfectly well, while every
// question about content has to go to the object that is gone. No permissions
// and no privilege are involved, so this runs identically for root and
// non-root users.
func TestCommittedInHEAD_DamagedObjectStoreIsNeverAYes(t *testing.T) {
	requireGit(t)

	// setup returns a repo with cap.md committed and its blob deleted.
	setup := func(t *testing.T) string {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "project")
		initGitRepo(t, repo)

		capPath := filepath.Join(repo, "cap.md")
		mustWrite(t, capPath, "v1\n")
		testfs.Git(t, repo, "add", "cap.md")
		testfs.Git(t, repo, "commit", "-qm", "initial")

		testfs.DeleteBlob(t, repo, "cap.md")

		// The premise, asserted rather than assumed: the path question is
		// still perfectly answerable. If a future git started reading the
		// blob here, this fixture would be testing something else.
		if got := gitTracked(capPath); got != TrackedYes {
			t.Fatalf("gitTracked = %v, want %v: deleting the blob was supposed to leave ls-tree working", got, TrackedYes)
		}
		return repo
	}

	t.Run("unmodified worktree, blob gone", func(t *testing.T) {
		repo := setup(t)
		if got := CommittedInHEAD(filepath.Join(repo, "cap.md")); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD = %v, want %v: the diff answered from the stat cache, so the missing blob has to be caught by asking for it",
				got, CommittedUnknown)
		}
	})

	t.Run("modified worktree, blob gone", func(t *testing.T) {
		repo := setup(t)
		capPath := filepath.Join(repo, "cap.md")
		mustWrite(t, capPath, "v2, and now git must read the blob to compare\n")
		if got := CommittedInHEAD(capPath); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD = %v, want %v: a diff that exits 128 is git failing, not git saying the file differs",
				got, CommittedUnknown)
		}
	})
}

// These regression guards ensure probes in probeGitRoot and gitProber.tracked
// distinguish failed git commands from clean negative answers.
//
// Every one of these passed before the fix, because every one of them was
// unreachable from a repository that works. That is the property of this bug
// class that makes it recur: the tests are green, the code is documented, and
// the defect only exists in the states nothing sets up. testfs's git fixtures
// exist to make setting them up a single line.
//
// Each case below asserts the *fixture* is healthy first, so a test that stops
// reproducing says so instead of passing because the arm it aims at was never
// entered.

// TestProbeGitRoot_RefusedRepoIsNotAbsentRepo covers the member with a
// user-visible symptom.
//
// git exits 128 both for "this is not a repository" and for "I will not talk
// about this repository". probeGitRoot read both as gitNo, which becomes
// TrackedNo — a definite claim that nothing is protecting the file. import
// --update's commit gate refuses on that by telling the user to commit
// project-agents/, i.e. to run git in the repository git has just declined to
// open. Nothing is destroyed, but the remedy cannot be carried out and points
// away from the cause.
func TestProbeGitRoot_RefusedRepoIsNotAbsentRepo(t *testing.T) {
	requireGit(t)

	t.Run("broken gitfile", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		testfs.GitRepo(t, project, "agent.md", "content\n")
		file := filepath.Join(project, "agent.md")

		if got := gitTracked(file); got != TrackedYes {
			t.Fatalf("the fixture is not tracked to begin with: %v", got)
		}

		testfs.BreakGitfile(t, project)

		if got := gitTracked(file); got != TrackedUnknown {
			t.Errorf("gitTracked = %v, want %v: a repository git refuses to open is not a repository with nothing committed",
				got, TrackedUnknown)
		}
		if got := CommittedInHEAD(file); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD = %v, want %v: %v routes import --update to the refusal that says \"commit project-agents/ first\", in a repo git will not open",
				got, CommittedUnknown, CommittedNo)
		}
	})

	// The realistic trigger, and the one the audit calls highest-value:
	// dubious ownership. sudo, cron, a container running as root over a
	// bind-mounted host checkout, a shared checkout on a build agent — all
	// make every git command exit 128 in a repository that is completely
	// healthy. This is the state in which the gate used to be silently off.
	t.Run("safe.directory dubious ownership", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		testfs.GitRepo(t, project, "agent.md", "content\n")
		file := filepath.Join(project, "agent.md")

		if got := gitTracked(file); got != TrackedYes {
			t.Fatalf("the fixture is not tracked to begin with: %v", got)
		}

		testfs.DubiousOwnership(t, project)

		if got := gitTracked(file); got != TrackedUnknown {
			t.Errorf("gitTracked under dubious ownership = %v, want %v: the repository is healthy and fully committed; git is refusing, not answering",
				got, TrackedUnknown)
		}
		if got := CommittedInHEAD(file); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD under dubious ownership = %v, want %v", got, CommittedUnknown)
		}
	})

	// The other half of the same guard: hasGitEntry must not turn a
	// genuine "no repository here" into an unknown. A directory with no
	// .git anywhere above it is a real negative and has to stay one, or
	// every project outside version control becomes unanswerable.
	t.Run("a genuinely absent repository stays a definite no", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "agent.md")
		mustWrite(t, file, "content\n")

		if got := gitTracked(file); got != TrackedNo {
			t.Errorf("gitTracked outside any repository = %v, want %v", got, TrackedNo)
		}
	})
}

// TestProbeGitRoot_UnbornHeadIsTheOnlyNo guards the second site, and it has to
// assert both halves of what its name claims: exit 1 from `rev-parse --verify
// --quiet HEAD` is "no commits yet", and any other non-zero is not.
//
// An earlier version of this test asserted only the exit-1 arm, on the belief
// that no repository could reach the other one — every structural corruption
// tried took `--show-toplevel` down with it, so the hasGitEntry guard fired
// first and --verify was never reached with a 128. That belief was wrong, and
// a corrupt .git/packed-refs is the counterexample: it splits the two probes
// in the same directory, leaving --show-toplevel answering 0 while --verify
// fails. So the arm was never untestable, only untested.
func TestProbeGitRoot_UnbornHeadIsTheOnlyNo(t *testing.T) {
	requireGit(t)

	// The reachable negative: staged but never committed, in a repository
	// with no commits at all. Exit 1 here is git answering, and it must stay
	// a definite TrackedNo rather than becoming unanswerable.
	t.Run("unborn HEAD is a definite no", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "project")
		mustMkdir(t, repo)
		testfs.Git(t, repo, "init", "-q", "-b", "main")

		file := filepath.Join(repo, "agent.md")
		mustWrite(t, file, "content\n")
		testfs.Git(t, repo, "add", "--", "agent.md")

		if got := gitTracked(file); got != TrackedNo {
			t.Errorf("gitTracked in a repo with an unborn HEAD = %v, want %v: exit 1 is a real answer and must not be read as a failure",
				got, TrackedNo)
		}
	})

	// The other half: a 128 from the same query is git failing, and nothing
	// upstream can catch it because --show-toplevel still exits 0. Read as
	// "unborn" it became TrackedNo, then CommittedNo, then import --update
	// telling the user to commit project-agents/ in a repository whose refs
	// git cannot parse. A corrupt refs file must be reported as a probe failure,
	// since committing cannot repair it.
	t.Run("a corrupt packed-refs is not an unborn HEAD", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		testfs.GitRepo(t, project, "agent.md", "content\n")
		file := filepath.Join(project, "agent.md")

		if got := gitTracked(file); got != TrackedYes {
			t.Fatalf("the fixture is not tracked to begin with: %v", got)
		}

		testfs.CorruptPackedRefs(t, project)

		if got := gitTracked(file); got != TrackedUnknown {
			t.Errorf("gitTracked with an unparseable packed-refs = %v, want %v: the repository has a commit, and git failing to read its refs is not the repository having none",
				got, TrackedUnknown)
		}
		if got := CommittedInHEAD(file); got != CommittedUnknown {
			t.Errorf("CommittedInHEAD = %v, want %v: %v routes import --update to \"commit project-agents/ first\", which cannot be carried out in a repo whose refs will not parse",
				got, CommittedUnknown, CommittedNo)
		}
	})

	// InspectDir asks the identical query, and the two sites are only worth
	// keeping identical if both are held to it. link's gate is the caller
	// here: DirVisible and DirIgnored both permit writing symlinks.
	t.Run("InspectDir agrees", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		testfs.GitRepo(t, project, filepath.Join(".claude", "agents", "a.md"), "an agent\n")
		agents := filepath.Join(project, ".claude", "agents")

		if got := InspectDir(agents).Tracking; got != DirTracked {
			t.Fatalf("the fixture is not tracked to begin with: %v", got)
		}

		testfs.CorruptPackedRefs(t, project)

		if got := InspectDir(agents).Tracking; got != DirUnknown {
			t.Errorf("InspectDir with an unparseable packed-refs = %v, want %v: falling through to the ignore check would let link write into a directory its own git tracks",
				got, DirUnknown)
		}
	})
}

// TestTracked_FailingLsTreeIsNotUntracked guards the third site.
//
// ls-tree is the sharpest case in the file because it has no negative exit at
// all: it exits 0 and prints nothing when a pathspec matches nothing, so the
// empty-output check is the whole of "not in HEAD" and a non-zero exit is
// never an answer. tracked() folded it into TrackedNo anyway, twelve lines
// below a comment in InspectDir saying why that is wrong.
//
// DeleteHeadCommit is the fixture that isolates it: --show-toplevel and
// --verify HEAD both still exit 0, so nothing before ls-tree gives any hint
// that the repository cannot answer.
func TestTracked_FailingLsTreeIsNotUntracked(t *testing.T) {
	requireGit(t)

	project := filepath.Join(t.TempDir(), "project")
	testfs.GitRepo(t, project, filepath.Join(".claude", "agents", "a.md"), "an agent\n")
	file := filepath.Join(project, ".claude", "agents", "a.md")

	if got := gitTracked(file); got != TrackedYes {
		t.Fatalf("the fixture is not tracked to begin with: %v", got)
	}

	testfs.DeleteHeadCommit(t, project)

	got := gitTracked(file)
	if got != TrackedUnknown {
		t.Errorf("gitTracked with the HEAD commit object gone = %v, want %v: ls-tree exiting 128 is git failing, and it prints nothing on a real miss",
			got, TrackedUnknown)
	}

	// The direction this was always safe in, pinned so a future change
	// cannot quietly reverse it: unknown is not tracked, so the file stays
	// at risk either way. The fix is about the reported reason being true,
	// not about the risk verdict.
	if !(Entry{State: StateOverride, Tracked: got}).AtRisk() {
		t.Error("an unanswerable tracking check must never downgrade the risk verdict")
	}

	// And the write path refuses, with the message that is actually true.
	if got := CommittedInHEAD(file); got != CommittedUnknown {
		t.Errorf("CommittedInHEAD = %v, want %v", got, CommittedUnknown)
	}
}

// TestInspectDir_DubiousOwnershipIsNotACleanAnswer extends the existing
// InspectDir guard to the trigger that made this class matter.
//
// The existing coverage uses a broken gitfile and a deleted commit object,
// both of which are damage. Dubious ownership is not damage: the repository is
// perfectly healthy and git refuses anyway, for reasons that have nothing to
// do with the repository and everything to do with who is asking. It is also
// the only one of the three that turns the gate off for *every* project at
// once rather than one.
func TestInspectDir_DubiousOwnershipIsNotACleanAnswer(t *testing.T) {
	requireGit(t)

	project := filepath.Join(t.TempDir(), "project")
	testfs.GitRepo(t, project, filepath.Join(".claude", "agents", "a.md"), "an agent\n")
	agents := filepath.Join(project, ".claude", "agents")

	if got := InspectDir(agents).Tracking; got != DirTracked {
		t.Fatalf("the fixture is not tracked to begin with: %v", got)
	}

	testfs.DubiousOwnership(t, project)

	if got := InspectDir(agents).Tracking; got != DirUnknown {
		t.Errorf("InspectDir under dubious ownership = %v, want %v: %v is the most permissive verdict in the switch, and reaching it here would disable link's gate for every project under sudo, cron, or a container",
			got, DirUnknown, DirNoRepo)
	}
}
