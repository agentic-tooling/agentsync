package linker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// --- fixtures ---------------------------------------------------------------

// fixture is a throwaway world: an agent repo, and projects beside it under a
// base directory that stands in for $HOME.
//
// Every test below drives the real classifier over real directories rather
// than hand-building entries. That is not ceremony: the states this package
// acts on differ from each other only in what os.Lstat and a content hash say
// about a path, so an entry asserting a state the filesystem does not actually
// produce would test the wrong thing exactly where it matters.
type fixture struct {
	t        *testing.T
	base     string
	repoRoot string
	repo     *scan.Repo

	// state is the document the plan reads and the apply writes, one object
	// per fixture as the real command uses it. It starts empty; a test that
	// cares declines something in it before planning.
	state *state.File
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

	// The base is resolved so paths built here compare equal to the ones the
	// scan reports; on macOS a temp dir is reached through /var, a symlink.
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("resolving %s: %v", base, err)
	}
	repo.Home = resolved

	return &fixture{t: t, base: resolved, repoRoot: repo.Root, repo: repo, state: state.New()}
}

// agentsDir is a project's .claude/agents directory.
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

// backup writes a file where an import would put it: this project's captured
// copy, which is the counterpart link must prefer.
func (f *fixture) backup(project, name, content string) string {
	f.t.Helper()
	p := f.repo.BackupPath(project, name)
	mustWrite(f.t, p, content)
	return p
}

// symlink puts a link into a project's agents directory.
func (f *fixture) symlink(project, name, target string) string {
	f.t.Helper()
	p := filepath.Join(f.agentsDir(project), name+".md")
	mustMkdir(f.t, filepath.Dir(p))
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
	return p
}

func (f *fixture) result() scan.Result {
	f.t.Helper()
	res, err := f.repo.Scan(f.base)
	if err != nil {
		f.t.Fatalf("Scan: %v", err)
	}
	if len(res.Problems) > 0 {
		f.t.Fatalf("fixture scan reported problems: %v", res.Problems)
	}
	return res
}

