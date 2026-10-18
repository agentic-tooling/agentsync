package linker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/state"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

func assertNoTemps(t *testing.T, dir, prefix string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return
		}
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			t.Errorf("temporary left behind in %s: %s", dir, e.Name())
		}
	}
}

func assertAbsent(t *testing.T, path, why string) {
	t.Helper()
	if fi, err := os.Lstat(path); err == nil {
		t.Errorf("%s exists (%v); %s", path, fi.Mode(), why)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lstat %s: %v", path, err)
	}
}

// oneError requires exactly one error, mentioning want. It does not stop the
// test, so the filesystem checks after it still report what a bad run did.
func oneError(t *testing.T, errs []error, want string) {
	t.Helper()
	if len(errs) != 1 {
		t.Errorf("errs = %v, want exactly one", errs)
	}
	if len(errs) > 0 && !strings.Contains(errs[0].Error(), want) {
		t.Errorf("error = %q, want it to mention %q", errs[0], want)
	}
}

// --- recheck ----------------------------------------------------------------

// TestApply_RecheckRefusesWhatChangedSinceThePlan perturbs one fact between
// plan and apply per case. Each refusal must leave the project exactly as the
// perturbation left it, with no link, no record and no temporary.
func TestApply_RecheckRefusesWhatChangedSinceThePlan(t *testing.T) {
	const body = "canonical shared agent\n"

	type world struct {
		f    *fixture
		plan Plan
	}
	// adoptable plans an adoption of proj/shared against the global agent.
	adoptable := func(t *testing.T) (world, string, string) {
		f := newFixture(t)
		target := f.global("shared", body)
		path := f.project("proj", "shared", body)
		return world{f: f, plan: f.plan(Options{})}, path, target
	}
	// creatable plans a create of fresh/shared into a project with no agents dir.
	creatable := func(t *testing.T) (world, string) {
		f := newFixture(t)
		f.global("shared", body)
		mustMkdir(t, filepath.Join(f.base, "fresh"))
		p := f.plan(Options{Projects: []string{"fresh"}, Agents: []string{"shared"}})
		return world{f: f, plan: p}, filepath.Join(f.agentsDir("fresh"), "shared.md")
	}

	tests := []struct {
		name string
		want string
		// arrange builds the plan, perturbs the disk, and returns the check
		// that the refusal left everything alone.
		arrange func(t *testing.T) (world, func(t *testing.T))
	}{
		{
			name: "target is no longer a regular file",
			want: "not a regular file",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path, target := adoptable(t)
				// Same bytes behind the link, so both re-hashes pass and only
				// the regular-file check refuses it.
				elsewhere := filepath.Join(w.f.base, "elsewhere", "shared.md")
				mustWrite(t, elsewhere, body)
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, target); err != nil {
					t.Fatal(err)
				}
				return w, func(t *testing.T) {
					assertRegular(t, path, "a link to a symlinked target must not replace the project file")
				}
			},
		},
		{
			name: "project path cannot be examined",
			want: "re-checking",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path := creatable(t)
				// A file where .claude should be makes Lstat fail with
				// ENOTDIR, which is not "absent" and must not be read as it.
				claude := filepath.Dir(filepath.Dir(path))
				mustWrite(t, claude, "not a directory\n")
				return w, func(t *testing.T) {
					if got := mustRead(t, claude); got != "not a directory\n" {
						t.Errorf("%s was rewritten: %q", claude, got)
					}
				}
			},
		},
		{
			name: "adopted file vanished",
			want: "vanished",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path, _ := adoptable(t)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return w, func(t *testing.T) {
					assertAbsent(t, path, "an adoption must not turn into a create")
				}
			},
		},
		{
			name: "created path appeared",
			want: "appeared",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path := creatable(t)
				mustWrite(t, path, "written while the scan ran\n")
				return w, func(t *testing.T) {
					if got := mustRead(t, path); got != "written while the scan ran\n" {
						t.Errorf("the new file was replaced: %q", got)
					}
				}
			},
		},
		{
			name: "adopted file became a symlink",
			want: "no longer a regular file",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path, _ := adoptable(t)
				// Same bytes as the plan hashed, so only the regular-file
				// check stands between this link and being replaced.
				elsewhere := filepath.Join(w.f.base, "elsewhere", "shared.md")
				mustWrite(t, elsewhere, body)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, path); err != nil {
					t.Fatal(err)
				}
				return w, func(t *testing.T) {
					if got, err := os.Readlink(path); err != nil || got != elsewhere {
						t.Errorf("readlink %s = %q, %v; want the hand-made link to %s intact", path, got, err, elsewhere)
					}
				}
			},
		},
		{
			name: "adoption planned without a hash",
			want: "without a content hash",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path, _ := adoptable(t)
				w.plan.Items[0].Hash = ""
				return w, func(t *testing.T) {
					assertRegular(t, path, "an unverified file must not be replaced")
				}
			},
		},
		{
			name: "target content changed",
			want: "no longer matches",
			arrange: func(t *testing.T) (world, func(t *testing.T)) {
				w, path, target := adoptable(t)
				mustWrite(t, target, "the repo's agent was rewritten\n")
				return w, func(t *testing.T) {
					assertRegular(t, path, "a file no longer identical to its target must not be replaced")
					if got := mustRead(t, path); got != body {
						t.Errorf("project file = %q, want %q", got, body)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, check := tt.arrange(t)
			if n := len(w.plan.Writes()); n != 1 {
				t.Fatalf("fixture planned %d writes, want 1", n)
			}
			item := w.plan.Writes()[0]

			done, errs := w.plan.Apply(w.f.repo, w.f.state)
			oneError(t, errs, tt.want)
			if len(done.Links) != 0 || len(done.Recorded) != 0 {
				t.Errorf("linked %v and recorded %v after a refusal", done.Links, done.Recorded)
			}
			assertAbsent(t, state.Path(w.f.repo), "a refused run has nothing to record")
			assertNoTemps(t, filepath.Dir(item.Path), ".agentsync-link-")
			check(t)
		})
	}
}

