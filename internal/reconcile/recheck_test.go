package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// Most tests here build a real plan, change the disk between planning and
// Apply, and check that the op noticed. Unreadable paths are made by putting a
// file where a directory belongs (ENOTDIR), or a path too long to open, neither
// of which root can bypass as it does permission bits.

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
}

// replaceWithFile swaps a directory for a regular file, so any path through it
// fails with ENOTDIR rather than ENOENT.
func replaceWithFile(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
	mustWrite(t, path, "a file where a directory was\n")
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return target
}

func oneRefusal(t *testing.T, errs []error, mentions string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), mentions) {
		t.Errorf("refusal %q does not mention %q", errs[0], mentions)
	}
}

// brokenRecordedLink plants a created record for a link whose target is gone,
// which is what --prune authorises removing.
func brokenRecordedLink(t *testing.T, st *state.File, project, path, target string) {
	t.Helper()
	mustWrite(t, target, "body\n")
	hash := mustHash(t, target)
	mustLink(t, target, path)
	mustRemove(t, target)
	recordCreated(t, st, project, strings.TrimSuffix(filepath.Base(path), ".md"), path, target, hash)
}

// TestApply_RefusesWithoutARepoOrAState: nothing it changed could be recorded.
func TestApply_RefusesWithoutARepoOrAState(t *testing.T) {
	_, repo := fixture(t)
	if _, errs := (Plan{}).Apply(nil, state.New()); len(errs) != 1 {
		t.Errorf("nil repo: errs = %v, want one", errs)
	}
	if _, errs := (Plan{}).Apply(repo, nil); len(errs) != 1 {
		t.Errorf("nil state: errs = %v, want one", errs)
	}
}

// --- prune ------------------------------------------------------------------

// TestApply_PruneRecheckLeavesTheLinkAlone: a broken link that stopped
// matching the plan is kept, along with the record that describes it.
func TestApply_PruneRecheckLeavesTheLinkAlone(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name     string
		disturb  func(t *testing.T, repo *scan.Repo, path string)
		mentions string
	}{
		{
			// Still dead and still in the repo, so only the target comparison
			// stands between this link and removal.
			name: "re-pointed at another missing file in the repo",
			disturb: func(t *testing.T, repo *scan.Repo, path string) {
				mustRemove(t, path)
				mustLink(t, filepath.Join(repo.Root, "agents", "other.md"), path)
			},
			mentions: "somebody re-pointed",
		},
		{
			// Lstat of the target now fails with ENOTDIR, an error that is not
			// "it is gone" and so must not be read as still broken.
			name: "the target's directory became a file",
			disturb: func(t *testing.T, repo *scan.Repo, path string) {
				replaceWithFile(t, filepath.Join(repo.Root, "agents"))
			},
			mentions: "re-checking the target",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			path := agentPath(base, project, "a")
			st := state.New()
			brokenRecordedLink(t, st, project, path, filepath.Join(repo.Root, "agents", "a.md"))

			plan := planFor(t, base, repo, st, Options{Prune: true})
			if got := itemFor(t, plan, project, "a").Op; got != OpPrune {
				t.Fatalf("op = %s, want %s", got, OpPrune)
			}
			tc.disturb(t, repo, path)

			done, errs := plan.Apply(repo, st)
			oneRefusal(t, errs, tc.mentions)
			if len(done.Pruned) != 0 {
				t.Errorf("pruned %v despite the refusal", done.Pruned)
			}
			assertIsLink(t, path)
			if _, ok := st.LookupLink(path); !ok {
				t.Error("the record was dropped for a link that was not removed")
			}
		})
	}
}

