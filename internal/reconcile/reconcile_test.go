package reconcile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/doctor"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// fixture builds a fake home holding the agent repo and the projects beside it,
// and points $HOME at it.
//
// The temp dir is resolved first because every path this package compares —
// a link's target against a record's, a capture against the directory being
// moved — is compared as a string. On macOS /var is a symlink, so an unresolved
// base would make every one of those comparisons fail for a reason that has
// nothing to do with the code.
func fixture(t *testing.T) (base string, repo *scan.Repo) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	mustMkdir(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)

	repo, err = scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return base, repo
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) string {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// agentPath is where an agent file lives inside a project.
func agentPath(base, project, name string) string {
	return filepath.Join(base, filepath.FromSlash(project), ".claude", "agents", name+".md")
}

// globalAgent writes an agent into the repo's global pool.
func globalAgent(t *testing.T, repo *scan.Repo, name, content string) string {
	t.Helper()
	return mustWrite(t, filepath.Join(repo.Root, "agents", name+".md"), content)
}

func mustLink(t *testing.T, target, path string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
}

// recordCreated plants the record link --apply would have written.
func recordCreated(t *testing.T, st *state.File, project, name, path, target, hash string) {
	t.Helper()
	if _, err := st.RecordCreated(state.Link{
		Key: state.Key{Project: project, Name: name}, Path: path, Target: target, Hash: hash,
	}, time.Now()); err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
}

func recordAdopted(t *testing.T, st *state.File, project, name, path, target, hash string) {
	t.Helper()
	if _, err := st.RecordAdopted(state.Link{
		Key: state.Key{Project: project, Name: name}, Path: path, Target: target, Hash: hash,
	}, time.Now()); err != nil {
		t.Fatalf("RecordAdopted: %v", err)
	}
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	h, err := scan.HashFile(path)
	if err != nil {
		t.Fatalf("hashing %s: %v", path, err)
	}
	return h
}

// planFor runs a whole reconcile plan the way the command does: scan, diagnose,
// then plan. Going through doctor rather than around it is the point — the plan
// may only act on drift doctor also reports.
func planFor(t *testing.T, base string, repo *scan.Repo, st *state.File, opts Options) Plan {
	t.Helper()
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	plan, err := NewPlan(repo, res, doctor.Diagnose(repo, res), st, opts)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return plan
}

// itemFor finds the single item for one agent, and fails when there is not
// exactly one: a plan holding two verdicts about one file is a bug the
// assertion below would otherwise pick between at random.
func itemFor(t *testing.T, p Plan, project, name string) Item {
	t.Helper()
	var found []Item
	for _, i := range p.Items {
		if i.Project == project && i.Name == name {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one item for %s/%s, got %d: %v", project, name, len(found), found)
	}
	return found[0]
}

func assertNoItem(t *testing.T, p Plan, project, name string) {
	t.Helper()
	for _, i := range p.Items {
		if i.Project == project && i.Name == name {
			t.Fatalf("%s/%s should not appear in the plan, but does as %s: %s", project, name, i.Op, i.Reason)
		}
	}
}

func assertIsLink(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (%v)", path, fi.Mode())
	}
}

func assertGone(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is still there (%v); %s", path, err, why)
	}
}

func assertPresent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is gone (%v); %s", path, err, why)
	}
}

func noErrs(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		t.Errorf("unexpected apply error: %v", err)
	}
}

// --- the record-against-disk table ------------------------------------------

// TestNewPlan_RecordAgainstDisk covers recorded paths that are now symlinks,
// regular files, or absent. Every reconciliation outcome for a recorded link
// is exercised from one fixture shape.
func TestNewPlan_RecordAgainstDisk(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name string
		// setup plants the on-disk situation and returns the record's target
		// and the hash the record carries.
		setup  func(t *testing.T, base string, repo *scan.Repo, path string) (target, hash string)
		wantOp Op
		reason string
	}{
		{
			name: "link gone and nothing in its place is a decline",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "gone", "body\n")
				hash := mustHash(t, target)
				mustMkdir(t, filepath.Dir(path))
				return target, hash
			},
			wantOp: OpDecline,
			reason: "nothing stands in its place",
		},
		{
			name: "link replaced by a file holding the recorded content is dereferenced",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "deref", "body\n")
				mustWrite(t, path, "body\n")
				return target, mustHash(t, target)
			},
			wantOp: OpDereferenced,
			reason: "safe to link again",
		},
		{
			name: "link replaced by a file edited since is a conflict",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "conflict", "body\n")
				hash := mustHash(t, target)
				mustWrite(t, path, "body\nedited by hand\n")
				return target, hash
			},
			wantOp: OpConflict,
			reason: "import --apply",
		},
		{
			name: "healthy link aimed where the record says is unchanged",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "healthy", "body\n")
				mustLink(t, target, path)
				return target, mustHash(t, target)
			},
			wantOp: OpUnchanged,
			reason: "still the link this tool recorded",
		},
		{
			name: "link re-pointed at another repo file has diverged",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "diverged", "body\n")
				other := globalAgent(t, repo, "elsewhere", "other\n")
				mustLink(t, other, path)
				return target, mustHash(t, target)
			},
			wantOp: OpDiverged,
			reason: "not the",
		},
		{
			name: "a record whose own target is outside the repo has diverged",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				// Only a hand-edited state file produces this, which is
				// exactly why the foreign arm has to be reachable.
				outside := mustWrite(t, filepath.Join(base, "elsewhere", "foreign.md"), "not ours\n")
				mustLink(t, outside, path)
				return outside, mustHash(t, outside)
			},
			wantOp: OpDiverged,
			reason: "outside this repo",
		},
		{
			name: "broken link still aimed where the record says is prunable",
			setup: func(t *testing.T, base string, repo *scan.Repo, path string) (string, string) {
				target := globalAgent(t, repo, "broken", "body\n")
				hash := mustHash(t, target)
				mustLink(t, target, path)
				if err := os.Remove(target); err != nil {
					t.Fatalf("removing the target: %v", err)
				}
				return target, hash
			},
			wantOp: OpPrunable,
			reason: "--" + PruneFlag,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			path := agentPath(base, project, "a")
			target, hash := tc.setup(t, base, repo, path)

			st := state.New()
			recordCreated(t, st, project, "a", path, target, hash)

			plan := planFor(t, base, repo, st, Options{})
			item := itemFor(t, plan, project, "a")
			if item.Op != tc.wantOp {
				t.Fatalf("op = %s, want %s (reason: %s)", item.Op, tc.wantOp, item.Reason)
			}
			if !strings.Contains(item.Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", item.Reason, tc.reason)
			}
			if len(plan.Problems) > 0 {
				t.Errorf("unexpected problems: %v", plan.Problems)
			}
		})
	}
}

// --- the prune gate ---------------------------------------------------------

