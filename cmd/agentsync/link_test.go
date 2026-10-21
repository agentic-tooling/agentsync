package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// linkFixture builds a fake home holding the agent repo and the projects
// beside it, and points $HOME at it.
//
// Pointing $HOME is not decoration. Every project key is derived from it, and
// the counterpart a link points at is selected by that key — so a fixture that
// left the developer's real $HOME in place would be testing the home guard
// rather than the link.
func linkFixture(t *testing.T) (base, repoRoot string) {
	t.Helper()

	base = t.TempDir()
	repoRoot = filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)
	return base, repoRoot
}

func linkArgs(t *testing.T, repoRoot, root string, extra ...string) []string {
	t.Helper()
	inTree(t, repoRoot, root)
	return append([]string{"link", "--" + allFlag}, extra...)
}

// globalAgent writes an agent into the repo's global pool and returns the path
// a correct link would point at.
func globalAgent(t *testing.T, repoRoot, name, content string) string {
	t.Helper()
	p := filepath.Join(repoRoot, "agents", name+".md")
	mustWriteT(t, p, content)
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolving %s: %v", p, err)
	}
	return resolved
}

func projectAgentPath(base, project, name string) string {
	return filepath.Join(base, filepath.FromSlash(project), ".claude", "agents", name+".md")
}

func assertIsLink(t *testing.T, path, want string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", path, fi.Mode())
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	if resolved != want {
		t.Errorf("%s resolves to %s, want %s", path, resolved, want)
	}
}

func assertIsRegular(t *testing.T, path, why string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("%s is no longer a regular file (%v); %s", path, fi.Mode(), why)
	}
}

// --- dry run ----------------------------------------------------------------

// TestRunLink_DryRunIsTheDefault: a write path defaults to describing itself.
// The report must name what it would link, and the disk must be untouched — the
// project file still a regular file, not a link.
func TestRunLink_DryRunIsTheDefault(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "DRY RUN") {
		t.Errorf("the report does not say it wrote nothing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--apply") {
		t.Errorf("the report does not say how to actually write:\n%s", stdout)
	}
	if !strings.Contains(stdout, "shared") {
		t.Errorf("the report does not name the file it would link:\n%s", stdout)
	}

	assertIsRegular(t, path, "a dry run must change nothing")
}

// --- apply ------------------------------------------------------------------

func TestRunLink_ApplyLinksAndIsIdempotent(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	target := globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	assertIsLink(t, path, target)

	code, stdout, stderr = runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("second run exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to link") && !strings.Contains(stdout, "Nothing was written") {
		t.Errorf("a second run does not report itself as a no-op:\n%s", stdout)
	}
	assertIsLink(t, path, target)
}

// TestRunLink_OverrideLinksToItsOwnCapture is the case the whole tool exists
// for, driven end to end through the CLI: a project's captured copy wins over
// the global agent that merely shares its name.
func TestRunLink_OverrideLinksToItsOwnCapture(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const custom = "the project's own web-researcher\n"
	global := globalAgent(t, repoRoot, "web-researcher", "the global web-researcher\n")

	capture := filepath.Join(repoRoot, "project-agents", "Web", "content-pipeline", "web-researcher.md")
	mustWriteT(t, capture, custom)
	resolvedCapture, err := filepath.EvalSymlinks(capture)
	if err != nil {
		t.Fatalf("resolving %s: %v", capture, err)
	}
	path := projectAgentPath(base, "Web/content-pipeline", "web-researcher")
	mustWriteT(t, path, custom)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	assertIsLink(t, path, resolvedCapture)
	if resolved, _ := filepath.EvalSymlinks(path); resolved == global {
		t.Fatal("the override was linked to the global agent of the same name")
	}
	if got := readFileT(t, path); got != custom {
		t.Errorf("the project now reads %q, want its own %q", got, custom)
	}
}

// --- the invariant ----------------------------------------------------------

