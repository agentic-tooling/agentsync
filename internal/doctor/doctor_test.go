package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// fixture is a throwaway world: an agent repo, and projects beside it under a
// base directory that stands in for $HOME.
//
// Every test below drives the real classifier over real directories rather than
// hand-building entries, for the same reason the linker's tests do: the states
// doctor reports on differ from each other only in what os.Lstat says about a
// path, so an entry asserting a state the filesystem does not actually produce
// would test the wrong thing exactly where it matters.
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
	mustMkdir(t, filepath.Join(repoRoot, capturesDir))

	repo, err := scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}

	// The base is resolved so paths built here compare equal to the ones the
	// scan reports; on macOS a temp dir is reached through /var, a symlink.
	// Home is pointed at it so project keys land in the home-relative
	// namespace the tests describe rather than under _outside-home/.
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("resolving %s: %v", base, err)
	}
	repo.Home = resolved

	return &fixture{t: t, base: resolved, repoRoot: repo.Root, repo: repo}
}

func (f *fixture) agentsDir(project string) string {
	return filepath.Join(f.base, filepath.FromSlash(project), ".claude", "agents")
}

// project writes a real file into a project's .claude/agents directory.
func (f *fixture) project(project, name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.agentsDir(project), name+".md")
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

// capture writes a file where an import would put it: this project's captured
// copy under project-agents/<key>/.
func (f *fixture) capture(project, name, content string) string {
	f.t.Helper()
	p := f.repo.BackupPath(project, name)
	mustWrite(f.t, p, content)
	return p
}

// symlink puts a link into a project's agents directory. The target need not
// exist — that is what a broken link is.
func (f *fixture) symlink(project, name, target string) string {
	f.t.Helper()
	p := filepath.Join(f.agentsDir(project), name+".md")
	mustMkdir(f.t, filepath.Dir(p))
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
	return p
}

// mkProject creates a project directory with no agents in it, which is what
// makes its captures stale rather than orphaned.
func (f *fixture) mkProject(project string) string {
	f.t.Helper()
	dir := filepath.Join(f.base, filepath.FromSlash(project))
	mustMkdir(f.t, dir)
	return dir
}

func (f *fixture) diagnose() Report {
	f.t.Helper()
	res, err := f.repo.Scan(f.base)
	if err != nil {
		f.t.Fatalf("Scan: %v", err)
	}
	if len(res.Problems) > 0 {
		f.t.Fatalf("fixture scan reported problems: %v", res.Problems)
	}
	rep := Diagnose(f.repo, res)
	if len(rep.Problems) > 0 {
		f.t.Fatalf("fixture diagnosis reported problems: %v", rep.Problems)
	}
	return rep
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

// only asserts that the report holds exactly one finding of this kind and
// returns it, which is what nearly every test below wants to inspect.
func only(t *testing.T, rep Report, kind Kind) Finding {
	t.Helper()
	group := rep.OfKind(kind)
	if len(group) != 1 {
		t.Fatalf("want exactly one %s finding, got %d\nfindings: %v", kind, len(group), rep.Findings)
	}
	return group[0]
}

func assertNoFindings(t *testing.T, rep Report) {
	t.Helper()
	if len(rep.Findings) != 0 {
		t.Fatalf("want a clean report, got %v", rep.Findings)
	}
}

func assertPresent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s is gone (%v); %s", path, err, why)
	}
}

// --- links ------------------------------------------------------------------

// A broken link is the case where an unlink looks most obviously right and is
// most obviously wrong: the missing target is a file this repo can restore from
// git history, and unlinking would throw away the record of which project
// wanted it.
func TestDiagnose_BrokenLinkIsReportedAndNeverUnlinked(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.repoRoot, "agents", "renamed-away.md")
	link := f.symlink("proj", "renamed-away", missing)

	rep := f.diagnose()

	found := only(t, rep, KindBrokenLink)
	if found.Project != "proj" || found.Name != "renamed-away" {
		t.Errorf("finding names %s/%s, want proj/renamed-away", found.Project, found.Name)
	}
	if found.Path != link {
		t.Errorf("finding path = %s, want %s", found.Path, link)
	}
	if found.Target != missing {
		t.Errorf("finding target = %s, want the missing target %s", found.Target, missing)
	}
	if !found.Kind.NeedsHuman() {
		t.Error("a broken link must need a human")
	}

	assertPresent(t, link, "doctor never unlinks")
}