// TestNewPlan_PruneGate: which combinations of origin and flag authorise
// removing a broken link. Nothing else in this tool removes anything, so this
// table is the whole authorisation surface.
func TestNewPlan_PruneGate(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name     string
		adopted  bool
		opts     Options
		wantOp   Op
		mentions string
	}{
		{name: "created, no flag", wantOp: OpPrunable, mentions: "--" + PruneFlag},
		{name: "created, --prune", opts: Options{Prune: true}, wantOp: OpPrune},
		{name: "adopted, no flag", adopted: true, wantOp: OpPrunable, mentions: "--" + PruneFlag},
		{
			name: "adopted, --prune alone is not enough", adopted: true,
			opts: Options{Prune: true}, wantOp: OpPrunable, mentions: "--" + PruneAdoptedFlag,
		},
		{
			name: "adopted, --prune --prune-adopted", adopted: true,
			opts: Options{Prune: true, PruneAdopted: true}, wantOp: OpPrune,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			target := globalAgent(t, repo, "a", "body\n")
			hash := mustHash(t, target)
			path := agentPath(base, project, "a")
			mustLink(t, target, path)
			if err := os.Remove(target); err != nil {
				t.Fatalf("removing the target: %v", err)
			}

			st := state.New()
			if tc.adopted {
				recordAdopted(t, st, project, "a", path, target, hash)
			} else {
				recordCreated(t, st, project, "a", path, target, hash)
			}

			item := itemFor(t, planFor(t, base, repo, st, tc.opts), project, "a")
			if item.Op != tc.wantOp {
				t.Fatalf("op = %s, want %s (reason: %s)", item.Op, tc.wantOp, item.Reason)
			}
			if tc.mentions != "" && !strings.Contains(item.Reason, tc.mentions) {
				t.Errorf("reason %q does not name %s", item.Reason, tc.mentions)
			}
		})
	}
}

// TestApply_PruneRemovesTheLinkThenTheRecord: the ordering the whole prune
// argument rests on, checked from the outside — the link is gone and the
// document no longer claims it, so a second run has nothing to say.
func TestApply_PruneRemovesTheLinkThenTheRecord(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	hash := mustHash(t, target)
	path := agentPath(base, project, "a")
	mustLink(t, target, path)
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	st := state.New()
	recordCreated(t, st, project, "a", path, target, hash)

	plan := planFor(t, base, repo, st, Options{Prune: true})
	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)

	if len(done.Pruned) != 1 {
		t.Fatalf("pruned %d links, want 1", len(done.Pruned))
	}
	assertGone(t, path, "the broken link was pruned")
	if _, ok := st.LookupLink(path); ok {
		t.Error("the record survived the link; nothing must claim a link that is not there")
	}

	// A second run is a no-op: no record, so nothing to judge, so no decline
	// marker either.
	second := planFor(t, base, repo, st, Options{Prune: true})
	assertNoItem(t, second, project, "a")
	if len(st.AllDeclined()) != 0 {
		t.Errorf("pruning wrote a declined marker: %v", st.AllDeclined())
	}
}

// TestApply_PruneRefusesWhenTheTargetCameBack: the re-check, which is also what
// makes running --rekey and --prune together safe. A move can restore the file
// a link points at between plan and apply, and a working link is never removed.
func TestApply_PruneRefusesWhenTheTargetCameBack(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	hash := mustHash(t, target)
	path := agentPath(base, project, "a")
	mustLink(t, target, path)
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	st := state.New()
	recordCreated(t, st, project, "a", path, target, hash)
	plan := planFor(t, base, repo, st, Options{Prune: true})

	globalAgent(t, repo, "a", "body\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "no longer broken") {
		t.Errorf("refusal does not say why: %v", errs[0])
	}
	if len(done.Pruned) != 0 {
		t.Errorf("pruned %v despite the refusal", done.Pruned)
	}
	assertIsLink(t, path)
	if _, ok := st.LookupLink(path); !ok {
		t.Error("the record was dropped for a link that was not removed")
	}
}

// TestNewPlan_BrokenLinkWithNoRecordIsNeverTouched: the property the state file
// exists for. A broken link this tool has no record of is indistinguishable
// from one somebody made by hand, so reconcile has nothing to say about it and
// certainly nothing to do.
func TestNewPlan_BrokenLinkWithNoRecordIsNeverTouched(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	path := agentPath(base, project, "a")
	mustLink(t, target, path)
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	st := state.New()
	plan := planFor(t, base, repo, st, Options{Prune: true, PruneAdopted: true, Adopt: true})
	assertNoItem(t, plan, project, "a")

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Pruned) != 0 {
		t.Errorf("pruned %v with no record authorising it", done.Pruned)
	}
	assertPresent(t, path, "an unrecorded link is never removed")
}

// --- decline ----------------------------------------------------------------

// TestApply_DeclineWritesTheMarkerAndDropsTheRecord, and does nothing at all
// the second time. A marker rewritten on every run would make its timestamp —
// the only evidence of when the decision was made — meaningless.
func TestApply_DeclineWritesTheMarkerAndDropsTheRecord(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	hash := mustHash(t, target)
	path := agentPath(base, project, "a")
	mustMkdir(t, filepath.Dir(path))

	st := state.New()
	recordCreated(t, st, project, "a", path, target, hash)

	done, errs := planFor(t, base, repo, st, Options{}).Apply(repo, st)
	noErrs(t, errs)
	if len(done.Declined) != 1 {
		t.Fatalf("declined %d, want 1", len(done.Declined))
	}
	key := state.Key{Project: project, Name: "a"}
	if !st.IsDeclined(key) {
		t.Fatal("the marker is not in the document")
	}
	if _, ok := st.LookupLink(path); ok {
		t.Error("the record survived; the next run would decline it again")
	}
	first := done.Declined[0].DeclinedAt

	second := planFor(t, base, repo, st, Options{})
	assertNoItem(t, second, project, "a")
	againDone, errs := second.Apply(repo, st)
	noErrs(t, errs)
	if len(againDone.Declined) != 0 {
		t.Errorf("the second run rewrote the marker: %v", againDone.Declined)
	}
	if got, _ := st.LookupDeclined(key); got.DeclinedAt != first {
		t.Errorf("declined_at moved from %s to %s", first, got.DeclinedAt)
	}
}

// TestApply_DeclineRefusesWhenSomethingAppeared: the re-check. A file that
// arrived between plan and apply means the observation the marker records —
// nothing in its place — is no longer true.
func TestApply_DeclineRefusesWhenSomethingAppeared(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	path := agentPath(base, project, "a")
	mustMkdir(t, filepath.Dir(path))

	st := state.New()
	recordCreated(t, st, project, "a", path, target, mustHash(t, target))
	plan := planFor(t, base, repo, st, Options{})

	mustWrite(t, path, "somebody put this back\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if len(done.Declined) != 0 || st.IsDeclined(state.Key{Project: project, Name: "a"}) {
		t.Error("a marker was written for a path that is no longer empty")
	}
}

// --- adopt ------------------------------------------------------------------