// TestRunLink_UncapturedFileBlocksItsProjectOnly: the refusal is per project,
// so one unprotected file cannot stop every other project from being linked —
// but the run still reports it in the exit code.
func TestRunLink_UncapturedFileBlocksItsProjectOnly(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	target := filepath.Join(repoRoot, "agents", "shared.md")
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}

	risky := projectAgentPath(base, "risky", "solo")
	mustWriteT(t, risky, "exists only here\n")
	riskyAdoptable := projectAgentPath(base, "risky", "shared")
	mustWriteT(t, riskyAdoptable, body)
	safe := projectAgentPath(base, "safe", "shared")
	mustWriteT(t, safe, body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitAtRisk {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "blocked") || !strings.Contains(stdout, "import") {
		t.Errorf("the report does not say the project is blocked and what to do:\n%s", stdout)
	}

	assertIsRegular(t, risky, "an uncaptured file must never be replaced")
	assertIsRegular(t, riskyAdoptable, "a blocked project's adoptable file must be left alone too")
	assertIsLink(t, safe, resolvedTarget)
}

// --- links left alone -------------------------------------------------------

func TestRunLink_BrokenLinkIsReportedAndKept(t *testing.T) {
	base, repoRoot := linkFixture(t)
	target := filepath.Join(repoRoot, "agents", "gone.md")
	path := projectAgentPath(base, "proj", "gone")
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitAtRisk {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "broken") {
		t.Errorf("the report does not name the broken link:\n%s", stdout)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("the broken link is gone: %v", err)
	}
	if got != target {
		t.Errorf("the broken link now points at %s, want %s", got, target)
	}
}