// --- exclusions -------------------------------------------------------------

// TestApply_UnwritableExclusionLinksNothingInThatProject: the exclusion is the
// condition under which the project was permitted at all. Other projects are
// independent and still get linked.
func TestApply_UnwritableExclusionLinksNothingInThatProject(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	sibling := f.project("other", "shared", "canonical shared agent\n")

	plan := f.plan(Options{ExcludeLocal: true})
	if len(plan.Excludes) != 1 || len(plan.Writes()) != 2 {
		t.Fatalf("fixture planned excludes %v and writes %v, want 1 and 2", plan.Excludes, plan.Writes())
	}
	// A file where .git/info should be: MkdirAll cannot create through it.
	info := filepath.Dir(f.excludeFile("tracked"))
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, info, "not a directory\n")

	done, errs := plan.Apply(f.repo, f.state)
	oneError(t, errs, "creating")
	if len(done.Excludes) != 0 {
		t.Errorf("reported exclusions %v that were not written", done.Excludes)
	}
	assertRegular(t, path, "a project whose exclusion failed must not be linked into")
	if _, ok := f.state.LookupLink(path); ok {
		t.Error("recorded a link in the project whose exclusion failed")
	}
	if len(done.Links) != 1 || done.Links[0].Path != sibling {
		t.Errorf("links = %v, want the unrelated sibling alone", done.Links)
	}
	assertLink(t, sibling, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

func TestApply_UnreadableExcludeFileIsNotOverwritten(t *testing.T) {
	f, plan, path, exclude := excludePlan(t)
	// A directory where the exclude file should be: reading it fails with
	// EISDIR, which is not "absent" and must not be treated as an empty file.
	if err := os.Remove(exclude); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(exclude, "kept")
	mustWrite(t, kept, "inside\n")

	done, errs := plan.Apply(f.repo, f.state)
	oneError(t, errs, "reading")
	if len(done.Excludes) != 0 || len(done.Links) != 0 {
		t.Errorf("wrote excludes %v and links %v", done.Excludes, done.Links)
	}
	if got := mustRead(t, kept); got != "inside\n" {
		t.Errorf("%s = %q, want it untouched", kept, got)
	}
	assertRegular(t, path, "nothing may be linked when the exclusion failed")
	assertNoTemps(t, filepath.Dir(exclude), ".agentsync-exclude-")
}

// TestApply_ExclusionAddedSinceThePlanIsNotDuplicated: writeExclude re-reads
// the file instead of trusting the plan.
func TestApply_ExclusionAddedSinceThePlanIsNotDuplicated(t *testing.T) {
	f, plan, path, exclude := excludePlan(t)
	const by = "*.local\n/.claude/agents/\n"
	mustWrite(t, exclude, by)

	done, errs := plan.Apply(f.repo, f.state)
	for _, err := range errs {
		t.Errorf("Apply: %v", err)
	}
	if got := mustRead(t, exclude); got != by {
		t.Errorf("exclude file = %q, want it byte-identical to %q", got, by)
	}
	if len(done.Links) != 1 {
		t.Errorf("links = %v, want the adoption", done.Links)
	}
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// TestApply_ExcludeFileIsCreatedWhenMissing: a repository that never had a
// local exclusion has no info/exclude, and that is not a refusal.
func TestApply_ExcludeFileIsCreatedWhenMissing(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	info := filepath.Dir(f.excludeFile("tracked"))
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}

	plan := f.plan(Options{Projects: []string{"tracked"}, ExcludeLocal: true})
	if len(plan.Blocked) != 0 || len(plan.Excludes) != 1 {
		t.Fatalf("blocked %v, excludes %v; want an exclusion planned", plan.Blocked, plan.Excludes)
	}
	applyOK(t, f, plan)
	if body := mustRead(t, f.excludeFile("tracked")); !strings.Contains(body, "\n/.claude/agents/\n") {
		t.Errorf("exclude file = %q, want the pattern on its own line", body)
	}
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// TestWriteExclude_FailedWriteIsReported. In this fixture the project's own
// path plus "/exclude" fits the platform limit, but every temporary name in it
// does not, so CreateTemp fails even for root.
func TestWriteExclude_FailedWriteIsReported(t *testing.T) {
	project := filepath.Dir(filepath.Dir(testfs.UnreadableAgentsDir(t, t.TempDir())))
	ex := Exclude{Project: "p", File: filepath.Join(project, "exclude"), Pattern: "/.claude/agents/"}

	err := writeExclude(ex)
	if !errors.Is(err, syscall.ENAMETOOLONG) || !strings.Contains(err.Error(), "excluding") {
		t.Fatalf("err = %v, want the exclusion's ENAMETOOLONG", err)
	}
	assertAbsent(t, ex.File, "a failed write must not leave an exclude file")
	assertNoTemps(t, project, ".agentsync-exclude-")
}

// --- writeAtomic ------------------------------------------------------------

func TestWriteAtomic_MissingDirectoryFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "exclude")
	if err := writeAtomic(path, "x\n"); err == nil {
		t.Fatal("writeAtomic succeeded into a directory that does not exist")
	}
	assertAbsent(t, filepath.Dir(path), "writeAtomic does not create directories")
}