// A foreign link is somebody else's and is reported for completeness only. If it
// counted as needing a human, every machine running another agent tool would
// fail forever and the exit code would stop meaning anything.
func TestDiagnose_ForeignLinkIsReportedButNeedsNoHuman(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(f.base, "elsewhere", "somebody-elses.md")
	mustWrite(t, elsewhere, "not ours\n")
	link := f.symlink("proj", "somebody-elses", elsewhere)

	rep := f.diagnose()

	found := only(t, rep, KindForeignLink)
	if found.Path != link {
		t.Errorf("finding path = %s, want %s", found.Path, link)
	}
	if found.Kind.NeedsHuman() {
		t.Error("a foreign link must not need a human")
	}
	if n := len(rep.NeedsHuman()); n != 0 {
		t.Errorf("NeedsHuman() = %d, want 0: a foreign link alone is a clean run", n)
	}

	assertPresent(t, link, "doctor never touches a foreign link")
}

// A healthy link pointing at this project's counterpart is the whole point of
// the tool working, and must produce nothing at all.
func TestDiagnose_HealthyLinkIsSilent(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	target := f.global("shared", body)
	f.symlink("proj", "shared", target)

	assertNoFindings(t, f.diagnose())
}

// A mismatch is the residual override: the project is served the global agent
// while its own captured copy sits unused. The remedy is link's, verbatim,
// because it names link's own flags and a second copy would go stale the moment
// one is renamed.
func TestDiagnose_MismatchReportsTheCounterpartAndLinksRemedy(t *testing.T) {
	f := newFixture(t)
	global := f.global("web-researcher", "the global one\n")
	want := f.capture("proj", "web-researcher", "this project's customized one\n")
	f.symlink("proj", "web-researcher", global)

	rep := f.diagnose()

	found := only(t, rep, KindMismatch)
	if found.Target != want {
		t.Errorf("finding target = %s, want this project's counterpart %s", found.Target, want)
	}
	if !strings.Contains(found.Detail, global) {
		t.Errorf("the detail does not name the file the link actually points at:\n%s", found.Detail)
	}
	for _, needed := range []string{
		"--" + linker.ProjectFlag, "proj",
		"--" + linker.AgentFlag, "web-researcher",
	} {
		if !strings.Contains(found.Detail, needed) {
			t.Errorf("the detail does not carry link's remedy (missing %q):\n%s", needed, found.Detail)
		}
	}
}

// A real file is scan's and import's business, not doctor's. Reporting an
// override here would duplicate a report the user already has, under a name
// that implies drift rather than a deliberate customization.
func TestDiagnose_RealFilesAreNotDoctorsBusiness(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "the global one\n")
	f.project("proj", "shared", "a deliberate local customization\n") // state 6
	f.project("proj", "only-here", "no repo copy at all\n")           // state 7

	rep := f.diagnose()

	// The captures for these two are missing on purpose: an uncaptured
	// override is import's finding, not doctor's, and inventing a
	// stale-capture finding for a capture that was never made would be
	// nonsense.
	assertNoFindings(t, rep)
	if rep.Examined.Entries != 2 {
		t.Errorf("Examined.Entries = %d, want 2: both files were classified even though neither is a finding", rep.Examined.Entries)
	}
}

// --- captures ---------------------------------------------------------------