// TestRunLink_ForeignLinkIsUntouchedAndDoesNotFailTheRun: somebody else's link
// is a permanent feature of a machine running another tool alongside this one.
// Failing on it forever would make the exit code useless.
func TestRunLink_ForeignLinkIsUntouchedAndDoesNotFailTheRun(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")

	outside := filepath.Join(base, "elsewhere", "shared.md")
	mustWriteT(t, outside, "someone else's agent\n")
	path := projectAgentPath(base, "proj", "shared")
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(outside, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: a foreign link must not fail the run\nstdout:\n%s\nstderr:\n%s",
			code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "foreign") {
		t.Errorf("the report does not mention the foreign link:\n%s", stdout)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != outside {
		t.Errorf("the foreign link now points at %s, want %s", got, outside)
	}
	if body := readFileT(t, path); body != "someone else's agent\n" {
		t.Errorf("the foreign link's content changed: %q", body)
	}
}

// --- the project's own git --------------------------------------------------

// TestRunLink_TrackedAgentsDirectoryIsRefusedUntilExcluded covers the whole
// flag: refused by default, linked once the directory is excluded locally, and
// the exclusion written to .git/info/exclude rather than .gitignore.
func TestRunLink_TrackedAgentsDirectoryIsRefusedUntilExcluded(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	target := globalAgent(t, repoRoot, "shared", body)

	projectDir := filepath.Join(base, "tracked")
	initRepo(t, projectDir)
	path := projectAgentPath(base, "tracked", "shared")
	mustWriteT(t, path, body)
	gitRun(t, projectDir, "add", ".")
	gitRun(t, projectDir, "commit", "-qm", "agents")

	exclude := filepath.Join(projectDir, ".git", "info", "exclude")
	gitignore := filepath.Join(projectDir, ".gitignore")

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitAtRisk {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "exclude-local") {
		t.Errorf("the refusal does not say how to proceed:\n%s", stdout)
	}
	assertIsRegular(t, path, "a tracked agents directory must not be linked into")

	// A dry run with the flag must still write nothing at all — including
	// nothing to the exclude file, which is an edit to somebody's repository.
	before := readFileT(t, exclude)
	code, stdout, stderr = runCLI(t, linkArgs(t, repoRoot, base, "--exclude-local")...)
	if code != exitOK {
		t.Fatalf("dry run exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "DRY RUN") {
		t.Errorf("the dry run does not say it wrote nothing:\n%s", stdout)
	}
	if after := readFileT(t, exclude); after != before {
		t.Errorf("a dry run edited .git/info/exclude:\n%s", after)
	}
	assertIsRegular(t, path, "a dry run must change nothing")
	// The dry run is where the user decides whether to pass --apply, so it is
	// the one mode where the limitation can still change the decision: git
	// ignore rules do not apply to tracked paths, so this project's committed
	// file will still show as a change afterwards.
	if !strings.Contains(stdout, "does not untrack") {
		t.Errorf("the dry run does not carry the exclusion caveat:\n%s", stdout)
	}

	code, stdout, stderr = runCLI(t, linkArgs(t, repoRoot, base, "--exclude-local", "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	assertIsLink(t, path, target)

	if got := readFileT(t, exclude); !strings.Contains(got, "/.claude/agents/") {
		t.Errorf(".git/info/exclude does not carry the pattern:\n%s", got)
	}
	// .gitignore is tracked and shared; writing a rule there would commit a
	// decision on behalf of everyone who clones the project.
	if _, err := os.Lstat(gitignore); err == nil {
		t.Error("a .gitignore was created; the rule belongs in .git/info/exclude")
	}
}

// TestRunLink_IgnoredAgentsDirectoryIsReportedAsLocalOnly: the benign case,
// which needs reporting rather than refusing.
func TestRunLink_IgnoredAgentsDirectoryIsReportedAsLocalOnly(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	target := globalAgent(t, repoRoot, "shared", body)

	projectDir := filepath.Join(base, "ignored")
	initRepo(t, projectDir)
	mustWriteT(t, filepath.Join(projectDir, ".gitignore"), ".claude/\n")
	gitRun(t, projectDir, "add", ".")
	gitRun(t, projectDir, "commit", "-qm", "ignore .claude")
	path := projectAgentPath(base, "ignored", "shared")
	mustWriteT(t, path, body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "local to this machine") {
		t.Errorf("the report does not say the links are local-only:\n%s", stdout)
	}
	assertIsLink(t, path, target)
}

// --- flags ------------------------------------------------------------------

// TestRunLink_AgentRequiresProject: linking an agent into every project found
// under the scan root is never what anyone means, so the combination is
// refused with a message rather than obeyed.
func TestRunLink_AgentRequiresProject(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "proj"))

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--agent", "shared", "--apply")...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitFailure, stdout, stderr)
	}
	if !strings.Contains(stderr, "--project") {
		t.Errorf("the refusal does not name the missing flag:\n%s", stderr)
	}
	if _, err := os.Lstat(projectAgentPath(base, "proj", "shared")); err == nil {
		t.Error("a link was created despite the refused flag combination")
	}

	// The valid form provisions the project it names, and only that one.
	mustMkdirT(t, filepath.Join(base, "other"))
	code, stdout, stderr = runCLI(t,
		linkArgs(t, repoRoot, base, "--project", "proj", "--agent", "shared", "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(repoRoot, "agents", "shared.md"))
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	assertIsLink(t, projectAgentPath(base, "proj", "shared"), resolved)
	if _, err := os.Lstat(projectAgentPath(base, "other", "shared")); err == nil {
		t.Error("an unselected project was provisioned")
	}
}

// TestRunLink_AgentNameCannotEscapeTheProject is the reviewer's reproduction:
// `--agent '../../../agents/shared'` reported "Linked 1 of 1 file" at exit 0
// and created the link outside any project. Both safety gates had been computed
// for the project's agents directory, so the write that actually happened was
// checked against neither.
func TestRunLink_AgentNameCannotEscapeTheProject(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "Proj", "trav"))

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base,
		"--project", "Proj/trav", "--agent", "../../../agents/shared", "--apply")...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitFailure, stdout, stderr)
	}
	if !strings.Contains(stderr, "agent") {
		t.Errorf("the refusal does not explain itself:\n%s", stderr)
	}
	// The escape target, one level above the project tree.
	if _, err := os.Lstat(filepath.Join(base, "Proj", "agents")); err == nil {
		t.Error("a link was created outside the project")
	}
	if _, err := os.Lstat(filepath.Join(base, "agents")); err == nil {
		t.Error("a link was created outside the project tree entirely")
	}
}

// TestRunLink_TrackedProjectWithAnUnaskableGitIsRefused is the CLI form of the
// gate being silently off: git exits non-zero, a repository plainly exists, and
// the adoptable file must survive.
func TestRunLink_TrackedProjectWithAnUnaskableGitIsRefused(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)
	mustWriteT(t, filepath.Join(base, "proj", ".git"), "gitdir: /nonexistent-agentsync-test\n")

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitAtRisk {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "blocked") {
		t.Errorf("the report does not say the project was refused:\n%s", stdout)
	}
	assertIsRegular(t, path, "a project whose git could not be asked must not be linked into")
}

// --- the home guard ---------------------------------------------------------