// TestNewPlan_AdoptTakesOnlyHealthyCorrectlyAimedLinks. Every other shape in
// one project, so a pass that widened its criteria would fail here rather than
// quietly claim ownership of something it did not create.
func TestNewPlan_AdoptTakesOnlyHealthyCorrectlyAimedLinks(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	healthy := globalAgent(t, repo, "healthy", "one\n")
	mustLink(t, healthy, agentPath(base, project, "healthy"))

	broken := globalAgent(t, repo, "broken", "two\n")
	mustLink(t, broken, agentPath(base, project, "broken"))
	if err := os.Remove(broken); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	outside := mustWrite(t, filepath.Join(base, "elsewhere", "foreign.md"), "not ours\n")
	mustLink(t, outside, agentPath(base, project, "foreign"))

	// A real file, identical to its counterpart: adoptable by link, and not
	// a link, so never adoptable by this pass.
	globalAgent(t, repo, "plain", "three\n")
	mustWrite(t, agentPath(base, project, "plain"), "three\n")

	// A healthy link aimed at the global agent while this project has its
	// own capture: link calls that a mismatch, and recording it would claim
	// ownership of somebody's deliberate choice.
	global := globalAgent(t, repo, "mismatch", "global\n")
	mustWrite(t, repo.BackupPath(project, "mismatch"), "captured\n")
	mustLink(t, global, agentPath(base, project, "mismatch"))

	st := state.New()
	plan := planFor(t, base, repo, st, Options{Adopt: true})

	if got := itemFor(t, plan, project, "healthy").Op; got != OpAdopt {
		t.Errorf("healthy link: op = %s, want %s", got, OpAdopt)
	}
	for _, name := range []string{"broken", "foreign", "plain", "mismatch"} {
		assertNoItem(t, plan, project, name)
	}

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Adopted) != 1 {
		t.Fatalf("adopted %d links, want 1: %v", len(done.Adopted), done.Adopted)
	}
	if got := done.Adopted[0].Origin; got != state.OriginAdopted {
		t.Errorf("origin = %s, want %s", got, state.OriginAdopted)
	}
}

// TestNewPlan_UnrecordedSaysWhatItCannotKnow. The caveat has to be in the
// report, not only in a doc comment: an adopted record is the one claim in this
// tool that is weaker than it looks, and the person reading the line is the one
// deciding whether to act on it.
func TestNewPlan_UnrecordedSaysWhatItCannotKnow(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	mustLink(t, target, agentPath(base, project, "a"))

	plain := itemFor(t, planFor(t, base, repo, state.New(), Options{}), project, "a")
	if plain.Op != OpUnrecorded {
		t.Fatalf("op = %s, want %s", plain.Op, OpUnrecorded)
	}
	if !strings.Contains(plain.Reason, "--"+AdoptFlag) {
		t.Errorf("reason does not name --%s: %q", AdoptFlag, plain.Reason)
	}

	adopting := itemFor(t, planFor(t, base, repo, state.New(), Options{Adopt: true}), project, "a")
	if !strings.Contains(adopting.Reason, "CANNOT tell") {
		t.Errorf("the adopt reason does not admit what the pass cannot know: %q", adopting.Reason)
	}
	if !strings.Contains(adopting.Reason, "--"+PruneAdoptedFlag) {
		t.Errorf("the adopt reason does not name --%s: %q", PruneAdoptedFlag, adopting.Reason)
	}
}

// TestApply_AdoptRefusesAChangedTarget: the record is a merge base, so one
// taken against content the target no longer holds would misjudge the very row
// it exists to decide.
func TestApply_AdoptRefusesAChangedTarget(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	mustLink(t, target, agentPath(base, project, "a"))

	st := state.New()
	plan := planFor(t, base, repo, st, Options{Adopt: true})
	mustWrite(t, target, "rewritten between the two phases\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if len(done.Adopted) != 0 {
		t.Errorf("recorded %v against content it did not verify", done.Adopted)
	}
}

// TestApply_StateSaveFailureReportsNoStateOnlyChanges: an adopt and a decline
// exist only in the state document, so a failed save means neither happened.
func TestApply_StateSaveFailureReportsNoStateOnlyChanges(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	adoptTarget := globalAgent(t, repo, "adopt", "one\n")
	mustLink(t, adoptTarget, agentPath(base, project, "adopt"))

	declineTarget := globalAgent(t, repo, "decline", "two\n")
	declinePath := agentPath(base, project, "decline")
	st := state.New()
	recordCreated(t, st, project, "decline", declinePath, declineTarget, mustHash(t, declineTarget))

	plan := planFor(t, base, repo, st, Options{Adopt: true})
	if got := itemFor(t, plan, project, "adopt").Op; got != OpAdopt {
		t.Fatalf("adopt op = %s, want %s", got, OpAdopt)
	}
	if got := itemFor(t, plan, project, "decline").Op; got != OpDecline {
		t.Fatalf("decline op = %s, want %s", got, OpDecline)
	}
	// A non-empty directory where the state file goes makes Save's rename fail.
	mustWrite(t, filepath.Join(state.Path(repo), "kept"), "inside\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), state.FileName) {
		t.Fatalf("want one error naming %s, got %v", state.FileName, errs)
	}
	if len(done.Adopted) != 0 {
		t.Errorf("adopted = %v, want none after a failed save", done.Adopted)
	}
	if len(done.Declined) != 0 {
		t.Errorf("declined = %v, want none after a failed save", done.Declined)
	}
}

// --- dry run ----------------------------------------------------------------

// TestNewPlan_WritesNothing: planning is read-only, whatever the flags say.
func TestNewPlan_WritesNothing(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	hash := mustHash(t, target)
	path := agentPath(base, project, "a")
	mustLink(t, target, path)
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}
	capture := mustWrite(t, repo.BackupPath(project, "cap"), "captured\n")

	// Written and reloaded so the document under test starts clean: Dirty is
	// what decides whether Apply saves at all, so a fixture that arrived
	// dirty would prove nothing about planning.
	seed := state.New()
	recordCreated(t, seed, project, "a", path, target, hash)
	if err := seed.Save(repo); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(state.Path(repo))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	st, err := state.Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	plan := planFor(t, base, repo, st, Options{
		Prune: true, PruneAdopted: true, Adopt: true,
		Drops: []state.Key{{Project: project, Name: "cap"}},
	})
	if !plan.Pending() {
		t.Fatal("the plan has nothing pending, so this proves nothing")
	}
	assertPresent(t, path, "a plan writes nothing")
	assertPresent(t, capture, "a plan writes nothing")
	if st.Dirty() {
		t.Error("planning made the state document dirty")
	}
	after, err := os.ReadFile(state.Path(repo))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if string(after) != string(before) {
		t.Error("planning rewrote the state file")
	}
}

// --- drop-capture -----------------------------------------------------------