func (f *fixture) plan(opts Options) Plan {
	f.t.Helper()
	p, err := NewPlan(f.repo, f.result(), f.state, opts)
	if err != nil {
		f.t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// gitInit makes a project a git repository, isolated from the developer's own
// git configuration so a global commit.gpgsign or template setting cannot make
// the suite behave differently on one machine.
func (f *fixture) gitInit(project string) string {
	f.t.Helper()
	dir := filepath.Join(f.base, filepath.FromSlash(project))
	mustMkdir(f.t, dir)
	gitRun(f.t, dir, "init", "-q", "-b", "main")
	return dir
}

// gitCommitAll commits everything currently in a project.
func (f *fixture) gitCommitAll(project, message string) {
	f.t.Helper()
	dir := filepath.Join(f.base, filepath.FromSlash(project))
	gitRun(f.t, dir, "add", "-A")
	gitRun(f.t, dir, "commit", "-qm", message)
}

// gitStatus returns the porcelain status of a project, which is what "does
// this project's git see the links" actually means.
func (f *fixture) gitStatus(project string) string {
	f.t.Helper()
	dir := filepath.Join(f.base, filepath.FromSlash(project))
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	cmd.Env = isolatedGitEnv(f.t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git status in %s: %v\n%s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isolatedGitEnv(t *testing.T) []string {
	t.Helper()
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig-system"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = isolatedGitEnv(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
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

// actions maps "project/name" to the action planned for it.
func actions(p Plan) map[string]Action {
	got := make(map[string]Action, len(p.Items))
	for _, i := range p.Items {
		got[i.Project+"/"+i.Name] = i.Action
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

func applyOK(t *testing.T, f *fixture, p Plan) []Item {
	t.Helper()
	done, errs := p.Apply(f.repo, f.state)
	for _, err := range errs {
		t.Errorf("Apply: %v", err)
	}
	return done.Links
}

// assertLink checks that path is a symlink leading to want.
func assertLink(t *testing.T, path, want string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", path, fi.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if target != want {
		t.Errorf("%s -> %s, want -> %s", path, target, want)
	}
	// A link is only useful if it resolves, and a resolving link that leads
	// somewhere other than the counterpart is the failure this whole command
	// exists to avoid.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolving %s: %v", want, err)
	}
	if resolved != wantResolved {
		t.Errorf("%s resolves to %s, want %s", path, resolved, wantResolved)
	}
}

// --- the state table --------------------------------------------------------

// TestNewPlan_StateTable is the core of the command: every state maps onto one
// action, and only two of them write.
//
// Each project is separate on purpose. The invariant is per-project, so
// putting an unadopted file beside an adoptable one would block both and the
// test would prove nothing about the mapping.
func TestNewPlan_StateTable(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")

	// State 2: a healthy link at this project's counterpart.
	f.symlink("healthy", "shared", filepath.Join(f.repoRoot, "agents", "shared.md"))
	// State 3: a link into the repo whose target was never created.
	f.symlink("broken", "gone", filepath.Join(f.repoRoot, "agents", "gone.md"))
	// State 4: a link to a file that has nothing to do with this repo.
	outside := filepath.Join(f.base, "elsewhere", "shared.md")
	mustWrite(t, outside, "someone else's agent\n")
	f.symlink("foreign", "shared", outside)
	// State 5: a real file byte-identical to the global agent.
	f.project("adoptable", "shared", "canonical shared agent\n")
	// State 6, not at risk: differs from both the global agent and its own
	// stale capture, but is committed in its own project's repository. That
	// is the only way a state-6 file reaches an item at all — an uncaptured
	// one blocks its whole project instead.
	f.gitInit("override")
	f.project("override", "shared", "customized\n")
	f.backup("override", "shared", "an older customization\n")
	f.gitCommitAll("override", "the project's own agent")
	// State 7: no counterpart at all. This one IS at risk, so its project is
	// refused outright rather than reported item by item.
	f.project("unadopted", "solo", "exists only here\n")

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{
		"healthy/shared":   ActionLinked,
		"broken/gone":      ActionBroken,
		"foreign/shared":   ActionForeign,
		"adoptable/shared": ActionAdopt,
		"override/shared":  ActionOverride,
	})

	if len(plan.Blocked) != 1 || plan.Blocked[0].Project != "unadopted" {
		t.Fatalf("blocked = %+v, want the unadopted project alone", plan.Blocked)
	}
	if writes := plan.Writes(); len(writes) != 1 || writes[0].Action != ActionAdopt {
		t.Errorf("writes = %v, want the adoptable file alone", writes)
	}
	if unresolved := plan.Unresolved(); len(unresolved) != 1 || unresolved[0].Action != ActionBroken {
		t.Errorf("unresolved = %v, want the broken link alone", unresolved)
	}
}

// --- adoption ---------------------------------------------------------------

func TestApply_AdoptableBecomesALinkToItsCounterpart(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptable", "shared", body)

	plan := f.plan(Options{})
	if got := applyOK(t, f, plan); len(got) != 1 {
		t.Fatalf("wrote %d items, want 1", len(got))
	}

	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
	if got := mustRead(t, path); got != body {
		t.Errorf("reading through the link gives %q, want %q", got, body)
	}
}

// TestApply_CapturedOverrideLinksToItsOwnCapture is the case the tool exists
// for. Web/content-pipeline/web-researcher differs from the global
// web-researcher by 80 lines; once captured, its counterpart is its own copy
// under project-agents/, and linking it to the same-named global agent instead
// would replace a unique customization with a different file.
func TestApply_CapturedOverrideLinksToItsOwnCapture(t *testing.T) {
	f := newFixture(t)
	const custom = "the project's own web-researcher, 80 lines different\n"
	global := f.global("web-researcher", "the global web-researcher\n")
	capture := f.backup("Web/content-pipeline", "web-researcher", custom)
	path := f.project("Web/content-pipeline", "web-researcher", custom)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{
		"Web/content-pipeline/web-researcher": ActionAdopt,
	})
	if got := plan.Items[0].Target; got != capture {
		t.Fatalf("target = %s, want the capture %s", got, capture)
	}

	if got := applyOK(t, f, plan); len(got) != 1 {
		t.Fatalf("wrote %d items, want 1", len(got))
	}
	assertLink(t, path, capture)
	if got := mustRead(t, path); got != custom {
		t.Errorf("the project now reads %q, want its own %q", got, custom)
	}

	// Stated the other way round, because this is the failure mode and a
	// passing target assertion above would not catch a second link appearing.
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target == global {
		t.Fatal("the override was linked to the global agent of the same name")
	}
}

// TestApply_AdoptionIsIdempotent: adopting turns the file into a healthy link,
// which the next scan classifies as state 2 and the next plan leaves alone.
func TestApply_AdoptionIsIdempotent(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptable", "shared", body)

	if got := applyOK(t, f, f.plan(Options{})); len(got) != 1 {
		t.Fatalf("first run wrote %d items, want 1", len(got))
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}

	second := f.plan(Options{})
	assertActions(t, second, map[string]Action{"adoptable/shared": ActionLinked})
	if got := applyOK(t, f, second); len(got) != 0 {
		t.Errorf("second run wrote %v, want nothing", got)
	}

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after second run: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the link was rewritten on an idempotent run (mtime %v -> %v)",
			before.ModTime(), after.ModTime())
	}
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// TestApply_AdoptionRefusesAFileThatChangedSinceThePlan is the recheck. A file
// edited between planning and writing is no longer adoptable, and replacing it
// with a link would discard the edit — the exact loss this command is built to
// make impossible.
func TestApply_AdoptionRefusesAFileThatChangedSinceThePlan(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptable", "shared", body)

	plan := f.plan(Options{})
	mustWrite(t, path, "edited while the scan was running\n")

	done, errs := plan.Apply(f.repo, f.state)
	if len(done.Links) != 0 {
		t.Errorf("wrote %v; the file was no longer adoptable", done.Links)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one refusal", errs)
	}
	if !strings.Contains(errs[0].Error(), "changed since the plan") {
		t.Errorf("refusal does not say why: %v", errs[0])
	}
	if got := mustRead(t, path); got != "edited while the scan was running\n" {
		t.Errorf("the edit was lost: %q", got)
	}
}

// --- links that are left alone ----------------------------------------------

func TestApply_BrokenLinkIsReportedAndLeftInPlace(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.repoRoot, "agents", "gone.md")
	path := f.symlink("broken", "gone", target)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"broken/gone": ActionBroken})
	if len(plan.Writes()) != 0 {
		t.Errorf("a broken link produced a write: %v", plan.Writes())
	}
	applyOK(t, f, plan)

	// Still there, still pointing where it did. link never unlinks.
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("the broken link is gone: %v", err)
	}
	if got != target {
		t.Errorf("the broken link now points at %s, want %s", got, target)
	}
}

func TestApply_ForeignLinkIsUntouched(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.base, "elsewhere", "shared.md")
	mustWrite(t, outside, "someone else's agent\n")
	// A global agent of the same name exists, so a name-driven tool would
	// happily point this at the repo.
	f.global("shared", "canonical shared agent\n")
	path := f.symlink("foreign", "shared", outside)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"foreign/shared": ActionForeign})
	if len(plan.Unresolved()) != 0 {
		t.Errorf("a foreign link counted as unresolved: %v", plan.Unresolved())
	}
	applyOK(t, f, plan)

	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != outside {
		t.Errorf("the foreign link now points at %s, want %s", got, outside)
	}
	if body := mustRead(t, path); body != "someone else's agent\n" {
		t.Errorf("the foreign link's content changed: %q", body)
	}
}