// TestRunLink_WrongHomeWritesNothing: $HOME is a real, readable directory that
// is simply not the one these projects live under — the sudo case. Every key
// falls through to _outside-home/, so every counterpart lookup behind a link is
// against the wrong project. It must not run at all.
func TestRunLink_WrongHomeWritesNothing(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)

	wrongHome := t.TempDir()
	t.Setenv("HOME", wrongHome)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d: a wrong $HOME must stop a write path", code, exitFailure)
	}
	if !strings.Contains(stderr, "$HOME") || !strings.Contains(stderr, wrongHome) {
		t.Errorf("the refusal does not name $HOME and the value in use:\n%s", stderr)
	}
	if !strings.Contains(stderr, "refusing to write") {
		t.Errorf("the refusal does not say that nothing was written:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("a refused run must not emit a report:\n%s", stdout)
	}
	assertIsRegular(t, path, "nothing may be written under a wrong $HOME")

	// The legitimate case the guard must not break: projects that really do
	// live outside $HOME, opted in explicitly.
	code, _, stderr = runCLI(t, linkArgs(t, repoRoot, base, "--apply", "--allow-outside-home")...)
	if code != exitOK {
		t.Fatalf("opted-in exit = %d, want %d\nstderr:\n%s", code, exitOK, stderr)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the opted-in run did not link %s", path)
	}
}

// --- JSON -------------------------------------------------------------------