// TestNewPlan_DropCaptureGate: only a clean CommittedYes authorises a delete.
// Every other answer, "could not ask" included, refuses — a capture whose bytes
// are in no commit may be the last copy of something.
func TestNewPlan_DropCaptureGate(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name      string
		setup     func(t *testing.T, base string, repo *scan.Repo)
		wantGuard Guard
	}{
		{
			name: "committed and unmodified is authorised",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
			},
		},
		{
			name: "edited since it was committed refuses",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
				mustWrite(t, repo.BackupPath(project, "cap"), "edited since\n")
			},
			wantGuard: GuardUncommitted,
		},
		{
			name: "untracked refuses",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "README.md", "hello\n")
				mustWrite(t, repo.BackupPath(project, "cap"), "never committed\n")
			},
			wantGuard: GuardUncommitted,
		},
		{
			name: "git that cannot be asked refuses",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
				testfs.DubiousOwnership(t, repo.Root)
			},
			wantGuard: GuardCommitUnknown,
		},
		{
			name: "no such capture refuses",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "README.md", "hello\n")
			},
			wantGuard: GuardNoCapture,
		},
		{
			name: "a capture a live link points at refuses",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
				mustLink(t, repo.BackupPath(project, "cap"), agentPath(base, project, "cap"))
			},
			wantGuard: GuardCaptureInUse,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			tc.setup(t, base, repo)

			plan := planFor(t, base, repo, state.New(), Options{
				Drops: []state.Key{{Project: project, Name: "cap"}},
			})

			if tc.wantGuard == "" {
				if len(plan.Refused) != 0 {
					t.Fatalf("refused: %v", plan.Refused)
				}
				if got := itemFor(t, plan, project, "cap").Op; got != OpDropCapture {
					t.Fatalf("op = %s, want %s", got, OpDropCapture)
				}
				return
			}
			if len(plan.Refused) != 1 {
				t.Fatalf("want exactly one refusal, got %v", plan.Refused)
			}
			if got := plan.Refused[0].Guard; got != tc.wantGuard {
				t.Errorf("guard = %q, want %q (%s)", got, tc.wantGuard, plan.Refused[0].Reason)
			}
			if n := len(plan.OfOp(OpDropCapture)); n != 0 {
				t.Errorf("%d captures authorised despite the refusal", n)
			}
		})
	}
}

// TestApply_DropCaptureDeletesOnlyWhatItGated, and leaves the working tree
// alone otherwise.
func TestApply_DropCaptureDeletesOnlyWhatItGated(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)
	testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")

	capture := repo.BackupPath(project, "cap")
	st := state.New()
	plan := planFor(t, base, repo, st, Options{Drops: []state.Key{{Project: project, Name: "cap"}}})

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Dropped) != 1 {
		t.Fatalf("dropped %d, want 1", len(done.Dropped))
	}
	assertGone(t, capture, "the capture was dropped")
}

// TestApply_DropCaptureRefusesBytesItDidNotGate: the commit gate is about the
// content, so a capture rewritten between plan and apply is a different file
// and the proof no longer covers it.
func TestApply_DropCaptureRefusesBytesItDidNotGate(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)
	testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")

	capture := repo.BackupPath(project, "cap")
	st := state.New()
	plan := planFor(t, base, repo, st, Options{Drops: []state.Key{{Project: project, Name: "cap"}}})

	mustWrite(t, capture, "rewritten between the two phases\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if len(done.Dropped) != 0 {
		t.Errorf("dropped %v", done.Dropped)
	}
	assertPresent(t, capture, "the bytes on disk were never proved recoverable")
}

// --- re-key -----------------------------------------------------------------

// TestApply_RekeyMovesCapturesAndRepointsLinks: the whole compound operation,
// including the record behind the link, which must move with it or the two
// disagree about where the link points.
func TestApply_RekeyMovesCapturesAndRepointsLinks(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)
	base, repo := fixture(t)

	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	hash := mustHash(t, oldCapture)
	path := agentPath(base, newKey, "a")
	mustLink(t, oldCapture, path)

	st := state.New()
	recordCreated(t, st, newKey, "a", path, oldCapture, hash)

	plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})
	if len(plan.Refused) != 0 {
		t.Fatalf("refused: %v", plan.Refused)
	}
	if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
		t.Fatalf("plan = %v", plan.Rekeys)
	}

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Rekeyed) != 1 || len(done.Repointed) != 1 {
		t.Fatalf("rekeyed %v, repointed %v", done.Rekeyed, done.Repointed)
	}

	newCapture := repo.BackupPath(newKey, "a")
	assertGone(t, oldCapture, "the captures moved")
	assertPresent(t, newCapture, "the captures moved here")

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	if resolved != newCapture {
		t.Errorf("the link resolves to %s, want %s", resolved, newCapture)
	}
	rec, ok := st.LookupLink(path)
	if !ok {
		t.Fatal("the record vanished")
	}
	if rec.Target != newCapture {
		t.Errorf("record target = %s, want %s", rec.Target, newCapture)
	}
	if rec.Origin != state.OriginCreated {
		t.Errorf("origin = %s, want %s; re-filing a record must not widen what may remove it", rec.Origin, state.OriginCreated)
	}
}

// TestNewPlan_RekeyGates: every refusal, each of which exists because the
// permissive answer silently merges or breaks somebody's captures.
func TestNewPlan_RekeyGates(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)

	cases := []struct {
		name  string
		setup func(t *testing.T, base string, repo *scan.Repo)
		move  KeyMove
		guard Guard
	}{
		{
			name: "a destination that already holds captures",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
				mustWrite(t, repo.BackupPath(newKey, "b"), "already here\n")
			},
			move: KeyMove{Old: oldKey, New: newKey}, guard: GuardOccupied,
		},
		{
			name: "a source holding another project's captures below it",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
				mustWrite(t, repo.BackupPath(oldKey+"/nested", "c"), "somebody else's\n")
			},
			move: KeyMove{Old: oldKey, New: newKey}, guard: GuardNestedCaptures,
		},
		{
			name: "a source with nothing to move",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				mustMkdir(t, filepath.Join(repo.Root, "project-agents", filepath.FromSlash(oldKey)))
			},
			move: KeyMove{Old: oldKey, New: newKey}, guard: GuardNoSource,
		},
		{
			name: "a source no other project may still be linking into",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				capture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
				mustLink(t, capture, agentPath(base, "Web/somebody-else", "a"))
			},
			move: KeyMove{Old: oldKey, New: newKey}, guard: GuardCaptureInUse,
		},
		{
			name: "keys nested in one another",
			setup: func(t *testing.T, base string, repo *scan.Repo) {
				mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			},
			move: KeyMove{Old: oldKey, New: oldKey + "/below"}, guard: GuardOverlapping,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			tc.setup(t, base, repo)

			plan := planFor(t, base, repo, state.New(), Options{Rekeys: []KeyMove{tc.move}})
			if len(plan.Rekeys) != 0 {
				t.Fatalf("the move was authorised: %v", plan.Rekeys)
			}
			if len(plan.Refused) != 1 {
				t.Fatalf("want exactly one refusal, got %v", plan.Refused)
			}
			if got := plan.Refused[0].Guard; got != tc.guard {
				t.Errorf("guard = %q, want %q (%s)", got, tc.guard, plan.Refused[0].Reason)
			}
		})
	}
}