// TestNewPlan_HealthyLinkAtTheWrongRepoFileIsAMismatch covers the residual form
// of the override problem: nothing is destroyed, but the project is being
// served the global agent while its own captured copy sits unused.
func TestNewPlan_HealthyLinkAtTheWrongRepoFileIsAMismatch(t *testing.T) {
	f := newFixture(t)
	global := f.global("web-researcher", "the global web-researcher\n")
	f.backup("proj", "web-researcher", "the project's own\n")
	path := f.symlink("proj", "web-researcher", global)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"proj/web-researcher": ActionMismatch})
	if len(plan.Writes()) != 0 {
		t.Errorf("a mismatch produced a write: %v", plan.Writes())
	}
	if len(plan.Unresolved()) != 1 {
		t.Errorf("a mismatch must need a human: %v", plan.Unresolved())
	}
	applyOK(t, f, plan)

	// Reported, not repointed.
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != global {
		t.Errorf("the link was repointed to %s; link does not repoint", got)
	}
}

// --- the invariant ----------------------------------------------------------

// TestNewPlan_UncapturedFileBlocksItsProjectAndOnlyItsProject is the
// import-before-link invariant. It is enforced per project so one unprotected
// file cannot stop every other project on the machine from being provisioned.
func TestNewPlan_UncapturedFileBlocksItsProjectAndOnlyItsProject(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)

	// An uncaptured override sitting next to a perfectly adoptable file.
	blocked := f.project("risky", "shared", "customized and captured nowhere\n")
	alsoBlocked := f.project("risky", "other", body)
	f.global("other", body)
	// A sibling project with nothing wrong with it.
	sibling := f.project("safe", "shared", body)

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"safe/shared": ActionAdopt})

	if len(plan.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want exactly the risky project", plan.Blocked)
	}
	if plan.Blocked[0].Project != "risky" {
		t.Errorf("blocked project = %q, want %q", plan.Blocked[0].Project, "risky")
	}
	if len(plan.Blocked[0].AtRisk) != 1 || plan.Blocked[0].AtRisk[0].Name != "shared" {
		t.Errorf("at-risk = %+v, want the uncaptured override alone", plan.Blocked[0].AtRisk)
	}

	applyOK(t, f, plan)

	// The blocked project is untouched in full — including the file that was
	// itself adoptable. A refusal is about the directory, not the file.
	assertRegular(t, blocked, "an uncaptured override must never be replaced")
	assertRegular(t, alsoBlocked, "a blocked project's adoptable file must be left alone too")
	assertLink(t, sibling, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// TestNewPlan_CapturedFileDoesNotBlock: once import has captured it, the same
// file is byte-identical to its own counterpart and the project links normally.
func TestNewPlan_CapturedFileDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	const custom = "customized\n"
	f.global("shared", "canonical shared agent\n")
	capture := f.backup("proj", "shared", custom)
	path := f.project("proj", "shared", custom)

	plan := f.plan(Options{})
	if len(plan.Blocked) != 0 {
		t.Fatalf("blocked = %+v, want none: the file is captured", plan.Blocked)
	}
	assertActions(t, plan, map[string]Action{"proj/shared": ActionAdopt})
	applyOK(t, f, plan)
	assertLink(t, path, capture)
}

func assertRegular(t *testing.T, path, why string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("%s is no longer a regular file (%v); %s", path, fi.Mode(), why)
	}
}

// --- the project's own git --------------------------------------------------

// excludeFile is where a project's local ignore rules live. .git/info/exclude,
// never .gitignore: .gitignore is tracked and shared, so a rule written there
// is committed on behalf of everyone who clones the project.
func (f *fixture) excludeFile(project string) string {
	return filepath.Join(f.base, filepath.FromSlash(project), ".git", "info", "exclude")
}

// trackedProject is a project whose own git has an agent committed under
// .claude/agents — a project with four committed agents — plus
// an adoptable file link would want to write over.
func trackedProject(t *testing.T, f *fixture, project string) string {
	t.Helper()
	const body = "canonical shared agent\n"
	f.global("shared", body)
	f.gitInit(project)
	path := f.project(project, "shared", body)
	f.gitCommitAll(project, "agents")
	return path
}

// seedExclude gives a project a known exclude file, so a test comparing it
// before and after does not depend on git's init template.
func seedExclude(t *testing.T, f *fixture, project string) string {
	t.Helper()
	const rules = "*.local\n"
	mustWrite(t, f.excludeFile(project), rules)
	return rules
}

// excludePlan plans an --exclude-local adoption in a tracked project and returns
// the plan, the project file and the exclude file.
func excludePlan(t *testing.T) (*fixture, Plan, string, string) {
	t.Helper()
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")
	seedExclude(t, f, "tracked")
	plan := f.plan(Options{Projects: []string{"tracked"}, ExcludeLocal: true})
	if len(plan.Excludes) != 1 {
		t.Fatalf("excludes = %v, want one", plan.Excludes)
	}
	return f, plan, path, f.excludeFile("tracked")
}