// TestApply_PruneRefusesALinkSpelledOutsideTheRepo: the planner owns a dangling
// link by its resolved parent, the recheck by the literal target, so a link
// spelled through an alias of the repo root is planned as a prune and refused
// at apply.
func TestApply_PruneRefusesALinkSpelledOutsideTheRepo(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)
	alias := filepath.Join(base, "repolink")
	mustLink(t, repo.Root, alias)

	// The record keeps the alias spelling only because the target's directory
	// did not exist when it was written; state resolves the parent otherwise.
	target := filepath.Join(alias, "later", "a.md")
	path := agentPath(base, project, "a")
	mustLink(t, target, path)
	st := state.New()
	recordCreated(t, st, project, "a", path, target, strings.Repeat("0", 64))
	mustMkdir(t, filepath.Join(repo.Root, "later"))

	plan := planFor(t, base, repo, st, Options{Prune: true})
	if item := itemFor(t, plan, project, "a"); item.Op != OpPrune {
		t.Fatalf("op = %s, want %s; without a planned prune this proves nothing (%s)", item.Op, OpPrune, item.Reason)
	}

	done, errs := plan.Apply(repo, st)
	oneRefusal(t, errs, "points outside this repo")
	if len(done.Pruned) != 0 {
		t.Errorf("pruned %v despite the refusal", done.Pruned)
	}
	assertIsLink(t, path)
}

// TestApply_PruneReportsALinkWhoseRecordVanished: the link goes first, so a
// record that disappeared before apply leaves a removed link that must be
// reported rather than counted as pruned.
func TestApply_PruneReportsALinkWhoseRecordVanished(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)
	path := agentPath(base, project, "a")
	st := state.New()
	brokenRecordedLink(t, st, project, path, filepath.Join(repo.Root, "agents", "a.md"))

	plan := planFor(t, base, repo, st, Options{Prune: true})
	if !st.Remove(path) {
		t.Fatal("the record was not there to remove")
	}

	done, errs := plan.Apply(repo, st)
	oneRefusal(t, errs, "no longer accounted for")
	if len(done.Pruned) != 0 {
		t.Errorf("reported %v as pruned with no record behind it", done.Pruned)
	}
	assertGone(t, path, "the link is removed before the record is consulted")
}

// --- decline ----------------------------------------------------------------

// TestApply_DeclineRefusesWhenItCannotLook: a path that cannot be examined is
// not a path known to be empty.
func TestApply_DeclineRefusesWhenItCannotLook(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	path := agentPath(base, project, "a")
	mustMkdir(t, filepath.Dir(path))
	st := state.New()
	recordCreated(t, st, project, "a", path, target, mustHash(t, target))

	plan := planFor(t, base, repo, st, Options{})
	if got := itemFor(t, plan, project, "a").Op; got != OpDecline {
		t.Fatalf("op = %s, want %s", got, OpDecline)
	}
	replaceWithFile(t, filepath.Dir(path))

	done, errs := plan.Apply(repo, st)
	oneRefusal(t, errs, "re-checking")
	if len(done.Declined) != 0 || st.IsDeclined(state.Key{Project: project, Name: "a"}) {
		t.Error("a marker was written for a path that could not be examined")
	}
	if _, ok := st.LookupLink(path); !ok {
		t.Error("the record was dropped without a decline to replace it")
	}
}

// --- adopt ------------------------------------------------------------------