// TestNewPlan_RekeyIsProposedNeverApplied: an orphaned capture directory beside
// a same-named live project with no captures is the shape a moved project
// leaves, and the shape a wrong guess would ruin. It is printed with the
// command that would apply it, and nothing moves.
func TestNewPlan_RekeyIsProposedNeverApplied(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Nested/old-name"
	)
	base, repo := fixture(t)

	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	// A live project of the same basename under a different parent, with an
	// agent file so the scan finds it and no captures of its own.
	mustWrite(t, agentPath(base, newKey, "a"), "captured\n")

	st := state.New()
	plan := planFor(t, base, repo, st, Options{})

	proposals := plan.OfOp(OpRekeyCandidate)
	if len(proposals) != 1 {
		t.Fatalf("want one proposal, got %v", proposals)
	}
	want := "--" + RekeyFlag + " " + oldKey + "=" + newKey
	if !strings.Contains(proposals[0].Reason, want) {
		t.Errorf("the proposal does not carry the command that applies it (%q): %q", want, proposals[0].Reason)
	}
	if len(plan.Rekeys) != 0 {
		t.Fatalf("a proposal was authorised: %v", plan.Rekeys)
	}

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Rekeyed) != 0 {
		t.Errorf("a proposal was applied: %v", done.Rekeyed)
	}
	assertPresent(t, oldCapture, "a proposal moves nothing")
}

// --- options and selectors --------------------------------------------------

func TestParseKeyMove(t *testing.T) {
	cases := []struct {
		in      string
		want    KeyMove
		wantErr string
	}{
		{in: "Web/a=Web/b", want: KeyMove{Old: "Web/a", New: "Web/b"}},
		{in: " Web/a = Web/b ", want: KeyMove{Old: "Web/a", New: "Web/b"}},
		{in: "Web/a/=Web/b/", want: KeyMove{Old: "Web/a", New: "Web/b"}},
		{in: "Web/a", wantErr: "expected OLD=NEW"},
		{in: "--apply", wantErr: "expected OLD=NEW"},
		{in: "Web/a=Web/a", wantErr: "the two keys are the same"},
		{in: "=Web/b", wantErr: "old key is empty"},
		{in: "Web/a=", wantErr: "new key is empty"},
		{in: "Web/a=../../etc", wantErr: "cannot point outside it"},
		{in: "/abs=Web/b", wantErr: "is absolute"},
		{in: "Web//a=Web/b", wantErr: "not in the form the report prints"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseKeyMove(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseKeyMove(%q) = %v, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKeyMove(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOptions_Validate covers the two ways a selector could reach a path this
// command deletes or moves, plus the flag combination that authorises nothing.
func TestOptions_Validate(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "empty is fine"},
		{
			name:    "--prune-adopted alone",
			opts:    Options{PruneAdopted: true},
			wantErr: "authorises nothing on its own",
		},
		{
			name:    "a drop selector climbing out of project-agents",
			opts:    Options{Drops: []state.Key{{Project: "../../etc", Name: "passwd"}}},
			wantErr: "cannot point outside it",
		},
		{
			name:    "a drop selector naming a parent directory",
			opts:    Options{Drops: []state.Key{{Project: "Web/proj", Name: ".."}}},
			wantErr: "not an agent name",
		},
		{
			name: "two moves from the same source",
			opts: Options{Rekeys: []KeyMove{
				{Old: "Web/a", New: "Web/b"}, {Old: "Web/a", New: "Web/c"},
			}},
			wantErr: "as the source twice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// TestNewPlan_RefusesWithoutAStateDocument: the empty document is not a neutral
// answer, so it is never conjured in place of a missing one.
func TestNewPlan_RefusesWithoutAStateDocument(t *testing.T) {
	base, repo := fixture(t)
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, err := NewPlan(repo, res, doctor.Diagnose(repo, res), nil, Options{}); err == nil {
		t.Fatal("NewPlan accepted a nil state document")
	}
}

// TestOpUnresolved_NeverFailsOnTheOrdinary: unrecorded links and re-key
// proposals are permanent features of a machine this tool has been run on, and
// an exit code that fails on them forever is one nobody reads.
func TestOpUnresolved_NeverFailsOnTheOrdinary(t *testing.T) {
	unresolved := map[Op]bool{
		OpPrunable: true, OpDereferenced: true, OpConflict: true, OpDiverged: true,
	}
	all := []Op{
		OpInvalid, OpUnrecorded, OpAdopt, OpPrunable, OpPrune, OpDecline,
		OpDereferenced, OpConflict, OpDiverged, OpUnchanged, OpRekeyCandidate,
		OpDropCapture, OpProjectGone,
	}
	for _, op := range all {
		if got := op.Unresolved(); got != unresolved[op] {
			t.Errorf("%s.Unresolved() = %v, want %v", op, got, unresolved[op])
		}
		if op != OpInvalid && op.String() == "invalid" {
			t.Errorf("op %d has no printable name", int(op))
		}
	}
	if OpInvalid.Writes() {
		t.Error("the zero value authorises a write")
	}
}

// --- printable forms and plan accessors -------------------------------------

// TestPlan_AccessorsAndPrintableForms. The report is built entirely out of
// these, so a String that dropped the one identifying detail would produce a
// report nobody could act on while every other test still passed.
func TestPlan_AccessorsAndPrintableForms(t *testing.T) {
	const (
		project = "Web/proj"
		oldKey  = "Web/old-name"
	)
	base, repo := fixture(t)

	// One of each shape: a prunable link, a conflict, an unrecorded link, a
	// refused selector, and an authorised move carrying a repoint.
	broken := globalAgent(t, repo, "broken", "body\n")
	brokenPath := agentPath(base, project, "broken")
	mustLink(t, broken, brokenPath)
	if err := os.Remove(broken); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	edited := globalAgent(t, repo, "edited", "body\n")
	editedPath := agentPath(base, project, "edited")
	mustWrite(t, editedPath, "edited by hand\n")

	healthy := globalAgent(t, repo, "healthy", "body\n")
	mustLink(t, healthy, agentPath(base, project, "healthy"))

	capture := mustWrite(t, repo.BackupPath(oldKey, "moved"), "captured\n")
	movedPath := agentPath(base, "Web/new-name", "moved")
	mustLink(t, capture, movedPath)

	st := state.New()
	recordCreated(t, st, project, "broken", brokenPath, broken, mustHash(t, edited))
	recordCreated(t, st, project, "edited", editedPath, edited, mustHash(t, edited))

	plan := planFor(t, base, repo, st, Options{
		Rekeys: []KeyMove{{Old: oldKey, New: "Web/new-name"}},
		Drops:  []state.Key{{Project: project, Name: "nope"}},
	})

	counts := plan.Counts()
	for _, op := range []Op{OpPrunable, OpConflict, OpUnrecorded} {
		if counts[op] != 1 {
			t.Errorf("counts[%s] = %d, want 1 (all: %v)", op, counts[op], counts)
		}
		if n := len(plan.OfOp(op)); n != 1 {
			t.Errorf("OfOp(%s) returned %d items", op, n)
		}
	}
	if n := len(plan.Unresolved()); n != 2 {
		t.Errorf("Unresolved() = %d items, want the prunable and the conflict", n)
	}
	if n := len(plan.Writes()); n != 0 {
		t.Errorf("Writes() = %d items with no authorising flag", n)
	}
	if !plan.Pending() {
		t.Error("Pending() is false with an authorised move in the plan")
	}

	if got := plan.OfOp(OpPrunable)[0].String(); !strings.Contains(got, project+"/broken") {
		t.Errorf("item String does not name the agent: %q", got)
	}
	if got := plan.OfOp(OpUnrecorded)[0].String(); !strings.Contains(got, OpUnrecorded.String()) {
		t.Errorf("item String does not name the op: %q", got)
	}
	if len(plan.Refused) != 1 {
		t.Fatalf("refused = %v", plan.Refused)
	}
	if got := plan.Refused[0].String(); !strings.Contains(got, string(GuardNoCapture)) {
		t.Errorf("refused String does not name the gate: %q", got)
	}
	if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
		t.Fatalf("rekeys = %v", plan.Rekeys)
	}
	if got := plan.Rekeys[0].String(); !strings.Contains(got, oldKey) || !strings.Contains(got, "1 capture") {
		t.Errorf("rekey String does not describe the move: %q", got)
	}
	if got := plan.Rekeys[0].Repoint[0].String(); !strings.Contains(got, movedPath) {
		t.Errorf("repoint String does not name the link being rewritten: %q", got)
	}
}

// TestNewPlan_UnjudgeableRecordedPathIsAProblem: not knowing what a file is, is
// not a licence to act on it — so it becomes a problem and no item at all.
func TestNewPlan_UnjudgeableRecordedPathIsAProblem(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	path := agentPath(base, project, "a")
	mustMkdir(t, path)

	st := state.New()
	recordCreated(t, st, project, "a", path, target, mustHash(t, target))

	plan := planFor(t, base, repo, st, Options{Prune: true, PruneAdopted: true})
	assertNoItem(t, plan, project, "a")
	if len(plan.Problems) == 0 {
		t.Fatal("an unjudgeable recorded path produced no problem")
	}

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Pruned) != 0 {
		t.Errorf("acted on a path it could not judge: %v", done.Pruned)
	}
}

// TestNewPlan_RefusesInputItCannotPlanAround: a nil repo and an option
// combination Validate rejects both stop before anything is examined.
func TestNewPlan_RefusesInputItCannotPlanAround(t *testing.T) {
	base, repo := fixture(t)
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	diag := doctor.Diagnose(repo, res)

	if _, err := NewPlan(nil, res, diag, state.New(), Options{}); err == nil {
		t.Error("NewPlan accepted a nil repo")
	}
	if _, err := NewPlan(repo, res, diag, state.New(), Options{PruneAdopted: true}); err == nil {
		t.Error("NewPlan accepted --prune-adopted without --prune")
	}
}

// TestApply_RekeyRefusesADestinationThatFilledUp: the re-check on the gate that
// matters most. Captures that arrived between plan and apply are another
// project's, and merging them is what this gate exists to prevent.
func TestApply_RekeyRefusesADestinationThatFilledUp(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)
	base, repo := fixture(t)

	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	st := state.New()
	plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})

	occupied := mustWrite(t, repo.BackupPath(newKey, "b"), "somebody else's\n")

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "no longer empty") {
		t.Errorf("refusal does not say why: %v", errs[0])
	}
	if len(done.Rekeyed) != 0 {
		t.Errorf("moved %v despite the refusal", done.Rekeyed)
	}
	assertPresent(t, oldCapture, "the move was refused")
	assertPresent(t, occupied, "the move was refused")
}