// TestNewPlan_TrackedAgentsDirectoryIsRefused: a symlink into this repo names
// an absolute path that resolves on one machine, so committing one hands every
// other clone of that project a broken link. The default is to refuse.
func TestNewPlan_TrackedAgentsDirectoryIsRefused(t *testing.T) {
	f := newFixture(t)
	path := trackedProject(t, f, "tracked")

	plan := f.plan(Options{})
	if len(plan.Items) != 0 {
		t.Errorf("items = %v, want none: the project is refused entirely", plan.Items)
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Project != "tracked" {
		t.Fatalf("blocked = %+v, want the tracked project", plan.Blocked)
	}
	if !strings.Contains(plan.Blocked[0].Reason, ExcludeLocalFlag) {
		t.Errorf("the refusal does not say how to proceed: %s", plan.Blocked[0].Reason)
	}
	if len(plan.Excludes) != 0 {
		t.Errorf("excludes = %v, want none without --%s", plan.Excludes, ExcludeLocalFlag)
	}

	applyOK(t, f, plan)
	assertRegular(t, path, "a tracked agents directory must not be linked into")
	// git init already ships an info/exclude, so the question is whether we
	// added to it, not whether it exists.
	if body, err := os.ReadFile(f.excludeFile("tracked")); err == nil && strings.Contains(string(body), ".claude/agents") {
		t.Error("the exclude file was edited without --" + ExcludeLocalFlag)
	}
}

// TestApply_ExcludeLocalMakesTheLinksInvisibleToTheProject is the opt-in. The
// exclusion is written first, then the links, and the project's git status is
// clean afterwards — which is the whole point of the flag.
//
// The linked agent is one the project has never committed. Excluding a
// directory does not untrack what is already in it, so replacing a *committed*
// file with a symlink still shows as a change; that case is covered separately
// below rather than papered over here.
func TestApply_ExcludeLocalMakesTheLinksInvisibleToTheProject(t *testing.T) {
	f := newFixture(t)
	f.gitInit("tracked")
	// Committed in its own project and absent from this repo — state 7, which
	// link reports and never touches. Nothing this run writes goes near it.
	f.project("tracked", "kept", "an agent this project committed\n")
	f.gitCommitAll("tracked", "agents")
	target := f.global("fresh", "a new agent\n")

	if got := f.gitStatus("tracked"); got != "" {
		t.Fatalf("the fixture did not start clean: %q", got)
	}

	plan := f.plan(Options{
		Projects:     []string{"tracked"},
		Agents:       []string{"fresh"},
		ExcludeLocal: true,
	})
	if len(plan.Blocked) != 0 {
		t.Fatalf("blocked = %+v, want none with --%s", plan.Blocked, ExcludeLocalFlag)
	}
	if len(plan.Excludes) != 1 {
		t.Fatalf("excludes = %v, want one", plan.Excludes)
	}
	if got := plan.Excludes[0].Pattern; got != "/.claude/agents/" {
		t.Errorf("pattern = %q, want %q anchored to the repo root", got, "/.claude/agents/")
	}
	if got := plan.Excludes[0].File; got != f.excludeFile("tracked") {
		t.Errorf("exclude file = %q, want %q — never .gitignore", got, f.excludeFile("tracked"))
	}

	done, errs := plan.Apply(f.repo, f.state)
	for _, err := range errs {
		t.Errorf("Apply: %v", err)
	}
	if len(done.Excludes) != 1 {
		t.Errorf("wrote %d exclusions, want 1", len(done.Excludes))
	}
	if len(done.Links) != 1 {
		t.Errorf("wrote %d links, want 1 (the requested agent alone)", len(done.Links))
	}

	if body := mustRead(t, f.excludeFile("tracked")); !strings.Contains(body, "/.claude/agents/") {
		t.Errorf(".git/info/exclude does not carry the pattern:\n%s", body)
	}
	assertLink(t, filepath.Join(f.agentsDir("tracked"), "fresh.md"), target)
	if got := f.gitStatus("tracked"); got != "" {
		t.Errorf("the project's git still sees the links:\n%s", got)
	}
}

// TestApply_ExcludeLocalIsIdempotent: the entry is added once. A command that
// grew its exclude file on every run would be a nuisance in somebody else's
// repository.
func TestApply_ExcludeLocalIsIdempotent(t *testing.T) {
	f := newFixture(t)
	trackedProject(t, f, "tracked")
	opts := Options{Projects: []string{"tracked"}, ExcludeLocal: true}

	first := f.plan(opts)
	if _, errs := first.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("first apply: %v", errs)
	}
	before := mustRead(t, f.excludeFile("tracked"))

	second := f.plan(opts)
	if len(second.Excludes) != 0 {
		t.Errorf("excludes = %v on a second run, want none", second.Excludes)
	}
	if _, errs := second.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("second apply: %v", errs)
	}
	if after := mustRead(t, f.excludeFile("tracked")); after != before {
		t.Errorf("the exclude file grew on a second run:\n%s", after)
	}
}

// TestApply_ExcludeLocalPreservesExistingRules: info/exclude belongs to the
// project, and whatever is already in it must survive.
func TestApply_ExcludeLocalPreservesExistingRules(t *testing.T) {
	f := newFixture(t)
	trackedProject(t, f, "tracked")
	// Deliberately without a trailing newline: appending to a file that does
	// not end in one is how two patterns get glued into a third that nobody
	// wrote.
	mustWrite(t, f.excludeFile("tracked"), "*.local\nbuild/")

	plan := f.plan(Options{Projects: []string{"tracked"}, ExcludeLocal: true})
	if _, errs := plan.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("Apply: %v", errs)
	}

	body := mustRead(t, f.excludeFile("tracked"))
	for _, want := range []string{"*.local", "build/", "/.claude/agents/"} {
		if !strings.Contains(body, want) {
			t.Errorf("exclude file lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "build//.claude") {
		t.Errorf("the appended pattern was glued to the last line:\n%s", body)
	}
}

// TestNewPlan_ExcludingDoesNotUntrackWhatIsAlreadyCommitted records a limit of
// the flag honestly rather than implying it does more than it does. git ignore
// rules do not apply to tracked paths, so adopting a file the project has
// committed still shows up as a change there — one the user resolves in their
// own repo.
func TestNewPlan_ExcludingDoesNotUntrackWhatIsAlreadyCommitted(t *testing.T) {
	f := newFixture(t)
	trackedProject(t, f, "tracked")

	plan := f.plan(Options{Projects: []string{"tracked"}, ExcludeLocal: true})
	if _, errs := plan.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("Apply: %v", errs)
	}

	if got := f.gitStatus("tracked"); got == "" {
		t.Error("expected the committed file's replacement to still show in git status; " +
			"if this now passes, excluding has started untracking and the report must stop saying otherwise")
	}
}