// Four captures from one project against an agents directory
// that no longer holds them.
func TestDiagnose_StaleCaptureIsReportedAndKept(t *testing.T) {
	f := newFixture(t)
	f.mkProject("Web/example")
	// The project still exists and still has an agents directory holding
	// something else — this is one deleted agent, not a gone project.
	target := f.global("senior-golang-dev", "still linked\n")
	f.symlink("Web/example", "senior-golang-dev", target)
	kept := f.capture("Web/example", "golang-developer", "a 44-line predecessor\n")

	rep := f.diagnose()

	found := only(t, rep, KindStaleCapture)
	if found.Project != "Web/example" || found.Name != "golang-developer" {
		t.Errorf("finding names %s/%s, want Web/example/golang-developer", found.Project, found.Name)
	}
	if found.Path != kept {
		t.Errorf("finding path = %s, want the capture %s", found.Path, kept)
	}
	wantLive := filepath.Join(f.agentsDir("Web/example"), "golang-developer.md")
	if found.Target != wantLive {
		t.Errorf("finding target = %s, want the missing live file %s", found.Target, wantLive)
	}
	if len(rep.OfKind(KindOrphanedCaptures)) != 0 {
		t.Error("a live project must never be reported as orphaned")
	}

	assertPresent(t, kept, "doctor never deletes a capture: it may be the only surviving copy")
	if body, err := os.ReadFile(kept); err != nil || string(body) != "a 44-line predecessor\n" {
		t.Errorf("the capture was modified: %q, %v", body, err)
	}
}

// The remedy a reader will imagine for a stale capture is rm, so the report has
// to say — in the line itself, not only in the summary — that this may be the
// only copy left.
func TestDiagnose_StaleCaptureDetailWarnsAgainstDeleting(t *testing.T) {
	f := newFixture(t)
	f.mkProject("proj")
	f.capture("proj", "gone-from-the-project", "the last copy\n")

	found := only(t, f.diagnose(), KindStaleCapture)
	if !strings.Contains(found.Detail, "only surviving copy") {
		t.Errorf("the detail does not warn that this may be the last copy:\n%s", found.Detail)
	}
}

// A whole missing project is a different fact from one missing file, and gets
// one finding for the directory rather than N repeats of the same sentence.
func TestDiagnose_OrphanedCaptureDirIsDistinctFromStaleFiles(t *testing.T) {
	f := newFixture(t)
	// No project directory at all. Two captures, so a per-file report would
	// be visibly the wrong shape.
	f.capture("Web/deleted-project", "one", "a\n")
	f.capture("Web/deleted-project", "two", "b\n")

	rep := f.diagnose()

	found := only(t, rep, KindOrphanedCaptures)
	if found.Project != "Web/deleted-project" {
		t.Errorf("finding names %s, want Web/deleted-project", found.Project)
	}
	if found.Name != "" {
		t.Errorf("an orphaned directory finding names an agent (%q); it is about the whole directory", found.Name)
	}
	wantDir := filepath.Join(f.repoRoot, capturesDir, "Web", "deleted-project")
	if found.Path != wantDir {
		t.Errorf("finding path = %s, want the capture directory %s", found.Path, wantDir)
	}
	if found.Target != filepath.Join(f.base, "Web", "deleted-project") {
		t.Errorf("finding target = %s, want the project directory that is missing", found.Target)
	}
	if n := len(rep.OfKind(KindStaleCapture)); n != 0 {
		t.Errorf("got %d stale-capture findings as well; a gone project is reported once, as a directory", n)
	}

	for _, name := range []string{"one", "two"} {
		assertPresent(t, f.repo.BackupPath("Web/deleted-project", name),
			"doctor never deletes an orphaned capture")
	}
}

// A live path that exists but is a broken symlink still means the project has
// that agent by name, so its capture is not stale. Getting this wrong needs only
// os.Stat in place of os.Lstat, and produces a stale report for a file that is
// plainly there.
func TestDiagnose_BrokenLiveLinkStillMeansTheCaptureIsNotStale(t *testing.T) {
	f := newFixture(t)
	f.mkProject("proj")
	f.symlink("proj", "renamed-away", filepath.Join(f.repoRoot, "agents", "renamed-away.md"))
	f.capture("proj", "renamed-away", "the captured copy\n")

	rep := f.diagnose()

	if n := len(rep.OfKind(KindStaleCapture)); n != 0 {
		t.Errorf("got %d stale-capture findings; the project still has the file, it is the link that is broken", n)
	}
	if n := len(rep.OfKind(KindBrokenLink)); n != 1 {
		t.Errorf("got %d broken-link findings, want 1", n)
	}
}