func TestRunLink_JSONReport(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	mustWriteT(t, projectAgentPath(base, "proj", "shared"), body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--json")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitOK, stderr)
	}

	var rep struct {
		Apply    bool             `json:"apply"`
		Items    []map[string]any `json:"items"`
		Counts   map[string]int   `json:"counts"`
		Blocked  []any            `json:"blocked"`
		Git      []any            `json:"git"`
		Excludes []any            `json:"excludes"`
		Written  []string         `json:"written"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, stdout)
	}
	if rep.Apply {
		t.Error("a dry run reported apply=true")
	}
	if len(rep.Items) != 1 {
		t.Fatalf("items = %v, want one", rep.Items)
	}
	// The action name is carried alongside the number so a jq pipeline can
	// read the report without a copy of the enum.
	if got := rep.Items[0]["action_name"]; got != "adopt" {
		t.Errorf("action_name = %v, want adopt", got)
	}
	if rep.Counts["adopt"] != 1 {
		t.Errorf("counts = %v, want one adopt", rep.Counts)
	}
	// Every array is emitted rather than left null, so a consumer can index
	// without a nil check.
	if rep.Blocked == nil || rep.Git == nil || rep.Excludes == nil || rep.Written == nil {
		t.Errorf("a null array in the report: %+v", rep)
	}
}

// --- the state file and declined markers ------------------------------------

// TestRunLink_ApplyRecordsWhatItCreated: the record is what a later pass reads
// to tell this tool's links from handmade ones, so a run that links without
// recording has produced something nothing can ever clean up.
func TestRunLink_ApplyRecordsWhatItCreated(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	target := globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, ".agentsync-state.json") {
		t.Errorf("the report does not name the state file:\n%s", stdout)
	}

	repo := mustRepo(t, repoRoot)
	st, err := state.Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec, ok := st.LookupLink(path)
	if !ok {
		t.Fatalf("no record for the link at %s", path)
	}
	if rec.Origin != state.OriginCreated {
		t.Errorf("origin = %v, want created", rec.Origin)
	}
	if rec.Target != target {
		t.Errorf("target = %q, want %q", rec.Target, target)
	}
	if rec.Project != "proj" || rec.Name != "shared" {
		t.Errorf("record names %s, want proj/shared", rec.Key)
	}
}

// TestRunLink_DryRunRecordsNothing.
func TestRunLink_DryRunRecordsNothing(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	mustWriteT(t, projectAgentPath(base, "proj", "shared"), body)

	if code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base)...); code != exitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Lstat(filepath.Join(repoRoot, ".agentsync-state.json")); err == nil {
		t.Error("a dry run wrote a state file")
	}
}

// TestRunLink_DeclinedAgentIsReportedAndNotRecreated, with exit 0: the marker
// records a decision the user made, and failing on it would exit 1 forever.
func TestRunLink_DeclinedAgentIsReportedAndNotRecreated(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "proj"))
	declineInState(t, repoRoot, "proj", "shared")

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base,
		"--apply", "--project", "proj", "--agent", "shared")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "declined") {
		t.Errorf("the report does not mention the declined agent:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--undecline") {
		t.Errorf("the report does not say how to change the decision:\n%s", stdout)
	}
	if _, err := os.Lstat(projectAgentPath(base, "proj", "shared")); err == nil {
		t.Error("the declined link was recreated")
	}
}

// TestRunLink_UndeclineClearsTheMarkerAndLinks: one run does both, so the flag
// is an override rather than a two-step ritual.
func TestRunLink_UndeclineClearsTheMarkerAndLinks(t *testing.T) {
	base, repoRoot := linkFixture(t)
	target := globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "proj"))
	declineInState(t, repoRoot, "proj", "shared")

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base,
		"--apply", "--project", "proj", "--agent", "shared", "--undecline=proj/shared")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	assertIsLink(t, projectAgentPath(base, "proj", "shared"), target)
	if !strings.Contains(stdout, "1 declined marker was cleared in") || strings.Contains(stdout, "NOT cleared") {
		t.Errorf("a saved undecline is not reported as cleared:\n%s", stdout)
	}

	st, err := state.Load(mustRepo(t, repoRoot))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.IsDeclined(state.Key{Project: "proj", Name: "shared"}) {
		t.Error("the marker survived --undecline --apply")
	}
	if _, ok := st.LookupLink(projectAgentPath(base, "proj", "shared")); !ok {
		t.Error("the newly created link was not recorded")
	}
}

// TestRunLink_UndeclineNeedsApply: clearing a marker is a write like any other.
func TestRunLink_UndeclineNeedsApply(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "proj"))
	declineInState(t, repoRoot, "proj", "shared")

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base,
		"--project", "proj", "--agent", "shared", "--undecline=proj/shared")...)
	if code != exitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "would be cleared") {
		t.Errorf("the dry run does not say the marker would be cleared:\n%s", stdout)
	}

	st, err := state.Load(mustRepo(t, repoRoot))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !st.IsDeclined(state.Key{Project: "proj", Name: "shared"}) {
		t.Error("a dry run cleared the marker on disk")
	}
}

// TestRunLink_UndeclineThatNamesNoMarkerIsReportedNotFailed. No marker is the
// state the user asked for, so the successful command's second run must not be
// the failing one.
func TestRunLink_UndeclineThatNamesNoMarkerIsReportedNotFailed(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")
	mustMkdirT(t, filepath.Join(base, "proj"))

	code, stdout, stderr := runCLI(t, linkArgs(t, repoRoot, base,
		"--apply", "--project", "proj", "--agent", "shared", "--undecline=proj/typo")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "proj/typo") {
		t.Errorf("the report does not name the selector that matched nothing:\n%s", stdout)
	}
}

// TestRunLink_UndeclineRefusesAValueThatIsNotAPair. The case that matters is
// `--undecline --apply`, where --apply would otherwise be swallowed as the
// value and the run would silently become a dry run.
func TestRunLink_UndeclineRefusesAValueThatIsNotAPair(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")

	code, _, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--undecline", "--apply")...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; --apply was swallowed as a selector", code, exitFailure)
	}
	if !strings.Contains(stderr, "undecline") {
		t.Errorf("stderr does not say which flag was wrong:\n%s", stderr)
	}
}

// TestRunLink_RefusesAPositionalArgument, as import does: an argument that
// lands there is one the user believed was doing something.
func TestRunLink_RefusesAPositionalArgument(t *testing.T) {
	base, repoRoot := linkFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical shared agent\n")

	code, _, stderr := runCLI(t, linkArgs(t, repoRoot, base, "proj/shared")...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "--undecline=proj/shared") {
		t.Errorf("stderr does not suggest the attached-value form:\n%s", stderr)
	}
}

// TestRunLink_AnUnreadableStateFileStopsTheRun. An empty document would claim
// nothing was ever declined, so a parse failure must not reach the planner.
func TestRunLink_AnUnreadableStateFileStopsTheRun(t *testing.T) {
	base, repoRoot := linkFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "shared", body)
	path := projectAgentPath(base, "proj", "shared")
	mustWriteT(t, path, body)
	mustWriteT(t, filepath.Join(repoRoot, ".agentsync-state.json"), "{\"schema\":99}\n")

	code, _, stderr := runCLI(t, linkArgs(t, repoRoot, base, "--apply")...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "schema") {
		t.Errorf("stderr does not explain the refusal:\n%s", stderr)
	}
	assertIsRegular(t, path, "a run that could not read its own record must write nothing")
}

// mustRepo resolves the repo the way the command does, so paths compare equal.
func mustRepo(t *testing.T, repoRoot string) *scan.Repo {
	t.Helper()
	repo, err := scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return repo
}

// declineInState plants a marker the way reconcile will, since nothing in this
// build writes one.
func declineInState(t *testing.T, repoRoot, project, name string) {
	t.Helper()
	repo := mustRepo(t, repoRoot)
	st, err := state.Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := st.Decline(state.Key{Project: project, Name: name},
		"link gone, nothing in its place", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if err := st.Save(repo); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// unsaveableFixture is linkFixture with the repo placed where its state file's
// path just fits the platform limit, so Load reads it as absent but Save's
// longer temporary name cannot be created. Project "p" and agent "a" keep every
// other repo path inside the limit.
func unsaveableFixture(t *testing.T) (base, repoRoot string) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	repoRoot = testfs.DirWithRoom(t, base, len(string(filepath.Separator)+state.FileName))
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)
	return base, repoRoot
}

func TestRunLink_StateSaveFailureCountsTheLinkAsNotRecorded(t *testing.T) {
	base, repoRoot := unsaveableFixture(t)
	const body = "canonical shared agent\n"
	target := globalAgent(t, repoRoot, "a", body)
	path := projectAgentPath(base, "p", "a")
	mustWriteT(t, path, body)
	inProject(t, repoRoot, filepath.Join(base, "p"))

	code, stdout, stderr := runCLI(t, "link", "--apply")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitFailure, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 link was created but NOT recorded") {
		t.Errorf("the report does not count the link as unrecorded:\n%s", stdout)
	}
	if strings.Contains(stdout, " recorded in ") {
		t.Errorf("the report claims a record the save never wrote:\n%s", stdout)
	}
	if !strings.Contains(stderr, ".agentsync-state-") {
		t.Errorf("stderr does not report the failed save:\n%s", stderr)
	}
	assertIsLink(t, path, target)
}

func TestRunLink_StateSaveFailureJSONRecordsNothing(t *testing.T) {
	base, repoRoot := unsaveableFixture(t)
	const body = "canonical shared agent\n"
	globalAgent(t, repoRoot, "a", body)
	mustWriteT(t, projectAgentPath(base, "p", "a"), body)
	inProject(t, repoRoot, filepath.Join(base, "p"))

	code, stdout, stderr := runCLI(t, "link", "--apply", "--json")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitFailure, stderr)
	}
	var rep struct {
		Recorded []any    `json:"recorded"`
		Written  []string `json:"written"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, stdout)
	}
	if len(rep.Written) != 1 {
		t.Errorf("written = %v, want the one link", rep.Written)
	}
	if rep.Recorded == nil || len(rep.Recorded) != 0 {
		t.Errorf("recorded = %v, want [] after a failed save", rep.Recorded)
	}
}