// TestApply_RepointRefusesALinkThatMoved: the captures move first, so a link
// somebody re-pointed in the meantime is left pointing where they put it and
// reported, rather than being rewritten on the strength of a stale observation.
func TestApply_RepointRefusesALinkThatMoved(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)
	base, repo := fixture(t)

	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	path := agentPath(base, newKey, "a")
	mustLink(t, oldCapture, path)

	st := state.New()
	plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})

	elsewhere := globalAgent(t, repo, "elsewhere", "other\n")
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the link: %v", err)
	}
	mustLink(t, elsewhere, path)

	done, errs := plan.Apply(repo, st)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal, got %v", errs)
	}
	if len(done.Rekeyed) != 1 {
		t.Errorf("the move itself should have gone ahead: %v", done.Rekeyed)
	}
	if len(done.Repointed) != 0 {
		t.Errorf("rewrote a link it had not examined: %v", done.Repointed)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	if resolved != elsewhere {
		t.Errorf("the link resolves to %s, want the %s somebody re-pointed it at", resolved, elsewhere)
	}
}

// TestApply_RekeyCarriesAnAdoptedRecordAcrossWithoutWideningIt: re-filing a
// record must not turn a weaker claim into a stronger one, because the origin
// is what decides whether --prune alone may remove the link later.
func TestApply_RekeyCarriesAnAdoptedRecordAcrossWithoutWideningIt(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)
	base, repo := fixture(t)

	oldCapture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
	path := agentPath(base, newKey, "a")
	mustLink(t, oldCapture, path)

	st := state.New()
	recordAdopted(t, st, newKey, "a", path, oldCapture, mustHash(t, oldCapture))

	plan := planFor(t, base, repo, st, Options{Rekeys: []KeyMove{{Old: oldKey, New: newKey}}})
	_, errs := plan.Apply(repo, st)
	noErrs(t, errs)

	rec, ok := st.LookupLink(path)
	if !ok {
		t.Fatal("the record vanished with the move")
	}
	if rec.Origin != state.OriginAdopted {
		t.Errorf("origin = %s, want %s", rec.Origin, state.OriginAdopted)
	}
	if rec.Target != repo.BackupPath(newKey, "a") {
		t.Errorf("record target = %s, want %s", rec.Target, repo.BackupPath(newKey, "a"))
	}
}