// project-agents/README.md documents the tree; it is not a capture of anything.
// Excluded by shape — a .md directly under project-agents/ has no project key
// above it — so any other documentation put beside it is excluded on the same
// grounds rather than by a hardcoded name.
func TestDiagnose_ReadmeAtTheCaptureRootIsNotACapture(t *testing.T) {
	f := newFixture(t)
	mustWrite(t, filepath.Join(f.repoRoot, capturesDir, "README.md"), "# project-agents\n")
	mustWrite(t, filepath.Join(f.repoRoot, capturesDir, "NOTES.md"), "also not a capture\n")

	rep := f.diagnose()

	assertNoFindings(t, rep)
	if rep.Examined.Captures != 0 {
		t.Errorf("Examined.Captures = %d, want 0: nothing under project-agents/ was a capture", rep.Examined.Captures)
	}
}

// project-agents/ is a tree, not one flat level: a directory there holds .md
// files if it is a project and subdirectories if it contains projects, and both
// when one project nests inside another. A one-level listing would miss the
// nested project entirely — and miss it silently, reporting the outer one clean.
func TestDiagnose_NestedProjectsUnderOneKeyAreBothWalked(t *testing.T) {
	f := newFixture(t)
	// Web/mono is a project in its own right AND the parent of another.
	f.mkProject("Web/mono")
	f.mkProject("Web/mono/pkg")
	f.capture("Web/mono", "outer", "a\n")
	f.capture("Web/mono/pkg", "inner", "b\n")

	rep := f.diagnose()

	if rep.Examined.Captures != 2 || rep.Examined.CaptureDirs != 2 {
		t.Errorf("examined %d captures in %d directories, want 2 in 2", rep.Examined.Captures, rep.Examined.CaptureDirs)
	}
	got := make(map[string]bool)
	for _, found := range rep.OfKind(KindStaleCapture) {
		got[found.Project+"/"+found.Name] = true
	}
	for _, want := range []string{"Web/mono/outer", "Web/mono/pkg/inner"} {
		if !got[want] {
			t.Errorf("%s was not reported; the walk did not reach it (findings: %v)", want, rep.Findings)
		}
	}
}

// A capture whose project still holds the file is the ordinary, healthy case and
// must be silent — otherwise every well-provisioned project produces noise.
func TestDiagnose_CaptureWithALiveFileIsSilent(t *testing.T) {
	f := newFixture(t)
	const body = "this project's own agent\n"
	f.project("proj", "local-only", body)
	f.capture("proj", "local-only", body)

	assertNoFindings(t, f.diagnose())
}

// A tree where nothing has drifted reports nothing, and says so through an empty
// finding list rather than through a count that happens to be zero.
func TestDiagnose_CleanTreeReportsNothing(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	target := f.global("shared", body)
	f.symlink("proj", "shared", target)
	f.mkProject("other")

	rep := f.diagnose()

	assertNoFindings(t, rep)
	if len(rep.NeedsHuman()) != 0 {
		t.Errorf("NeedsHuman() = %v, want none", rep.NeedsHuman())
	}
	if len(rep.Problems) != 0 {
		t.Errorf("Problems = %v, want none", rep.Problems)
	}
}