// TestWriteAtomic_FailedRenameRemovesTheTemporary. A non-empty directory at the
// destination makes the rename fail after the temporary is fully written.
func TestWriteAtomic_FailedRenameRemovesTheTemporary(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "exclude")
	mustWrite(t, filepath.Join(dest, "kept"), "inside\n")

	err := writeAtomic(dest, "new content\n")
	if err == nil {
		t.Fatal("writeAtomic replaced a non-empty directory")
	}
	if !strings.Contains(err.Error(), "renaming") {
		t.Errorf("error = %v, want the rename named", err)
	}
	if got := mustRead(t, filepath.Join(dest, "kept")); got != "inside\n" {
		t.Errorf("the destination was disturbed: %q", got)
	}
	assertNoTemps(t, dir, ".agentsync-exclude-")
}

// --- symlink writes ---------------------------------------------------------

// TestSymlinkExclusive_NeverReplacesAnExistingFile is the guarantee linkOne
// relies on for create: recheck gives the message, EEXIST is the safety.
func TestSymlinkExclusive_NeverReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.md")
	mustWrite(t, target, "target\n")
	path := filepath.Join(dir, "agents", "shared.md")
	mustWrite(t, path, "already here\n")

	err := symlinkExclusive(target, path)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want EEXIST", err)
	}
	assertRegular(t, path, "an exclusive create must not replace a file")
	if got := mustRead(t, path); got != "already here\n" {
		t.Errorf("%s = %q", path, got)
	}
}

func TestSymlinkWrites_FailWhenTheDirectoryCannotBeMade(t *testing.T) {
	for name, write := range map[string]func(target, path string) error{
		"symlinkExclusive": symlinkExclusive,
		"ReplaceLink":      ReplaceLink,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			blocker := filepath.Join(dir, "project", ".claude")
			mustWrite(t, blocker, "not a directory\n")

			err := write(filepath.Join(dir, "target.md"), filepath.Join(blocker, "agents", "shared.md"))
			if err == nil {
				t.Fatal("created a link beneath a regular file")
			}
			// The directory itself, not the link beneath it: the Symlink that
			// follows also fails with a "creating" message.
			if want := "creating " + strconv.Quote(filepath.Join(blocker, "agents")); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %s", err, want)
			}
			if got := mustRead(t, blocker); got != "not a directory\n" {
				t.Errorf("%s = %q", blocker, got)
			}
		})
	}
}

// TestApply_CreateThroughADanglingAgentsDirectoryIsReported reaches linkOne's
// write-failure arm. Lstat through a dangling link answers ENOENT, so recheck
// passes, and MkdirAll then refuses to build over the link.
func TestApply_CreateThroughADanglingAgentsDirectoryIsReported(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	mustMkdir(t, filepath.Join(f.base, "fresh"))
	plan := f.plan(Options{Projects: []string{"fresh"}, Agents: []string{"shared"}})
	assertActions(t, plan, map[string]Action{"fresh/shared": ActionCreate})

	dir := f.agentsDir("fresh")
	gone := filepath.Join(f.base, "unmounted", "agents")
	mustMkdir(t, filepath.Dir(dir))
	if err := os.Symlink(gone, dir); err != nil {
		t.Fatal(err)
	}

	done, errs := plan.Apply(f.repo, f.state)
	oneError(t, errs, "linking fresh/shared")
	if len(done.Links) != 0 || len(done.Recorded) != 0 {
		t.Errorf("linked %v and recorded %v after a failed write", done.Links, done.Recorded)
	}
	if got, err := os.Readlink(dir); err != nil || got != gone {
		t.Errorf("readlink %s = %q, %v; want the dangling link left as it was", dir, got, err)
	}
	assertAbsent(t, gone, "nothing may be built at the far end of the link")
}