// TestNewPlan_IgnoredAgentsDirectoryLinksAndIsReportedLocalOnly: the benign
// case. Most repos ignore .claude/, so the links are already invisible.
func TestNewPlan_IgnoredAgentsDirectoryLinksAndIsReportedLocalOnly(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	dir := f.gitInit("ignored")
	mustWrite(t, filepath.Join(dir, ".gitignore"), ".claude/\n")
	f.gitCommitAll("ignored", "ignore .claude")
	path := f.project("ignored", "shared", body)

	plan := f.plan(Options{})
	if len(plan.Blocked) != 0 {
		t.Fatalf("blocked = %+v, want none: the directory is ignored", plan.Blocked)
	}
	if len(plan.Excludes) != 0 {
		t.Errorf("excludes = %v, want none: it is already ignored", plan.Excludes)
	}
	if len(plan.Git) != 1 || plan.Git[0].Tracking != "ignored" {
		t.Fatalf("git = %+v, want the directory reported as ignored", plan.Git)
	}
	if !strings.Contains(plan.Git[0].Note, "local") {
		t.Errorf("the note does not say the links are local-only: %q", plan.Git[0].Note)
	}

	applyOK(t, f, plan)
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
	if got := f.gitStatus("ignored"); got != "" {
		t.Errorf("the project's git sees the link:\n%s", got)
	}
}

// TestNewPlan_ProjectOutsideGitLinksFreely: nothing can be committed, so there
// is nothing to reconcile. setup-python-macos is the live example.
func TestNewPlan_ProjectOutsideGitLinksFreely(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("nogit", "shared", body)

	plan := f.plan(Options{})
	if len(plan.Blocked) != 0 {
		t.Fatalf("blocked = %+v, want none", plan.Blocked)
	}
	if len(plan.Git) != 1 || plan.Git[0].Tracking != "no-repo" {
		t.Fatalf("git = %+v, want the project reported as having no repo", plan.Git)
	}
	applyOK(t, f, plan)
	assertLink(t, path, filepath.Join(f.repoRoot, "agents", "shared.md"))
}

// TestNewPlan_GitUnavailableIsRefusedNotAssumedClean is the direction the
// three-way answer exists for. If git cannot be asked, the one condition that
// disables the check is also the one that would let the damaging write through.
func TestNewPlan_GitUnavailableIsRefusedNotAssumedClean(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("proj", "shared", body)

	// Planning happens with no git on PATH at all, so every git question
	// comes back "could not ask" rather than "no".
	t.Setenv("PATH", "")

	plan, err := NewPlan(f.repo, f.result(), f.state, Options{})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if len(plan.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want the project refused", plan.Blocked)
	}
	if len(plan.Writes()) != 0 {
		t.Errorf("planned writes with no answer from git: %v", plan.Writes())
	}

	if _, errs := plan.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("Apply: %v", errs)
	}
	assertRegular(t, path, "an unanswerable git check must not be treated as a pass")
}

// TestApply_TrackedOverrideSurvivesBesideAnAdoptedSibling is the claim
// project-agents/README.md now makes, kept honest by asserting it.
//
// The README used to record a dilemma: gate on Entry.AtRisk() and "link will
// symlink over a tracked override". That premise is false for this design —
// states 6 and 7 have no write in the state table, so no choice of gate can
// reach an override. What the softer gate actually buys is this: the sibling
// gets linked instead of the whole project being refused.
func TestApply_TrackedOverrideSurvivesBesideAnAdoptedSibling(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	const custom = "customized, and committed only in this project\n"
	f.global("shared", body)
	f.global("custom", "the global version\n")

	// Both files live in one project, which is the point: the override does
	// not have to be isolated for the sibling to be safe to link.
	f.gitInit("proj")
	override := f.project("proj", "custom", custom)
	sibling := f.project("proj", "shared", body)
	f.gitCommitAll("proj", "agents")
	// --exclude-local is required, and not incidentally: a file tracked in its
	// own repo is by definition in a directory that repo tracks, so the second
	// gate always fires here too. "Tracked override" and "tracked agents
	// directory" are the same fact seen by two checks, and the git gate is the
	// one that speaks first.
	plan := f.plan(Options{ExcludeLocal: true})
	if len(plan.Blocked) != 0 {
		t.Fatalf("blocked = %+v, want none: the override is committed in its own repo", plan.Blocked)
	}
	assertActions(t, plan, map[string]Action{
		"proj/custom": ActionOverride,
		"proj/shared": ActionAdopt,
	})

	if got := applyOK(t, f, plan); len(got) != 1 {
		t.Fatalf("wrote %d links, want the sibling alone", len(got))
	}
	assertLink(t, sibling, filepath.Join(f.repoRoot, "agents", "shared.md"))
	assertRegular(t, override, "a tracked override must never be replaced")
	if got := mustRead(t, override); got != custom {
		t.Errorf("the override reads %q, want it byte-intact as %q", got, custom)
	}
}

// TestNewPlan_FailedGitProbeRefusesTheProject is the end-to-end form of the
// gate being silently off.
//
// git runs and exits non-zero — the shape safe.directory produces on a
// dubiously-owned checkout under sudo, cron, or a container — while a
// repository plainly exists. Before the fix this reached the most permissive
// verdict in the switch and the file was linked with exit 0 and no opt-in.
func TestNewPlan_FailedGitProbeRefusesTheProject(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("proj", "shared", body)
	// A gitfile pointing at a gitdir that is not there: every git probe in
	// this project exits non-zero, and .git is still right there to be found.
	mustWrite(t, filepath.Join(f.base, "proj", ".git"), "gitdir: /nonexistent-agentsync-test\n")

	plan := f.plan(Options{})
	if len(plan.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want the project refused", plan.Blocked)
	}
	if len(plan.Writes()) != 0 {
		t.Errorf("planned writes with no answer from git: %v", plan.Writes())
	}
	applyOK(t, f, plan)
	assertRegular(t, path, "a project whose git could not be asked must not be linked into")
}