// A capture directory whose key names nothing this scheme could have produced is
// a problem, never a finding. Guessing at it would report a project as gone on
// the strength of a key we could not resolve.
func TestDiagnose_UnresolvableCaptureKeyIsAProblemNotAFinding(t *testing.T) {
	f := newFixture(t)
	// _outside-home is a namespace prefix, not a project: ProjectDir refuses
	// it rather than turning it into a path.
	mustWrite(t, filepath.Join(f.repoRoot, capturesDir, "_outside-home", "stray.md"), "x\n")

	res, err := f.repo.Scan(f.base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	rep := Diagnose(f.repo, res)

	if len(rep.Findings) != 0 {
		t.Errorf("Findings = %v, want none: an unresolvable key is not evidence of drift", rep.Findings)
	}
	if len(rep.Problems) != 1 {
		t.Fatalf("Problems = %v, want exactly one", rep.Problems)
	}
}

// --- read-only --------------------------------------------------------------

// The package's central claim, asserted rather than documented: a diagnosis
// leaves the world byte-identical, including creating no file anywhere.
func TestDiagnose_ChangesNothingOnDisk(t *testing.T) {
	f := newFixture(t)
	// One of everything doctor reports on, so the assertion covers every code
	// path that might be tempted to fix something.
	global := f.global("shared", "canonical\n")
	f.symlink("healthy", "shared", global)
	f.symlink("broken", "renamed-away", filepath.Join(f.repoRoot, "agents", "renamed-away.md"))
	mustWrite(t, filepath.Join(f.base, "elsewhere", "theirs.md"), "not ours\n")
	f.symlink("foreign", "theirs", filepath.Join(f.base, "elsewhere", "theirs.md"))
	f.capture("mismatched", "shared", "this project's own\n")
	f.symlink("mismatched", "shared", global)
	f.mkProject("stale")
	f.capture("stale", "deleted-by-the-project", "the last copy\n")
	f.capture("Web/gone", "orphaned", "also the last copy\n")
	f.project("override", "shared", "customized\n")

	before := snapshot(t, f.base)
	rep := f.diagnose()
	after := snapshot(t, f.base)

	if len(rep.Findings) == 0 {
		t.Fatal("the fixture produced no findings, so this proves nothing about the paths that report them")
	}
	assertSameTree(t, before, after)
}

// snapshot records every path under root and what it is, without following a
// single symlink: content for a regular file, the literal target for a link.
//
// Lstat and Readlink rather than Stat and ReadFile, so a link silently replaced
// by a copy of its target — the one corruption that leaves the bytes equal —
// still registers as a change.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "link -> " + target
		case fi.IsDir():
			out[rel] = "dir"
		default:
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = "file " + fi.Mode().String() + " " + string(body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return out
}

func assertSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, was := range before {
		now, still := after[path]
		if !still {
			t.Errorf("%s was REMOVED by a read-only command", path)
			continue
		}
		if now != was {
			t.Errorf("%s was MODIFIED by a read-only command:\n  before: %s\n  after:  %s", path, was, now)
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			t.Errorf("%s was CREATED by a read-only command", path)
		}
	}
}