func TestRunLink_StateSaveFailureLeavesTheMarkerDeclined(t *testing.T) {
	base, repoRoot := unsaveableFixture(t)
	globalAgent(t, repoRoot, "a", "canonical shared agent\n")
	mustMkdirT(t, filepath.Dir(projectAgentPath(base, "p", "a")))
	// Written directly: the state file's own path fits, only Save's temporary
	// name does not.
	repo := mustRepo(t, repoRoot)
	st := state.New()
	key := state.Key{Project: "p", Name: "a"}
	if err := st.Decline(key, "link gone, nothing in its place", time.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	b, err := st.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(state.Path(repo), b, 0o600); err != nil {
		t.Fatalf("writing the state file: %v", err)
	}
	inProject(t, repoRoot, filepath.Join(base, "p"))
	args := []string{"link", "--agent", "a", "--undecline=p/a", "--apply"}

	code, stdout, stderr := runCLI(t, args...)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitFailure, stdout, stderr)
	}
	if strings.Contains(stdout, "was cleared") || strings.Contains(stdout, "— cleared") {
		t.Errorf("the report claims a cleared marker the save never wrote:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 declined marker was NOT cleared") {
		t.Errorf("the report does not say the marker was not cleared:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if got, err := state.Load(repo); err != nil || !got.IsDeclined(key) {
		t.Errorf("the marker is gone from disk (load error %v)", err)
	}

	code, stdout, _ = runCLI(t, append(args, "--json")...)
	if code != exitFailure {
		t.Fatalf("--json exit = %d, want %d", code, exitFailure)
	}
	var rep struct {
		Undeclined []string `json:"undeclined"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, stdout)
	}
	if rep.Undeclined == nil || len(rep.Undeclined) != 0 {
		t.Errorf("undeclined = %v, want [] after a failed save", rep.Undeclined)
	}
}