// --- agent names ------------------------------------------------------------

// TestOptions_AgentNameMustBeBare closes an escape from the project directory.
//
// The name is joined onto the project's agents directory, so one carrying a
// path writes outside it — and MkdirAll builds the way there. What makes that
// a safety bug rather than a tidiness one is that both gates, the
// import-before-link invariant and the project's git tracking, were computed
// for the agents directory. A write landing anywhere else passed checks that
// do not describe it.
func TestOptions_AgentNameMustBeBare(t *testing.T) {
	for _, name := range []string{
		"../../../agents/shared",
		"../shared",
		"sub/shared",
		"/etc/shared",
		".",
		"..",
		"",
	} {
		err := Options{Projects: []string{"proj"}, Agents: []string{name}}.Validate()
		if err == nil {
			t.Errorf("--agent %q was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), AgentFlag) {
			t.Errorf("--agent %q: the message does not name the flag: %v", name, err)
		}
	}

	// Ordinary agent names, including the ones with dots and dashes this repo
	// actually uses, must keep working.
	for _, name := range []string{"shared", "senior-golang-dev", "web-researcher", "a.b"} {
		if err := (Options{Projects: []string{"proj"}, Agents: []string{name}}).Validate(); err != nil {
			t.Errorf("--agent %q was rejected: %v", name, err)
		}
	}
}

// TestNewPlan_EscapingAgentNameWritesNothing: the rejection has to happen
// before anything is planned, not be caught later by a check downstream.
func TestNewPlan_EscapingAgentNameWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	mustMkdir(t, filepath.Join(f.base, "proj"))

	_, err := NewPlan(f.repo, f.result(), f.state, Options{
		Projects: []string{"proj"},
		Agents:   []string{"../../../agents/shared"},
	})
	if err == nil {
		t.Fatal("NewPlan accepted an agent name that escapes the project")
	}
	// Nothing outside the project's own agents directory may have appeared.
	if _, err := os.Lstat(filepath.Join(f.base, "agents")); err == nil {
		t.Error("a directory was created outside the project")
	}
}

// --- selection --------------------------------------------------------------

func TestNewPlan_ProjectSelection(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	f.project("one", "shared", body)
	f.project("two", "shared", body)

	plan := f.plan(Options{Projects: []string{"two"}})
	assertActions(t, plan, map[string]Action{"two/shared": ActionAdopt})
}

func TestNewPlan_UnknownProjectKeyIsReportedNotGuessed(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")

	plan := f.plan(Options{Projects: []string{"nope"}})
	if len(plan.Unknown) != 1 || plan.Unknown[0].Project != "nope" {
		t.Fatalf("unknown = %+v, want the bad key reported", plan.Unknown)
	}
	if len(plan.Items) != 0 {
		t.Errorf("items = %v, want none", plan.Items)
	}
	// Nothing was created for it: a typo must not provision a tree.
	if _, err := os.Lstat(filepath.Join(f.base, "nope")); err == nil {
		t.Error("a directory was created for an unknown project key")
	}
}

// --- requested agents (state 1) ---------------------------------------------

// TestApply_RequestedAgentIsCreated covers the one state nothing on disk can
// produce, including the case that makes it worth supporting: a project whose
// .claude/agents directory does not exist yet, which the scan cannot find and
// only the project key can address.
func TestApply_RequestedAgentIsCreated(t *testing.T) {
	f := newFixture(t)
	target := f.global("shared", "canonical shared agent\n")
	mustMkdir(t, filepath.Join(f.base, "fresh"))

	plan := f.plan(Options{Projects: []string{"fresh"}, Agents: []string{"shared"}})
	assertActions(t, plan, map[string]Action{"fresh/shared": ActionCreate})
	if got := applyOK(t, f, plan); len(got) != 1 {
		t.Fatalf("wrote %d items, want 1", len(got))
	}
	assertLink(t, filepath.Join(f.base, "fresh", ".claude", "agents", "shared.md"), target)
}

// TestNewPlan_RequestedAgentPrefersTheProjectsOwnCopy: --agent must go through
// Counterpart like everything else, or naming an agent would hand a project the
// global one over its own captured version.
func TestNewPlan_RequestedAgentPrefersTheProjectsOwnCopy(t *testing.T) {
	f := newFixture(t)
	f.global("web-researcher", "the global web-researcher\n")
	capture := f.backup("proj", "web-researcher", "the project's own\n")
	mustMkdir(t, filepath.Join(f.base, "proj"))

	plan := f.plan(Options{Projects: []string{"proj"}, Agents: []string{"web-researcher"}})
	if len(plan.Items) != 1 {
		t.Fatalf("items = %v, want one", plan.Items)
	}
	if got := plan.Items[0].Target; got != capture {
		t.Errorf("target = %s, want the capture %s", got, capture)
	}
}

// TestNewPlan_RequestedAgentAlreadyPresentKeepsItsOwnState: asking for an agent
// that is already there must not invent a second item claiming the path is
// empty, and must not override the judgement its actual state earned.
func TestNewPlan_RequestedAgentAlreadyPresentKeepsItsOwnState(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	f.symlink("proj", "shared", filepath.Join(f.repoRoot, "agents", "shared.md"))

	plan := f.plan(Options{Projects: []string{"proj"}, Agents: []string{"shared"}})
	assertActions(t, plan, map[string]Action{"proj/shared": ActionLinked})
}