// TestApply_AdoptRechecksTheLinkAndItsTarget: no record is written for a link
// or target that changed after it was judged.
func TestApply_AdoptRechecksTheLinkAndItsTarget(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name string
		// disturb returns what the refusal must mention.
		disturb func(t *testing.T, repo *scan.Repo, path, target string) string
	}{
		{
			name: "the link vanished",
			disturb: func(t *testing.T, repo *scan.Repo, path, target string) string {
				mustRemove(t, path)
				return fmt.Sprintf("re-checking %q", path)
			},
		},
		{
			// The original target is untouched, so the hash would still match
			// if the link's own target were not compared.
			name: "the link was re-pointed",
			disturb: func(t *testing.T, repo *scan.Repo, path, target string) string {
				mustRemove(t, path)
				mustLink(t, globalAgent(t, repo, "other", "other\n"), path)
				return "did not examine"
			},
		},
		{
			name: "the target vanished",
			disturb: func(t *testing.T, repo *scan.Repo, path, target string) string {
				mustRemove(t, target)
				return "re-checking the target"
			},
		},
		{
			// Hashing a directory also fails, so only the message tells the
			// type check apart from the hash failing.
			name: "the target became a directory",
			disturb: func(t *testing.T, repo *scan.Repo, path, target string) string {
				mustRemove(t, target)
				mustMkdir(t, target)
				return "not a regular file"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			target := globalAgent(t, repo, "a", "body\n")
			path := agentPath(base, project, "a")
			mustLink(t, target, path)

			st := state.New()
			plan := planFor(t, base, repo, st, Options{Adopt: true})
			if got := itemFor(t, plan, project, "a").Op; got != OpAdopt {
				t.Fatalf("op = %s, want %s", got, OpAdopt)
			}
			mentions := tc.disturb(t, repo, path, target)

			done, errs := plan.Apply(repo, st)
			oneRefusal(t, errs, mentions)
			if len(done.Adopted) != 0 {
				t.Errorf("adopted %v despite the refusal", done.Adopted)
			}
			if _, ok := st.LookupLink(path); ok {
				t.Error("a record was written for a link that failed its recheck")
			}
		})
	}
}

// --- drop-capture -----------------------------------------------------------

// TestApply_DropCaptureRechecksTheCaptureAndTheCommitGate: the delete is
// authorised only by the capture being the same regular file and still in
// HEAD, so losing either keeps it.
func TestApply_DropCaptureRechecksTheCaptureAndTheCommitGate(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name     string
		disturb  func(t *testing.T, base string, repo *scan.Repo, capture string)
		mentions string
		gone     bool
	}{
		{
			name: "untracked and committed since",
			disturb: func(t *testing.T, base string, repo *scan.Repo, capture string) {
				testfs.Git(t, repo.Root, "rm", "-q", "--cached", "--", "project-agents/Web/proj/cap.md")
				testfs.Git(t, repo.Root, "commit", "-qm", "untrack")
			},
			mentions: "is uncommitted as of now",
		},
		{
			name: "git can no longer be asked",
			disturb: func(t *testing.T, base string, repo *scan.Repo, capture string) {
				testfs.BreakGitfile(t, repo.Root)
			},
			mentions: "is unknown as of now",
		},
		{
			// Identical bytes behind the link, so a Stat in place of Lstat
			// would hash the same. The commit gate also refuses a typechange,
			// so only the message separates the two.
			name: "replaced by a symlink to the same bytes",
			disturb: func(t *testing.T, base string, repo *scan.Repo, capture string) {
				same := mustWrite(t, filepath.Join(base, "elsewhere", "cap.md"), "captured\n")
				mustRemove(t, capture)
				mustLink(t, same, capture)
			},
			mentions: "no longer a regular file",
		},
		{
			name:     "deleted outright",
			disturb:  func(t *testing.T, base string, repo *scan.Repo, capture string) { mustRemove(t, capture) },
			mentions: "re-checking",
			gone:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
			capture := repo.BackupPath(project, "cap")

			st := state.New()
			plan := planFor(t, base, repo, st, Options{Drops: []state.Key{{Project: project, Name: "cap"}}})
			if got := itemFor(t, plan, project, "cap").Op; got != OpDropCapture {
				t.Fatalf("op = %s, want %s", got, OpDropCapture)
			}
			tc.disturb(t, base, repo, capture)

			done, errs := plan.Apply(repo, st)
			oneRefusal(t, errs, tc.mentions)
			if len(done.Dropped) != 0 {
				t.Errorf("dropped %v despite the refusal", done.Dropped)
			}
			if !tc.gone {
				assertPresent(t, capture, "the gate no longer vouches for these bytes")
			}
		})
	}
}

// --- re-key -----------------------------------------------------------------