// TestReplaceLink_TemporaryLinkFailureIsReturned. The directory's own path fits
// the platform limit and any temporary name inside it does not, so the
// failure holds for root too.
func TestReplaceLink_TemporaryLinkFailureIsReturned(t *testing.T) {
	dir := filepath.Dir(testfs.UnreadableAgentsDir(t, t.TempDir()))

	err := ReplaceLink(filepath.Join(t.TempDir(), "target.md"), filepath.Join(dir, "agent.md"))
	if !errors.Is(err, syscall.ENAMETOOLONG) || !strings.Contains(err.Error(), "temporary link") {
		t.Fatalf("err = %v, want the temporary link's ENAMETOOLONG", err)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(entries) != 1 || entries[0].Name() != "agents" {
		t.Errorf("%s now holds %v, want only the fixture's agents directory", dir, entries)
	}
}

// TestReplaceLink_FailedRenameRemovesTheTemporary. The temporary sits in a
// project's agents directory, where a stray one would be read as an agent.
func TestReplaceLink_FailedRenameRemovesTheTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.md")
	kept := filepath.Join(path, "kept")
	mustWrite(t, kept, "inside\n")

	err := ReplaceLink(filepath.Join(dir, "target.md"), path)
	if err == nil || !strings.Contains(err.Error(), "renaming") {
		t.Fatalf("err = %v, want the rename's failure", err)
	}
	if got := mustRead(t, kept); got != "inside\n" {
		t.Errorf("the destination was disturbed: %q", got)
	}
	assertNoTemps(t, dir, ".agentsync-link-")
}

// TestTempSymlink_OnlyRetriesATakenName: any failure other than EEXIST is
// returned at once rather than burning the retries.
func TestTempSymlink_OnlyRetriesATakenName(t *testing.T) {
	_, err := tempSymlink(filepath.Join(t.TempDir(), "absent"), "target")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the ENOENT itself", err)
	}
}

// --- recording --------------------------------------------------------------

// TestRecord_UnhashableTargetIsReportedNotRecorded: an empty hash would match
// nothing, so the link is left unowned and the failure returned.
func TestRecord_UnhashableTargetIsReportedNotRecorded(t *testing.T) {
	dir := t.TempDir()
	st := state.New()
	var into []state.Record

	errs := record(st, []Item{{
		Project: "proj",
		Name:    "shared",
		Path:    filepath.Join(dir, "shared.md"),
		Target:  filepath.Join(dir, "missing.md"),
		Action:  ActionCreate,
	}}, &into)
	oneError(t, errs, "hashing")
	if len(into) != 0 || len(st.Records()) != 0 || st.Dirty() {
		t.Errorf("recorded %v (document %v) for a target that could not be hashed", into, st.Records())
	}
}

// TestApply_RecordFailureAfterTheLinkIsReported: the link exists, the record
// was refused, and the run must say so rather than claim a clean apply.
func TestApply_RecordFailureAfterTheLinkIsReported(t *testing.T) {
	f := newFixture(t)
	target := f.global("shared", "canonical shared agent\n")
	path := filepath.Join(t.TempDir(), "shared.md")
	// A hand-made item with no project key: state refuses to record it.
	plan := Plan{Items: []Item{{Name: "shared", Path: path, Target: target, Action: ActionCreate}}}

	done, errs := plan.Apply(f.repo, f.state)
	oneError(t, errs, "recording")
	if len(done.Links) != 1 || len(done.Recorded) != 0 {
		t.Errorf("linked %v and recorded %v, want the link and no record", done.Links, done.Recorded)
	}
	assertLink(t, path, target)
	assertAbsent(t, state.Path(f.repo), "nothing was recorded")
}

func TestApply_StateSaveFailureIsReported(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("proj", "shared", body)
	// A non-empty directory where the state file goes makes Save's rename fail.
	mustWrite(t, filepath.Join(state.Path(f.repo), "kept"), "inside\n")

	done, errs := f.plan(Options{}).Apply(f.repo, f.state)
	oneError(t, errs, state.FileName)
	if len(done.Links) != 1 {
		t.Errorf("links = %v, want the adoption", done.Links)
	}
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
	assertNoTemps(t, f.repoRoot, ".agentsync-state-")
}