func TestNewPlan_RequestedAgentTheRepoDoesNotHave(t *testing.T) {
	f := newFixture(t)
	mustMkdir(t, filepath.Join(f.base, "proj"))

	plan := f.plan(Options{Projects: []string{"proj"}, Agents: []string{"nonexistent"}})
	assertActions(t, plan, map[string]Action{"proj/nonexistent": ActionMissing})
	if len(plan.Writes()) != 0 {
		t.Errorf("planned a link to nothing: %v", plan.Writes())
	}
	if len(plan.Unresolved()) != 1 {
		t.Errorf("a missing agent must need a human: %v", plan.Unresolved())
	}
}

// TestOptions_AgentRequiresProject: linking an agent into every project found
// under the scan root is never what anyone means, so the combination is
// refused rather than obeyed.
func TestOptions_AgentRequiresProject(t *testing.T) {
	err := Options{Agents: []string{"shared"}}.Validate()
	if err == nil {
		t.Fatal("--agent without --project was accepted")
	}
	for _, want := range []string{AgentFlag, ProjectFlag} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name --%s: %v", want, err)
		}
	}

	f := newFixture(t)
	if _, err := NewPlan(f.repo, f.result(), f.state, Options{Agents: []string{"shared"}}); err == nil {
		t.Error("NewPlan accepted --agent without --project")
	}

	if err := (Options{Projects: []string{"proj"}, Agents: []string{"shared"}}).Validate(); err != nil {
		t.Errorf("the valid combination was rejected: %v", err)
	}
	if err := (Options{}).Validate(); err != nil {
		t.Errorf("the zero value was rejected: %v", err)
	}
}

// --- writing mechanics ------------------------------------------------------

// TestApply_RefusesToLinkAtAMissingTarget: creating a link to something that is
// not there manufactures exactly the broken link (state 3) this command
// refuses to clean up afterwards.
func TestApply_RefusesToLinkAtAMissingTarget(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	target := f.global("shared", body)
	path := f.project("adoptable", "shared", body)

	plan := f.plan(Options{})
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the target: %v", err)
	}

	done, errs := plan.Apply(f.repo, f.state)
	if len(done.Links) != 0 {
		t.Errorf("wrote %v with no target to point at", done.Links)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one refusal", errs)
	}
	assertRegular(t, path, "the project file must survive a refused link")
}

// TestApply_LeavesNoTemporaryBehind: the temporary lives in the project's own
// agents directory, where a stray file would be picked up as an agent by the
// next scan and by Claude Code itself.
func TestApply_LeavesNoTemporaryBehind(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptable", "shared", body)

	applyOK(t, f, f.plan(Options{}))

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentsync-link-") {
			t.Errorf("temporary left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("the agents directory holds %d entries, want just the link", len(entries))
	}
}

// --- the state document -----------------------------------------------------

// TestApply_RecordsEveryLinkItCreates. A link with no record is unowned: no
// later pass can prove this tool made it, so nothing will ever remove it.
func TestApply_RecordsEveryLinkItCreates(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	target := f.global("shared", body)
	mustMkdir(t, filepath.Join(f.base, "fresh"))
	adopted := f.project("adoptee", "shared", body)

	plan := f.plan(Options{Projects: []string{"fresh", "adoptee"}, Agents: []string{"shared"}})
	done, errs := plan.Apply(f.repo, f.state)
	for _, err := range errs {
		t.Fatalf("Apply: %v", err)
	}
	if len(done.Links) != 2 || len(done.Recorded) != 2 {
		t.Fatalf("linked %d and recorded %d, want 2 and 2", len(done.Links), len(done.Recorded))
	}

	// Read back from disk, not from memory: the point of the save is that the
	// next process can see it.
	back, err := state.Load(f.repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantHash, err := scan.HashFile(target)
	if err != nil {
		t.Fatalf("hashing the target: %v", err)
	}

	for _, path := range []string{filepath.Join(f.base, "fresh", ".claude", "agents", "shared.md"), adopted} {
		rec, ok := back.LookupLink(path)
		if !ok {
			t.Fatalf("no record for %s", path)
		}
		// Every link link makes is created, ActionAdopt included: state's
		// "adopted" means a link that was already there.
		if rec.Origin != state.OriginCreated {
			t.Errorf("%s: origin = %v, want created", path, rec.Origin)
		}
		if rec.Hash != wantHash {
			t.Errorf("%s: hash = %q, want the target's %q", path, rec.Hash, wantHash)
		}
		if rec.Target != target {
			t.Errorf("%s: target = %q, want %q", path, rec.Target, target)
		}
		if rec.Name != "shared" {
			t.Errorf("%s: name = %q", path, rec.Name)
		}
	}
}

// TestApply_RecordsNothingForALinkItDidNotWrite: the record follows the write.
// A record for a link that is not there would claim this tool created something
// it did not, which is the inference the document exists to prevent.
func TestApply_RecordsNothingForALinkItDidNotWrite(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptee", "shared", body)

	plan := f.plan(Options{})
	// Edited between plan and apply, so recheck refuses the adoption.
	mustWrite(t, path, "edited while the scan was running\n")

	done, errs := plan.Apply(f.repo, f.state)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want the one refusal", errs)
	}
	if len(done.Recorded) != 0 {
		t.Errorf("recorded %+v for a link that was never written", done.Recorded)
	}
	if _, ok := f.state.LookupLink(path); ok {
		t.Error("the document holds a record for a link this run refused to make")
	}
}

// TestApply_WritesTheDocumentOnlyWhenSomethingChanged, or every dry-clean run
// would conjure a state file for a repo that has recorded nothing.
func TestApply_WritesTheDocumentOnlyWhenSomethingChanged(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	f.symlink("linked", "shared", filepath.Join(f.repoRoot, "agents", "shared.md"))

	plan := f.plan(Options{})
	if len(plan.Writes()) != 0 {
		t.Fatalf("writes = %v, want none: the link is already correct", plan.Writes())
	}
	if _, errs := plan.Apply(f.repo, f.state); len(errs) != 0 {
		t.Fatalf("Apply: %v", errs)
	}
	if _, err := os.Lstat(state.Path(f.repo)); err == nil {
		t.Error("a run that recorded nothing still wrote a state file")
	}
}