// TestApply_RekeyRechecksBothDirectories: a source or destination that changed
// after planning stops the move before anything is moved or repointed.
func TestApply_RekeyRechecksBothDirectories(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)

	cases := []struct {
		name string
		// disturb returns what the refusal must mention.
		disturb func(t *testing.T, oldDir, newDir string) string
	}{
		{
			name: "the source gained a subdirectory",
			disturb: func(t *testing.T, oldDir, newDir string) string {
				mustWrite(t, filepath.Join(oldDir, "nested", "c.md"), "somebody else's\n")
				return "gained a subdirectory"
			},
		},
		{
			name: "the source lost its captures",
			disturb: func(t *testing.T, oldDir, newDir string) string {
				mustRemove(t, filepath.Join(oldDir, "a.md"))
				return "no longer holds any captures"
			},
		},
		{
			// An unreadable source reads as empty if the error is dropped, and
			// the empty-source refusal does not name the read.
			name: "the source can no longer be read",
			disturb: func(t *testing.T, oldDir, newDir string) string {
				replaceWithFile(t, oldDir)
				return fmt.Sprintf("re-checking %q", oldDir)
			},
		},
		{
			name: "the destination can no longer be read",
			disturb: func(t *testing.T, oldDir, newDir string) string {
				mustWrite(t, newDir, "a file where the destination goes\n")
				return fmt.Sprintf("re-checking %q", newDir)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			path := agentPath(base, newKey, "a")
			mustLink(t, oldCapture, path)
			st := state.New()
			recordCreated(t, st, newKey, "a", path, oldCapture, mustHash(t, oldCapture))

			plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})
			if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
				t.Fatalf("plan = %v, refused %v", plan.Rekeys, plan.Refused)
			}
			oldDir, newDir := plan.Rekeys[0].OldDir, plan.Rekeys[0].NewDir
			mentions := tc.disturb(t, oldDir, newDir)

			done, errs := plan.Apply(repo, st)
			oneRefusal(t, errs, mentions)
			if len(done.Rekeyed) != 0 || len(done.Repointed) != 0 {
				t.Errorf("rekeyed %v, repointed %v despite the refusal", done.Rekeyed, done.Repointed)
			}
			assertPresent(t, oldDir, "the move was refused")
			if got := mustReadlink(t, path); got != oldCapture {
				t.Errorf("the link points at %s, want it left at %s", got, oldCapture)
			}
			if rec, _ := st.LookupLink(path); rec.Target != oldCapture {
				t.Errorf("record target = %s, want it left at %s", rec.Target, oldCapture)
			}
		})
	}
}

// TestApply_RepointRechecksTheLinkAndTheMovedCapture: the move has happened,
// so a link whose own state or whose moved capture is wrong is left as it was,
// with its record, and reported.
func TestApply_RepointRechecksTheLinkAndTheMovedCapture(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)

	cases := []struct {
		name     string
		disturb  func(t *testing.T, base, path, oldCapture string)
		mentions string
		linkGone bool
	}{
		{
			name:     "the link vanished",
			disturb:  func(t *testing.T, base, path, oldCapture string) { mustRemove(t, path) },
			mentions: "reading the link",
			linkGone: true,
		},
		{
			name:     "the capture it follows vanished",
			disturb:  func(t *testing.T, base, path, oldCapture string) { mustRemove(t, oldCapture) },
			mentions: "the moved capture is not at",
		},
		{
			// A symlink rather than a directory: the source recheck counts a
			// directory and would refuse the whole move first.
			name: "the capture it follows became a symlink",
			disturb: func(t *testing.T, base, path, oldCapture string) {
				same := mustWrite(t, filepath.Join(base, "elsewhere", "a.md"), "captured\n")
				mustRemove(t, oldCapture)
				mustLink(t, same, oldCapture)
			},
			mentions: "is not a regular file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			// A second capture keeps the source non-empty when a.md goes.
			mustWrite(t, repo.BackupPath(oldKey, "b"), "kept\n")
			path := agentPath(base, newKey, "a")
			mustLink(t, oldCapture, path)
			st := state.New()
			recordCreated(t, st, newKey, "a", path, oldCapture, mustHash(t, oldCapture))

			plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})
			if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
				t.Fatalf("plan = %v, refused %v", plan.Rekeys, plan.Refused)
			}
			tc.disturb(t, base, path, oldCapture)

			done, errs := plan.Apply(repo, st)
			oneRefusal(t, errs, tc.mentions)
			if len(done.Rekeyed) != 1 {
				t.Errorf("the move itself should have gone ahead: %v", done.Rekeyed)
			}
			if len(done.Repointed) != 0 {
				t.Errorf("repointed %v despite the refusal", done.Repointed)
			}
			if !tc.linkGone {
				if got := mustReadlink(t, path); got != oldCapture {
					t.Errorf("the link points at %s, want it left at %s", got, oldCapture)
				}
			}
			if rec, _ := st.LookupLink(path); rec.Target != oldCapture {
				t.Errorf("record target = %s, want it left at %s", rec.Target, oldCapture)
			}
		})
	}
}