// TestApply_RechecksEverySubjectImmediatelyBeforeActing.
//
// The plan's verdict is formed during a scan that may have walked a hundred
// repositories. Every case below is that window being used: the subject changes
// between the two phases, and the op has to notice rather than act on what it
// last saw.
func TestApply_RechecksEverySubjectImmediatelyBeforeActing(t *testing.T) {
	const project = "Web/proj"

	cases := []struct {
		name string
		// setup returns the options and plants the pre-plan situation.
		setup func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options
		// disturb changes the subject after the plan and before the apply.
		disturb  func(t *testing.T, base string, repo *scan.Repo)
		mentions string
	}{
		{
			name: "the link to prune became a real file",
			setup: func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options {
				target := globalAgent(t, repo, "a", "body\n")
				hash := mustHash(t, target)
				path := agentPath(base, project, "a")
				mustLink(t, target, path)
				if err := os.Remove(target); err != nil {
					t.Fatalf("removing the target: %v", err)
				}
				recordCreated(t, st, project, "a", path, target, hash)
				return Options{Prune: true}
			},
			disturb: func(t *testing.T, base string, repo *scan.Repo) {
				path := agentPath(base, project, "a")
				if err := os.Remove(path); err != nil {
					t.Fatalf("removing the link: %v", err)
				}
				mustWrite(t, path, "somebody replaced it\n")
			},
			mentions: "no longer a symlink",
		},
		{
			name: "the link to prune was re-pointed",
			setup: func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options {
				target := globalAgent(t, repo, "a", "body\n")
				hash := mustHash(t, target)
				path := agentPath(base, project, "a")
				mustLink(t, target, path)
				if err := os.Remove(target); err != nil {
					t.Fatalf("removing the target: %v", err)
				}
				recordCreated(t, st, project, "a", path, target, hash)
				return Options{Prune: true}
			},
			disturb: func(t *testing.T, base string, repo *scan.Repo) {
				path := agentPath(base, project, "a")
				if err := os.Remove(path); err != nil {
					t.Fatalf("removing the link: %v", err)
				}
				mustLink(t, globalAgent(t, repo, "elsewhere", "other\n"), path)
			},
			mentions: "somebody re-pointed",
		},
		{
			name: "the link to prune vanished",
			setup: func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options {
				target := globalAgent(t, repo, "a", "body\n")
				hash := mustHash(t, target)
				path := agentPath(base, project, "a")
				mustLink(t, target, path)
				if err := os.Remove(target); err != nil {
					t.Fatalf("removing the target: %v", err)
				}
				recordCreated(t, st, project, "a", path, target, hash)
				return Options{Prune: true}
			},
			disturb: func(t *testing.T, base string, repo *scan.Repo) {
				if err := os.Remove(agentPath(base, project, "a")); err != nil {
					t.Fatalf("removing the link: %v", err)
				}
			},
			mentions: "re-checking",
		},
		{
			name: "the link to adopt stopped being one",
			setup: func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options {
				target := globalAgent(t, repo, "a", "body\n")
				mustLink(t, target, agentPath(base, project, "a"))
				return Options{Adopt: true}
			},
			disturb: func(t *testing.T, base string, repo *scan.Repo) {
				path := agentPath(base, project, "a")
				if err := os.Remove(path); err != nil {
					t.Fatalf("removing the link: %v", err)
				}
				mustWrite(t, path, "a real file now\n")
			},
			mentions: "no longer a symlink",
		},
		{
			name: "the capture to drop stopped being a file",
			setup: func(t *testing.T, base string, repo *scan.Repo, st *state.File) Options {
				testfs.GitRepo(t, repo.Root, "project-agents/Web/proj/cap.md", "captured\n")
				return Options{Drops: []state.Key{{Project: project, Name: "cap"}}}
			},
			disturb: func(t *testing.T, base string, repo *scan.Repo) {
				capture := repo.BackupPath(project, "cap")
				if err := os.Remove(capture); err != nil {
					t.Fatalf("removing the capture: %v", err)
				}
				mustMkdir(t, capture)
			},
			mentions: "no longer a regular file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			st := state.New()
			opts := tc.setup(t, base, repo, st)

			plan := planFor(t, base, repo, st, opts)
			if !plan.Pending() {
				t.Fatalf("nothing pending, so this proves nothing: %v", plan.Items)
			}
			tc.disturb(t, base, repo)

			done, errs := plan.Apply(repo, st)
			if len(errs) != 1 {
				t.Fatalf("want exactly one refusal, got %v", errs)
			}
			if !strings.Contains(errs[0].Error(), tc.mentions) {
				t.Errorf("refusal %q does not mention %q", errs[0], tc.mentions)
			}
			if n := len(done.Pruned) + len(done.Adopted) + len(done.Dropped); n != 0 {
				t.Errorf("acted on %d subjects it had not re-examined", n)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{in: "planner"},
		{in: "senior-golang-dev"},
		{in: "", wantErr: true},
		{in: ".", wantErr: true},
		{in: "..", wantErr: true},
		{in: "sub/agent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if err := validName(tc.in); (err != nil) != tc.wantErr {
				t.Errorf("validName(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// --- the two round-2 review findings ----------------------------------------

// TestNewPlan_RekeyGateSeesAnAliasedLinkTarget.
//
// A link naming a capture through a symlinked path component records a
// LinkTarget that is not under the capture directory and a LinkResolved that
// is. Consulting only the first made the link invisible to the gate: it was
// neither repointed nor refused, so the move went ahead and left it dangling
// while the report said the links had been repointed.
//
// Both spellings of the same link must reach the same verdict, so the test
// asserts on the pair rather than on one of them.
func TestNewPlan_RekeyGateSeesAnAliasedLinkTarget(t *testing.T) {
	const (
		oldKey = "Web/old-name"
		newKey = "Web/new-name"
	)

	// spell builds the target path a link will name, given the repo root and
	// an alias of it that resolves to the same directory.
	cases := []struct {
		name  string
		spell func(repoRoot, alias string) string
	}{
		{
			name:  "spelled through the real repo root",
			spell: func(repoRoot, alias string) string { return repoRoot },
		},
		{
			name:  "spelled through a symlink to the repo root",
			spell: func(repoRoot, alias string) string { return alias },
		},
	}

	for _, tc := range cases {
		t.Run("another project links in: "+tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			alias := filepath.Join(base, "repolink")
			mustLink(t, repo.Root, alias)

			mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			root := tc.spell(repo.Root, alias)
			target := filepath.Join(root, "project-agents", filepath.FromSlash(oldKey), "a.md")
			mustLink(t, target, agentPath(base, "Web/somebody-else", "a"))

			plan := planFor(t, base, repo, state.New(), Options{
				Rekeys: []KeyMove{{Old: oldKey, New: newKey}},
			})
			if len(plan.Rekeys) != 0 {
				t.Fatalf("the move was authorised while another project still links into it: %v", plan.Rekeys)
			}
			if len(plan.Refused) != 1 {
				t.Fatalf("want exactly one refusal, got %v", plan.Refused)
			}
			if got := plan.Refused[0].Guard; got != GuardCaptureInUse {
				t.Errorf("guard = %q, want %q (%s)", got, GuardCaptureInUse, plan.Refused[0].Reason)
			}
		})

		t.Run("the destination project links in: "+tc.name, func(t *testing.T) {
			base, repo := fixture(t)
			alias := filepath.Join(base, "repolink")
			mustLink(t, repo.Root, alias)

			capture := mustWrite(t, repo.BackupPath(oldKey, "a"), "captured\n")
			root := tc.spell(repo.Root, alias)
			target := filepath.Join(root, "project-agents", filepath.FromSlash(oldKey), "a.md")
			path := agentPath(base, newKey, "a")
			mustLink(t, target, path)

			st := state.New()
			recordCreated(t, st, newKey, "a", path, target, mustHash(t, capture))

			plan := planFor(t, base, repo, st, Options{
				Rekeys: []KeyMove{{Old: oldKey, New: newKey}},
			})
			if len(plan.Refused) != 0 {
				t.Fatalf("refused: %v", plan.Refused)
			}
			if len(plan.Rekeys) != 1 || len(plan.Rekeys[0].Repoint) != 1 {
				t.Fatalf("the link was not scheduled to follow the captures: %v", plan.Rekeys)
			}

			done, errs := plan.Apply(repo, st)
			noErrs(t, errs)
			if len(done.Repointed) != 1 {
				t.Fatalf("repointed %v, want the one link", done.Repointed)
			}

			// The whole point: it still resolves afterwards.
			newCapture := repo.BackupPath(newKey, "a")
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatalf("the link no longer resolves after the move: %v", err)
			}
			if resolved != newCapture {
				t.Errorf("the link resolves to %s, want %s", resolved, newCapture)
			}
			if rec, ok := st.LookupLink(path); !ok || rec.Target != newCapture {
				t.Errorf("record target = %v (found %v), want %s", rec.Target, ok, newCapture)
			}
		})
	}
}

// TestNewPlan_AMovedProjectIsNotADecline.
//
// `mv $HOME/proj $HOME/proj2` removes every recorded link's path at once. Read
// as row three of the table, that writes a declined marker for every agent in
// the project and drops every record — suppressing agents nobody declined, by
// way of the one write no flag guards. An absent agents directory is evidence
// about the project; only an absent link inside a directory that is still there
// is row three.
func TestNewPlan_AMovedProjectIsNotADecline(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	other := globalAgent(t, repo, "b", "other\n")
	hash, otherHash := mustHash(t, target), mustHash(t, other)
	pathA, pathB := agentPath(base, project, "a"), agentPath(base, project, "b")
	mustLink(t, target, pathA)
	mustLink(t, other, pathB)

	st := state.New()
	recordCreated(t, st, project, "a", pathA, target, hash)
	recordCreated(t, st, project, "b", pathB, other, otherHash)

	// The move. Both recorded paths vanish at the same instant.
	if err := os.Rename(filepath.Join(base, "Web", "proj"), filepath.Join(base, "Web", "proj2")); err != nil {
		t.Fatalf("moving the project: %v", err)
	}

	plan := planFor(t, base, repo, st, Options{})
	for _, name := range []string{"a", "b"} {
		item := itemFor(t, plan, project, name)
		if item.Op != OpProjectGone {
			t.Fatalf("%s: op = %s, want %s (reason: %s)", name, item.Op, OpProjectGone, item.Reason)
		}
		if !strings.Contains(item.Reason, "moved") {
			t.Errorf("%s: reason does not offer the explanation: %q", name, item.Reason)
		}
	}
	if n := len(plan.OfOp(OpDecline)); n != 0 {
		t.Fatalf("%d declines planned for a project that merely moved", n)
	}
	if len(plan.Problems) != 0 {
		t.Errorf("a move is a report, not a failure to look: %v", plan.Problems)
	}

	done, errs := plan.Apply(repo, st)
	noErrs(t, errs)
	if len(done.Declined) != 0 {
		t.Errorf("markers were written for a moved project: %v", done.Declined)
	}
	if len(st.AllDeclined()) != 0 {
		t.Errorf("the document holds markers nobody asked for: %v", st.AllDeclined())
	}
	// The records survive: they still describe links that exist, at the old
	// path, and dropping them would make the ones at the new path unownable.
	if len(st.Records()) != 2 {
		t.Errorf("records = %d, want both kept", len(st.Records()))
	}
}

// TestNewPlan_ADeletedLinkInALiveProjectIsStillADecline: the other side of the
// same split. Narrowing row three must not narrow it out of existence.
func TestNewPlan_ADeletedLinkInALiveProjectIsStillADecline(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	kept := globalAgent(t, repo, "b", "other\n")
	pathA := agentPath(base, project, "a")
	// A sibling agent keeps the directory alive, which is exactly the
	// difference from the moved-project case.
	mustLink(t, kept, agentPath(base, project, "b"))

	st := state.New()
	recordCreated(t, st, project, "a", pathA, target, mustHash(t, target))

	item := itemFor(t, planFor(t, base, repo, st, Options{}), project, "a")
	if item.Op != OpDecline {
		t.Fatalf("op = %s, want %s (reason: %s)", item.Op, OpDecline, item.Reason)
	}
}

// TestNewPlan_AnAgentsDirectoryThatIsNotADirectoryIsAProblem: the tool looked
// and got an answer it cannot reason from, which is exit 2 rather than either
// verdict.
func TestNewPlan_AnAgentsDirectoryThatIsNotADirectoryIsAProblem(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")
	path := agentPath(base, project, "a")
	mustMkdir(t, filepath.Dir(filepath.Dir(path)))
	mustWrite(t, filepath.Dir(path), "a file where the agents directory should be\n")

	st := state.New()
	recordCreated(t, st, project, "a", path, target, mustHash(t, target))

	plan := planFor(t, base, repo, st, Options{})
	assertNoItem(t, plan, project, "a")
	if len(plan.Problems) == 0 {
		t.Fatal("no problem recorded for an agents directory that is not one")
	}
}

// TestNewPlan_ASymlinkedAgentsDirectoryIsNotAMovedProject.
//
// ProjectKey deliberately does not resolve <project>/.claude/agents, so a
// symlinked agents directory is a supported layout. Checking it with Lstat
// alone would call every project using one "gone" and silence a genuine row
// three across all of them.
func TestNewPlan_ASymlinkedAgentsDirectoryIsNotAMovedProject(t *testing.T) {
	const project = "Web/proj"
	base, repo := fixture(t)

	target := globalAgent(t, repo, "a", "body\n")

	shared := filepath.Join(base, "shared-agents")
	mustMkdir(t, shared)
	agentsDir := filepath.Join(base, "Web", "proj", ".claude", "agents")
	mustMkdir(t, filepath.Dir(agentsDir))
	mustLink(t, shared, agentsDir)

	st := state.New()
	recordCreated(t, st, project, "a", filepath.Join(agentsDir, "a.md"), target, mustHash(t, target))

	item := itemFor(t, planFor(t, base, repo, st, Options{}), project, "a")
	if item.Op != OpDecline {
		t.Fatalf("op = %s, want %s — a symlinked agents directory is present, not gone (reason: %s)",
			item.Op, OpDecline, item.Reason)
	}
}