// TestApply_RefusesWithoutADocumentToRecordInto: creating links first and
// discovering there is nowhere to record them is the unowned-link state reached
// on purpose.
func TestApply_RefusesWithoutADocumentToRecordInto(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	path := f.project("adoptee", "shared", body)

	plan := f.plan(Options{})
	done, errs := plan.Apply(f.repo, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one refusal", errs)
	}
	if len(done.Links) != 0 {
		t.Errorf("wrote %v with nowhere to record it", done.Links)
	}
	assertRegular(t, path, "nothing may be written when the record cannot be")
}

func TestNewPlan_RefusesWithoutADocument(t *testing.T) {
	f := newFixture(t)
	if _, err := NewPlan(f.repo, f.result(), nil, Options{}); err == nil {
		t.Error("NewPlan accepted a nil state document; it cannot tell what was declined without one")
	}
}

// --- declined markers -------------------------------------------------------

// TestNewPlan_DeclinedAgentIsReportedAndNotCreated. The skip has to be visible:
// a silent one is how this feature becomes a bug report.
func TestNewPlan_DeclinedAgentIsReportedAndNotCreated(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	mustMkdir(t, filepath.Join(f.base, "fresh"))
	if err := f.state.Decline(state.Key{Project: "fresh", Name: "shared"},
		"link gone, nothing in its place", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	plan := f.plan(Options{Projects: []string{"fresh"}, Agents: []string{"shared"}})
	assertActions(t, plan, map[string]Action{"fresh/shared": ActionDeclined})
	if len(plan.Writes()) != 0 {
		t.Errorf("writes = %v, want none", plan.Writes())
	}
	if len(plan.Unresolved()) != 0 {
		t.Errorf("unresolved = %v: a decision the user already made must not fail the run forever", plan.Unresolved())
	}
	if reason := plan.Items[0].Reason; !strings.Contains(reason, UndeclineFlag) {
		t.Errorf("reason = %q, want it to name the flag that changes the decision", reason)
	}

	applyOK(t, f, plan)
	if _, err := os.Lstat(filepath.Join(f.base, "fresh", ".claude", "agents", "shared.md")); err == nil {
		t.Error("the declined link was recreated")
	}
}

// TestNewPlan_DeclinedIsReportedEvenWhenTheRepoHasNoSuchAgent: the marker is
// checked first, so the user is told about the decision recorded against this
// pair rather than sent looking for a missing file.
func TestNewPlan_DeclinedIsReportedEvenWhenTheRepoHasNoSuchAgent(t *testing.T) {
	f := newFixture(t)
	mustMkdir(t, filepath.Join(f.base, "fresh"))
	if err := f.state.Decline(state.Key{Project: "fresh", Name: "absent"}, "gone", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	plan := f.plan(Options{Projects: []string{"fresh"}, Agents: []string{"absent"}})
	assertActions(t, plan, map[string]Action{"fresh/absent": ActionDeclined})
}

// TestNewPlan_DeclinedDoesNotSuppressAnAdoptableFile. The marker records that
// the link was gone and nothing stood in its place; a real file is a different
// situation, and adopting it destroys nothing.
func TestNewPlan_DeclinedDoesNotSuppressAnAdoptableFile(t *testing.T) {
	f := newFixture(t)
	const body = "canonical shared agent\n"
	f.global("shared", body)
	f.project("proj", "shared", body)
	if err := f.state.Decline(state.Key{Project: "proj", Name: "shared"}, "gone", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	plan := f.plan(Options{})
	assertActions(t, plan, map[string]Action{"proj/shared": ActionAdopt})
}

// TestNewPlan_DeclinedOnlyAppliesToTheProjectAndAgentItNames.
func TestNewPlan_DeclinedOnlyAppliesToTheProjectAndAgentItNames(t *testing.T) {
	f := newFixture(t)
	f.global("shared", "canonical shared agent\n")
	mustMkdir(t, filepath.Join(f.base, "fresh"))
	mustMkdir(t, filepath.Join(f.base, "other"))
	if err := f.state.Decline(state.Key{Project: "other", Name: "shared"}, "gone", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	plan := f.plan(Options{Projects: []string{"fresh", "other"}, Agents: []string{"shared"}})
	assertActions(t, plan, map[string]Action{
		"fresh/shared": ActionCreate,
		"other/shared": ActionDeclined,
	})
}

func TestAction_DeclinedNeitherWritesNorFailsTheRun(t *testing.T) {
	if ActionDeclined.Writes() {
		t.Error("ActionDeclined writes; a declined agent must never be linked")
	}
	if ActionDeclined.Unresolved() {
		t.Error("ActionDeclined is unresolved; link --apply would then exit 1 forever")
	}
	if got := ActionDeclined.String(); got != "declined" {
		t.Errorf("String() = %q, want declined", got)
	}
}

// TestReplaceLink_SwapsWithoutAGap. The exported form of the adopt path's
// atomic swap, which reconcile uses to repoint a link when a project is
// re-keyed. The property it exists for is that nothing observes an absence:
// the old destination is readable until the moment the new one is.
func TestReplaceLink_SwapsWithoutAGap(t *testing.T) {
	f := newFixture(t)
	first := f.global("first", "one\n")
	second := f.global("second", "two\n")

	path := filepath.Join(t.TempDir(), "agent.md")
	if err := os.Symlink(first, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := ReplaceLink(second, path); err != nil {
		t.Fatalf("ReplaceLink: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (%v)", path, fi.Mode())
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != second {
		t.Errorf("points at %s, want %s", got, second)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want just the link", len(entries))
	}
}