// TestApply_RepointReportsALinkItCouldNotRewrite: a failed rewrite leaves the
// link and its record pointing at the old capture, together.
func TestApply_RepointReportsALinkItCouldNotRewrite(t *testing.T) {
	const oldKey = "Web/old-name"
	base, repo := fixture(t)

	// 36 bytes of room fits the link (+20) and the moved capture (+32), but
	// not ReplaceLink's temporary name beside the link, which fails with
	// ENAMETOOLONG for root too.
	agents := filepath.Join(testfs.DirWithRoom(t, base, 36), ".claude", "agents")
	newKey := scan.ProjectKey(base, agents)
	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	path := filepath.Join(agents, "a.md")
	mustLink(t, oldCapture, path)
	st := state.New()
	recordCreated(t, st, newKey, "a", path, oldCapture, mustHash(t, oldCapture))

	plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})
	if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
		t.Fatalf("plan = %v, refused %v", plan.Rekeys, plan.Refused)
	}

	done, errs := plan.Apply(repo, st)
	oneRefusal(t, errs, "creating a temporary link")
	if len(done.Repointed) != 0 {
		t.Errorf("repointed %v despite the failure", done.Repointed)
	}
	if got := mustReadlink(t, path); got != oldCapture {
		t.Errorf("the link points at %s, want it left at %s", got, oldCapture)
	}
	if rec, _ := st.LookupLink(path); rec.Target != oldCapture {
		t.Errorf("record target = %s, want it left at %s", rec.Target, oldCapture)
	}
}

// --- re-key proposals -------------------------------------------------------

// TestNewPlan_RekeyIsNotProposedOverALiveProjectsCaptures: a live project that
// has captures of its own, or whose captures cannot be looked at, is not an
// empty landing place for an orphan.
func TestNewPlan_RekeyIsNotProposedOverALiveProjectsCaptures(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Nested/old-name"
	)

	cases := []struct {
		name        string
		setup       func(t *testing.T, repo *scan.Repo)
		wantProblem bool
	}{
		{
			name: "it has captures of its own",
			setup: func(t *testing.T, repo *scan.Repo) {
				mustWrite(t, repo.BackupPath(newKey, "b"), "its own\n")
			},
		},
		{
			name: "its capture directory cannot be read",
			setup: func(t *testing.T, repo *scan.Repo) {
				mustWrite(t, filepath.Dir(repo.BackupPath(newKey, "b")), "not a directory\n")
			},
			wantProblem: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			mustWrite(t, agentPath(base, newKey, "a"), "captured\n")
			tc.setup(t, repo)

			plan := planFor(t, base, repo, state.New(), Options{})
			if got := plan.OfOp(OpRekeyCandidate); len(got) != 0 {
				t.Errorf("proposed %v", got)
			}
			found := false
			for _, p := range plan.Problems {
				if strings.Contains(p.Err, "captures of its own") {
					found = true
				}
			}
			if found != tc.wantProblem {
				t.Errorf("problem reported = %v, want %v: %v", found, tc.wantProblem, plan.Problems)
			}
		})
	}
}