// filepath.Walk does not follow the symlinks in the fixture, so the snapshot
// above never leaves the tree. Asserted here rather than assumed, because a
// snapshot that wandered out through a link would be comparing something other
// than the tree it names, and would pass regardless of what happened to it.
//
// A *directory* symlink is the case that matters and the one the concern is
// actually about: a file link contributes one entry either way, while a
// followed directory link would splice a whole foreign subtree into both
// snapshots. Both are planted, since Walk treats them the same way and the test
// is cheap.
func TestSnapshot_DoesNotFollowLinks(t *testing.T) {
	f := newFixture(t)

	outside := filepath.Join(t.TempDir(), "outside.md")
	mustWrite(t, outside, "somewhere else entirely\n")
	f.symlink("proj", "away", outside)

	// A directory living outside the snapshot root, reached from inside it.
	elsewhere := t.TempDir()
	mustWrite(t, filepath.Join(elsewhere, "buried", "deep.md"), "a whole foreign subtree\n")
	if err := os.Symlink(elsewhere, filepath.Join(f.base, "portal")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	shot := snapshot(t, f.base)
	for path, what := range shot {
		if strings.Contains(what, "somewhere else entirely") || strings.Contains(what, "a whole foreign subtree") {
			t.Fatalf("the snapshot followed a link out of the tree at %s", path)
		}
	}
	// The link itself is recorded — as a link, by its target. A snapshot that
	// silently omitted it could not notice one being replaced.
	if got, want := shot["portal"], "link -> "+elsewhere; got != want {
		t.Errorf("portal recorded as %q, want %q", got, want)
	}
	if _, followed := shot[filepath.Join("portal", "buried")]; followed {
		t.Error("the snapshot descended through a directory symlink")
	}
}

// --- resolution edge cases --------------------------------------------------

// The case dirPresent's symlink branch actually exists for: ProjectKey
// deliberately does not resolve <project>/.claude/agents — resolving it would
// give every project pointing at a shared location the same key — so a
// symlinked agents directory is a supported layout. An Lstat-only check would
// call it absent and report every capture in that project stale.
func TestDiagnose_SymlinkedAgentsDirIsStillAnAgentsDir(t *testing.T) {
	f := newFixture(t)

	// One shared agents directory, reached by the project through a link.
	shared := filepath.Join(f.base, "shared-agents")
	const body = "served from a shared directory\n"
	mustWrite(t, filepath.Join(shared, "local-only.md"), body)

	mustMkdir(t, filepath.Join(f.base, "proj", ".claude"))
	if err := os.Symlink(shared, f.agentsDir("proj")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.capture("proj", "local-only", body)

	rep := f.diagnose()

	if n := len(rep.OfKind(KindStaleCapture)); n != 0 {
		t.Errorf("got %d stale-capture findings; the live file is there, behind a symlinked agents directory: %v", n, rep.Findings)
	}
	if n := len(rep.OfKind(KindOrphanedCaptures)); n != 0 {
		t.Errorf("got %d orphaned-captures findings; the project is there: %v", n, rep.Findings)
	}
}

// The weaker half of the same branch: a project directory that is itself a
// link. It mostly does not arise — ProjectKey resolves the project directory,
// so such a project is keyed by its resolved path — but the branch handles it
// and the cost of saying so is one test.
func TestDiagnose_ProjectBehindASymlinkIsNotOrphaned(t *testing.T) {
	f := newFixture(t)

	// The real directory lives outside the keyed path; Web/linked reaches it
	// through a link, exactly as a project on an external volume would.
	real := filepath.Join(f.base, "volume", "actual-project")
	mustMkdir(t, filepath.Join(real, ".claude", "agents"))
	mustMkdir(t, filepath.Join(f.base, "Web"))
	if err := os.Symlink(real, filepath.Join(f.base, "Web", "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	const body = "this project's own agent\n"
	mustWrite(t, filepath.Join(real, ".claude", "agents", "local-only.md"), body)
	mustWrite(t, filepath.Join(f.repoRoot, capturesDir, "Web", "linked", "local-only.md"), body)

	rep := f.diagnose()

	if n := len(rep.OfKind(KindOrphanedCaptures)); n != 0 {
		t.Errorf("got %d orphaned-captures findings; the project is there, just behind a link: %v", n, rep.Findings)
	}
	if n := len(rep.OfKind(KindStaleCapture)); n != 0 {
		t.Errorf("got %d stale-capture findings; the live file is there, just behind a link: %v", n, rep.Findings)
	}
}

// A capture that is not a regular file cannot be judged as one — we do not know
// whose content it is or whether it is the last copy of anything. Recorded as a
// problem rather than dropped, so an incomplete diagnosis never passes for a
// complete one.
func TestWalkCaptures_NonRegularCaptureIsAProblem(t *testing.T) {
	f := newFixture(t)
	f.mkProject("proj")
	elsewhere := filepath.Join(f.base, "elsewhere.md")
	mustWrite(t, elsewhere, "x\n")

	odd := filepath.Join(f.repoRoot, capturesDir, "proj", "linked-capture.md")
	mustMkdir(t, filepath.Dir(odd))
	if err := os.Symlink(elsewhere, odd); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	captures, problems := walkCaptures(filepath.Join(f.repoRoot, capturesDir))
	if len(captures) != 0 {
		t.Errorf("captures = %v, want none: a symlink is not a capture we can judge", captures)
	}
	if len(problems) != 1 || problems[0].Path != odd {
		t.Fatalf("problems = %v, want one naming %s", problems, odd)
	}
}

// A capture subtree that cannot be read is skipped and recorded, never silently
// treated as empty — an unreadable directory and an empty one must not produce
// the same report.
//
// Structural rather than chmod 000 (issue #11): the CI gate runs as root, and
// permission bits do not restrict root's walk, so the chmod form of this test
// skipped in CI and covered nothing there. testfs.UnreadableSubtree fails the
// same way for every uid.
func TestWalkCaptures_UnreadableSubtreeIsAProblem(t *testing.T) {
	f := newFixture(t)
	f.mkProject("readable")
	f.capture("readable", "visible", "x\n")

	locked := filepath.Join(f.repoRoot, capturesDir, "locked")
	mustMkdir(t, locked)
	unreadable, inside := testfs.UnreadableSubtree(t, locked)
	if err := inside.WriteFile("hidden.md", []byte("unreachable\n"), 0o644); err != nil {
		t.Fatalf("planting the unreachable capture: %v", err)
	}

	captures, problems := walkCaptures(filepath.Join(f.repoRoot, capturesDir))
	if len(captures) != 1 || captures[0].name != "visible" {
		t.Errorf("captures = %v, want the one readable capture", captures)
	}
	if len(problems) != 1 || problems[0].Path != unreadable {
		t.Fatalf("problems = %v, want one naming %s", problems, unreadable)
	}
}

// A repo that has captured nothing yet has no project-agents/ directory at all.
// That is an absence, not a fault.
func TestWalkCaptures_MissingCaptureTreeIsNotAProblem(t *testing.T) {
	captures, problems := walkCaptures(filepath.Join(t.TempDir(), "never-created"))
	if len(captures) != 0 || len(problems) != 0 {
		t.Errorf("captures = %v, problems = %v; a repo with no captures has nothing to report", captures, problems)
	}
}

// --- report shape -----------------------------------------------------------

func TestReport_CountsAndPrintableForms(t *testing.T) {
	rep := Report{}
	rep.add(Finding{Kind: KindBrokenLink, Project: "a", Name: "one", Detail: "gone"})
	rep.add(Finding{Kind: KindForeignLink, Project: "a", Name: "two", Detail: "theirs"})
	rep.add(Finding{Kind: KindOrphanedCaptures, Project: "b", Detail: "no project"})

	counts := rep.Counts()
	if counts[KindBrokenLink] != 1 || counts[KindForeignLink] != 1 || counts[KindOrphanedCaptures] != 1 {
		t.Errorf("counts = %v", counts)
	}
	if n := len(rep.NeedsHuman()); n != 2 {
		t.Errorf("NeedsHuman() = %d, want 2: the foreign link is not one", n)
	}

	// A finding with an agent names it; one about a whole directory does not,
	// and must not print an empty slot where the name would be.
	if got, want := rep.Findings[0].String(), "broken-link a/one: gone"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := rep.Findings[2].String(), "orphaned-captures b: no project"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := KindOverrideCannotRun.String(); got != "override-cannot-run" || !KindOverrideCannotRun.NeedsHuman() {
		t.Errorf("KindOverrideCannotRun = %q, needs human %v; want override-cannot-run, true", got, KindOverrideCannotRun.NeedsHuman())
	}
	if got := KindInvalid.String(); got != "invalid" {
		t.Errorf("KindInvalid.String() = %q, want invalid", got)
	}
	if KindInvalid.NeedsHuman() {
		t.Error("the zero kind must never demand a human")
	}
}

// --- the home signal --------------------------------------------------------

// HomeSuspect is the inference the CLI turns into a warning: if every capture
// directory in the repo names a project that is not there, the home used to
// resolve those keys is a likelier explanation than fifteen simultaneous
// deletions.
func TestReport_HomeSuspect(t *testing.T) {
	// Each key becomes one orphaned capture directory; live is how many
	// further capture directories exist whose projects are fine.
	build := func(live int, keys ...string) Report {
		rep := Report{Examined: Examined{CaptureDirs: live, HomeKeyedCaptureDirs: live}}
		for _, key := range keys {
			rep.add(Finding{Kind: KindOrphanedCaptures, Project: key})
			rep.Examined.CaptureDirs++
			if scan.KeyDependsOnHome(key) {
				rep.Examined.HomeKeyedCaptureDirs++
			}
		}
		return rep
	}

	cases := []struct {
		name    string
		rep     Report
		suspect bool
	}{
		{"every home-keyed directory orphaned", build(0, "Web/a", "Web/b", "Web/c"), true},
		{"one of three orphaned", build(2, "Web/a"), false},
		{"no captures at all", build(0), false},
		{"captures, none orphaned", build(3), false},
		{"_home counts: it is resolved from home", build(0, "_home"), true},

		// The case the home-keyed filter exists for. ProjectDir resolves an
		// _outside-home/ key by joining its absolute path onto "/", never
		// reading home, so such a project being gone is zero evidence about
		// the home — and it is the honest-orphan shape a user who set
		// --allow-outside-home is most likely to hit.
		{"only an _outside-home directory orphaned", build(0, "_outside-home/opt/gone"), false},
		{"_outside-home orphan beside a live home-keyed one", build(1, "_outside-home/opt/gone"), false},

		// It must not dilute the signal either: every home-keyed directory
		// is still orphaned, which is the whole of the evidence, and an
		// unrelated outside-home orphan alongside them changes nothing.
		{"_outside-home orphan beside an all-orphaned home-keyed set", build(0, "Web/a", "_outside-home/opt/gone"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.HomeSuspect(); got != tc.suspect {
				t.Errorf("HomeSuspect() = %v, want %v", got, tc.suspect)
			}
			warning := tc.rep.HomeWarning("/somewhere")
			if (warning != "") != tc.suspect {
				t.Errorf("HomeWarning() = %q, want empty = %v", warning, !tc.suspect)
			}
			if !tc.suspect {
				return
			}
			if !strings.Contains(warning, "/somewhere") {
				t.Errorf("the warning does not show the home in use:\n%s", warning)
			}
			// The count must be the home-keyed one, never the total: naming
			// a directory the home never resolved as evidence about the home
			// is exactly the misattribution the filter removes.
			n := tc.rep.Examined.HomeKeyedCaptureDirs
			if want := fmt.Sprintf("(%d of %d;", n, n); !strings.Contains(warning, want) {
				t.Errorf("the warning does not count only home-keyed directories (want %q):\n%s", want, warning)
			}
			// Both channels print this sentence, and the findings sit below
			// it on one and above it on the other.
			for _, wrong := range []string{"finding below", "finding above"} {
				if strings.Contains(warning, wrong) {
					t.Errorf("the warning points a direction that is wrong on one of the two channels (%q):\n%s", wrong, warning)
				}
			}
		})
	}
}

// A home that could not be determined at all still has to produce a readable
// sentence rather than an empty pair of quotes.
func TestReport_HomeWarningWithNoHome(t *testing.T) {
	rep := Report{Examined: Examined{CaptureDirs: 1, HomeKeyedCaptureDirs: 1}}
	rep.add(Finding{Kind: KindOrphanedCaptures, Project: "a"})

	warning := rep.HomeWarning("")
	if !strings.Contains(warning, "empty or unset") {
		t.Errorf("an unknown home is not described:\n%s", warning)
	}
	if strings.Contains(warning, `""`) {
		t.Errorf("an unknown home printed as empty quotes:\n%s", warning)
	}
}
