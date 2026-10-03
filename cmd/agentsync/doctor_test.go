package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/doctor"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// doctorFixture builds a fake home holding the agent repo and the projects
// beside it, and points $HOME at it.
//
// Pointing $HOME is not decoration: every project key is derived from it, and
// doctor turns those keys back into project directories to decide whether a
// capture is stale. A fixture leaving the developer's real $HOME in place would
// report every capture orphaned.
func doctorFixture(t *testing.T) (base, repoRoot string) {
	t.Helper()

	base = t.TempDir()
	repoRoot = filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)
	return base, repoRoot
}

func doctorArgs(repoRoot, root string, extra ...string) []string {
	return append([]string{"doctor", "--repo", repoRoot, "--root", root}, extra...)
}

// capturePath is where an import would have written a project's copy.
func capturePath(repoRoot, project, name string) string {
	return filepath.Join(repoRoot, "project-agents", filepath.FromSlash(project), name+".md")
}

func linkAt(t *testing.T, path, target string) string {
	t.Helper()
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
	return path
}

func doctorJSON(t *testing.T, out string) doctorReport {
	t.Helper()
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, out)
	}
	return rep
}

// --- exit codes -------------------------------------------------------------

// A broken link is reported, exits 1, and is still there afterwards. All three
// are the contract; the third is the one a careless "fix" would break.
func TestRunDoctor_BrokenLinkExitsOneAndIsLeftInPlace(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	missing := filepath.Join(repoRoot, "agents", "renamed-away.md")
	link := linkAt(t, projectAgentPath(base, "proj", "renamed-away"), missing)

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "broken-link") {
		t.Errorf("the report does not name the finding:\n%s", stdout)
	}
	if !strings.Contains(stdout, "renamed-away") {
		t.Errorf("the report does not name the agent:\n%s", stdout)
	}
	if !strings.Contains(stdout, missing) {
		t.Errorf("the report does not name the missing target:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NOT unlinked") {
		t.Errorf("the report does not say the link was left alone:\n%s", stdout)
	}

	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the broken link is gone (%v); doctor never unlinks", err)
	}
}

// A foreign link is reported and changes nothing about the exit code. Any other
// answer makes doctor unusable on a machine running a second agent tool, and an
// exit code nobody can act on is worse than none.
func TestRunDoctor_ForeignLinkAloneStillExitsZero(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	elsewhere := filepath.Join(base, "elsewhere", "theirs.md")
	mustWriteT(t, elsewhere, "not ours\n")
	linkAt(t, projectAgentPath(base, "proj", "theirs"), elsewhere)

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d: a foreign link is not a fault\nstdout:\n%s\nstderr:\n%s",
			code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "foreign-link") {
		t.Errorf("the foreign link was not reported at all:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Nothing has drifted") {
		t.Errorf("the summary does not read as clean:\n%s", stdout)
	}
}

// doctor reports drift and only drift, so its exit 0 must not read as an
// all-clear for the toolchain. link refuses on the import-before-link
// invariant, on a git-tracked agents directory, and on an unknown --project
// key, and none of those is drift or doctor's to find.
func TestRunDoctor_CleanSummaryDoesNotClaimLinkWouldRunClean(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	target := globalAgent(t, repoRoot, "shared", "canonical\n")
	linkAt(t, projectAgentPath(base, "proj", "shared"), target)

	code, stdout, _ := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if strings.Contains(stdout, "Nothing needs attention") {
		t.Errorf("the summary claims nothing needs attention, which is broader than what doctor checked:\n%s", stdout)
	}
	if !strings.Contains(stdout, "statement about drift only") {
		t.Errorf("the summary does not scope itself to drift:\n%s", stdout)
	}
	if !strings.Contains(stdout, "agentsync link") {
		t.Errorf("the summary does not point at the command that answers the other question:\n%s", stdout)
	}
}

// A tree with nothing wrong in it exits 0 and says so, with an empty finding
// list rather than a suppressed one.
func TestRunDoctor_CleanTreeExitsZeroWithAnEmptyReport(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	target := globalAgent(t, repoRoot, "shared", "canonical\n")
	linkAt(t, projectAgentPath(base, "proj", "shared"), target)

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "Nothing to report") {
		t.Errorf("the report does not say the tree is clean:\n%s", stdout)
	}

	_, jsonOut, _ := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	rep := doctorJSON(t, jsonOut)
	if len(rep.Findings) != 0 {
		t.Errorf("Findings = %v, want []", rep.Findings)
	}
	if rep.Findings == nil {
		t.Error("Findings marshalled as null; every array this tool emits is [] when empty")
	}
}

// --- captures ---------------------------------------------------------------

// The live shape, reduced: a project that still exists but no longer holds an
// agent this repo captured for it.
func TestRunDoctor_StaleCaptureExitsOneAndTheCaptureSurvives(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	mustMkdirT(t, filepath.Join(base, "Web", "example"))
	kept := capturePath(repoRoot, "Web/example", "golang-developer")
	mustWriteT(t, kept, "a 44-line predecessor\n")

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "stale-capture") {
		t.Errorf("the report does not name the finding:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Web/example") || !strings.Contains(stdout, "golang-developer") {
		t.Errorf("the report does not name the project and agent:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Do NOT reach for rm") {
		t.Errorf("the summary does not warn against deleting the capture:\n%s", stdout)
	}

	got, err := os.ReadFile(kept)
	if err != nil || string(got) != "a 44-line predecessor\n" {
		t.Errorf("the capture was removed or modified: %q, %v", got, err)
	}
}

// An orphaned capture directory is one finding about a directory, not N about
// its files. The difference is the whole point: "this project is gone" and "this
// project dropped one agent" call for different decisions.
func TestRunDoctor_OrphanedCaptureDirIsReportedAsOneDirectory(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	mustWriteT(t, capturePath(repoRoot, "Web/deleted", "one"), "a\n")
	mustWriteT(t, capturePath(repoRoot, "Web/deleted", "two"), "b\n")

	// A second, living project with a capture of its own. Without it every
	// capture directory in the fixture would be orphaned, which is the
	// home-suspect shape rather than the one this test is about — and the
	// distinction is the point: one gone project among several live ones is
	// evidence about that project, not about $HOME.
	const alive = "still here\n"
	mustWriteT(t, projectAgentPath(base, "Web/living", "kept"), alive)
	mustWriteT(t, capturePath(repoRoot, "Web/living", "kept"), alive)

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "orphaned-captures") {
		t.Errorf("the report does not name the finding:\n%s", stdout)
	}
	if strings.Contains(stdout, "stale-capture") {
		t.Errorf("a gone project was also reported file-by-file:\n%s", stdout)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("one gone project among live ones must not be blamed on $HOME:\n%s", stderr)
	}

	_, jsonOut, _ := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	rep := doctorJSON(t, jsonOut)
	if len(rep.Findings) != 1 {
		t.Fatalf("Findings = %v, want exactly one", rep.Findings)
	}
	if rep.Findings[0].KindName != "orphaned-captures" {
		t.Errorf("kind_name = %q, want orphaned-captures", rep.Findings[0].KindName)
	}
	if rep.Findings[0].Name != "" {
		t.Errorf("the finding names an agent (%q); it is about the whole directory", rep.Findings[0].Name)
	}
	if rep.Counts["orphaned-captures"] != 1 {
		t.Errorf("counts = %v, want one orphaned-captures", rep.Counts)
	}

	for _, name := range []string{"one", "two"} {
		if _, err := os.Lstat(capturePath(repoRoot, "Web/deleted", name)); err != nil {
			t.Errorf("capture %s was removed (%v); doctor never deletes", name, err)
		}
	}
}

// A mismatch carries the command that fixes it. Refusing to act is only
// reasonable if the report says what acting looks like — and the command has to
// be the one link actually accepts, which is why the string comes from linker
// rather than from a copy here.
func TestRunDoctor_MismatchCarriesTheRemedyCommand(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	global := globalAgent(t, repoRoot, "web-researcher", "the global one\n")
	want := capturePath(repoRoot, "proj", "web-researcher")
	mustWriteT(t, want, "this project's customized one\n")
	linkAt(t, projectAgentPath(base, "proj", "web-researcher"), global)

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "mismatch") {
		t.Errorf("the report does not name the finding:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--project proj --agent web-researcher") {
		t.Errorf("the report does not carry link's remedy command:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NOT repointed") {
		t.Errorf("the report does not say the link was left alone:\n%s", stdout)
	}
}

// project-agents/README.md documents the tree and is not a capture of anything.
func TestRunDoctor_ReadmeIsNeverACapture(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	mustWriteT(t, filepath.Join(repoRoot, "project-agents", "README.md"), "# project-agents\n")

	code, stdout, _ := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	rep := doctorJSON(t, stdout)
	if rep.Examined.Captures != 0 {
		t.Errorf("examined.captures = %d, want 0", rep.Examined.Captures)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("Findings = %v, want none", rep.Findings)
	}
}

// --- the scan's own gaps ----------------------------------------------------

// A subtree the walk could not enter may hide exactly the drift being asked
// about, so "nothing is wrong" cannot be said about it. Same rule as import and
// link: an incomplete diagnosis must not exit the way a clean one does.
//
// Structural rather than chmod 000 (issue #11), so the CI gate — which runs as
// root, for whom permission bits are advisory — actually runs it. The chmod
// form skipped there, and a skip prints nothing.
func TestRunDoctor_UnreadableSubtreeIsNotACleanRun(t *testing.T) {
	base, repoRoot := doctorFixture(t)

	locked := filepath.Join(base, "locked")
	mustMkdirT(t, locked)
	unreadable, inside := testfs.UnreadableSubtree(t, locked)
	testfs.PlantAgentsDir(t, inside, "proj")

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "could not be classified") {
		t.Errorf("the report does not account for the unreadable subtree:\n%s", stdout)
	}
	// The exact path rather than the prefix, matching the other four: on a
	// filesystem whose readdir returns DT_UNKNOWN, the walk fails one level
	// up instead, which is still correct but is not the arm this asserts.
	if !strings.Contains(stdout, unreadable) {
		t.Errorf("the report does not name the unreadable subtree:\n%s", stdout)
	}
	if strings.Contains(stdout, "Nothing needs attention.") {
		t.Errorf("the summary reads as an unqualified all-clear over a tree it could not read:\n%s", stdout)
	}
}

// --- the home guard ---------------------------------------------------------

// doctor warns about an implausible $HOME and carries on, exactly as scan does.
// RequireHome is the write path's guard, and there is no write here to guard: a
// wrong home makes doctor over-report (captures look orphaned because their keys
// resolve somewhere else), which is loud, harmless, and visible — while
// refusing would break the legitimate projects-outside-$HOME case for a
// read-only command.
func TestRunDoctor_ImplausibleHomeWarnsRatherThanFails(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	target := globalAgent(t, repoRoot, "shared", "canonical\n")
	linkAt(t, projectAgentPath(base, "proj", "shared"), target)

	// A real, valid home directory that is simply not the one these projects
	// live under — the shape `sudo -i` produces.
	t.Setenv("HOME", t.TempDir())

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	if code == exitFailure {
		t.Errorf("exit = %d: a read-only command must not refuse over $HOME\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Errorf("no warning about the implausible $HOME:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--allow-outside-home") {
		t.Errorf("the warning does not name the opt-out flag:\n%s", stderr)
	}

	_, _, quiet := runCLI(t, doctorArgs(repoRoot, base, "--allow-outside-home")...)
	if strings.Contains(quiet, "warning:") {
		t.Errorf("--allow-outside-home did not suppress the warning:\n%s", quiet)
	}
}

// The shape that scan.CheckHome is structurally blind to, and the one doctor
// is uniquely exposed to: no projects found, so the scanned-directory check has
// nothing to say, while every capture key is still resolved through a home that
// is not the one those projects live under. Without a second signal the run
// reports every capture in the repo as belonging to a deleted project, with the
// summary pointing at rm and $HOME mentioned nowhere.
//
// A wrong $HOME tends to PRODUCE this shape rather than merely coincide with
// it: the derived root falls back to $HOME/Web and the walk that derives it
// stops at $HOME, so the same mistake that misplaces the home also empties the
// root that would otherwise supply the evidence.
func TestRunDoctor_CapturesWarnAboutHomeEvenWhenNoProjectWasScanned(t *testing.T) {
	_, repoRoot := doctorFixture(t)

	// Captures for three projects, none of which exists under the home the
	// run will use.
	for _, key := range []string{"Web/one", "Web/two", "Web/nested/three"} {
		mustWriteT(t, capturePath(repoRoot, key, "agent"), "the only copy\n")
	}

	// A valid, readable home that is simply not the right one, and a root
	// with no projects in it — exactly what a container or a sudo run gives.
	t.Setenv("HOME", t.TempDir())
	emptyRoot := t.TempDir()

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, emptyRoot)...)

	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s", code, exitAtRisk, stdout)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("stderr is silent while every capture was reported orphaned:\n%s\nstdout:\n%s", stderr, stdout)
	}
	if !strings.Contains(stderr, "home directory") {
		t.Errorf("the warning does not name the home directory as the suspect:\n%s", stderr)
	}
	if !strings.Contains(stderr, "3 of 3") {
		t.Errorf("the warning does not show the evidence it rests on:\n%s", stderr)
	}

	// stdout too: a piped run discards stderr, and the line that points a
	// reader at rm lives in stdout.
	if !strings.Contains(stdout, "BUT:") {
		t.Errorf("the summary asserts the projects are gone with no caveat:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not proof that anything was deleted") {
		t.Errorf("the summary does not withdraw the claim the orphan line makes:\n%s", stdout)
	}

	// --allow-outside-home must not silence it: that flag claims the projects
	// live outside $HOME, and such projects key under _outside-home/<abs path>
	// and resolve to real directories, so it says nothing about this evidence.
	_, _, stillLoud := runCLI(t, doctorArgs(repoRoot, emptyRoot, "--allow-outside-home")...)
	if !strings.Contains(stillLoud, "warning:") {
		t.Errorf("--allow-outside-home silenced a warning it makes no claim about:\n%s", stillLoud)
	}
}

// An _outside-home/ capture carries an absolute path that scan.ProjectDir
// resolves without reading the home directory at all, so its project being gone
// is zero evidence about that home. Counting it would make the warning blame a
// directory that took no part in the lookup — and it is exactly the honest
// orphan a user who set --allow-outside-home is most likely to produce, since
// that flag describes precisely this layout.
func TestRunDoctor_OutsideHomeOrphanDoesNotBlameTheHome(t *testing.T) {
	base, repoRoot := doctorFixture(t)

	// A key in the absolute-path namespace, naming a directory no machine
	// has. ProjectDir turns it back into /nonexistent-volume-for-tests/gone.
	const key = "_outside-home/nonexistent-volume-for-tests/gone"
	mustWriteT(t, capturePath(repoRoot, key, "agent"), "the only copy\n")

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)

	// The orphan itself is real and still reported.
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstdout:\n%s", code, exitAtRisk, stdout)
	}
	if !strings.Contains(stdout, "orphaned-captures") {
		t.Errorf("the orphan was not reported at all:\n%s", stdout)
	}

	// But the home is not implicated, on either channel.
	if strings.Contains(stderr, "home directory in use") {
		t.Errorf("a home-independent key was blamed on the home directory:\n%s", stderr)
	}
	if strings.Contains(stdout, "BUT:") {
		t.Errorf("the summary caveats a finding the home took no part in:\n%s", stdout)
	}

	_, jsonOut, _ := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	rep := doctorJSON(t, jsonOut)
	if rep.Home.Suspect {
		t.Errorf("home.suspect = true for a key the home never resolved:\n%s", jsonOut)
	}
	if rep.Examined.CaptureDirs != 1 {
		t.Errorf("examined.capture_dirs = %d, want 1", rep.Examined.CaptureDirs)
	}
	if rep.Examined.HomeKeyedCaptureDirs != 0 {
		t.Errorf("examined.home_keyed_capture_dirs = %d, want 0: the only capture is outside the home namespace",
			rep.Examined.HomeKeyedCaptureDirs)
	}
}

// The same signal in the JSON, where it matters most: a machine consumer
// discards stderr, and {"orphaned-captures": 3} is otherwise indistinguishable
// from three genuinely deleted projects.
func TestRunDoctor_JSONCarriesTheHomeSignal(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	mustWriteT(t, capturePath(repoRoot, "Web/one", "agent"), "the only copy\n")

	t.Setenv("HOME", t.TempDir())
	_, stdout, _ := runCLI(t, doctorArgs(repoRoot, t.TempDir(), "--json")...)

	rep := doctorJSON(t, stdout)
	if !rep.Home.Suspect {
		t.Errorf("home.suspect = false while every capture was reported orphaned:\n%s", stdout)
	}
	if rep.Home.Warning == "" {
		t.Error("home.warning is empty, so a machine consumer sees the doubt but not the reason")
	}
	if rep.Home.Path == "" {
		t.Error("home.path is empty; a user cannot fix a home directory they were never shown")
	}

	// And absent when there is nothing to doubt, so the field is a signal
	// rather than decoration.
	t.Setenv("HOME", base)
	mustMkdirT(t, filepath.Join(base, "Web", "one"))
	_, clean, _ := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	if got := doctorJSON(t, clean); got.Home.Suspect || got.Home.Warning != "" {
		t.Errorf("home signal fired on a run with a correct home: %+v", got.Home)
	}
}

// --- read-only --------------------------------------------------------------

// The command's central claim, asserted end to end: a doctor run leaves both the
// projects and this repo byte-identical, and creates no file anywhere — no state
// file, no .git/info/exclude line, nothing.
func TestRunDoctor_WritesNothingAnywhere(t *testing.T) {
	base, repoRoot := doctorFixture(t)

	// One of everything doctor reports on, so the assertion covers every path
	// that might be tempted to fix something.
	global := globalAgent(t, repoRoot, "shared", "canonical\n")
	linkAt(t, projectAgentPath(base, "healthy", "shared"), global)
	linkAt(t, projectAgentPath(base, "broken", "renamed-away"),
		filepath.Join(repoRoot, "agents", "renamed-away.md"))
	mustWriteT(t, filepath.Join(base, "elsewhere", "theirs.md"), "not ours\n")
	linkAt(t, projectAgentPath(base, "foreign", "theirs"), filepath.Join(base, "elsewhere", "theirs.md"))
	mustWriteT(t, capturePath(repoRoot, "mismatched", "shared"), "this project's own\n")
	linkAt(t, projectAgentPath(base, "mismatched", "shared"), global)
	mustMkdirT(t, filepath.Join(base, "stale"))
	mustWriteT(t, capturePath(repoRoot, "stale", "deleted-by-the-project"), "the last copy\n")
	mustWriteT(t, capturePath(repoRoot, "Web/gone", "orphaned"), "also the last copy\n")
	mustWriteT(t, projectAgentPath(base, "override", "shared"), "customized\n")
	mustWriteT(t, filepath.Join(repoRoot, "project-agents", "README.md"), "# project-agents\n")

	// base holds the repo too, so one snapshot covers both sides.
	before := treeSnapshot(t, base)
	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base)...)
	after := treeSnapshot(t, base)

	if code != exitAtRisk {
		t.Fatalf("exit = %d, want %d — the fixture must actually produce findings, or this proves nothing\nstdout:\n%s\nstderr:\n%s",
			code, exitAtRisk, stdout, stderr)
	}
	assertUnchanged(t, before, after)

	// And again in JSON, which walks the same code with a different emitter.
	before = treeSnapshot(t, base)
	runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	assertUnchanged(t, before, treeSnapshot(t, base))
}

// treeSnapshot records every path under root and what it is, following no
// symlink: content for a regular file, the literal target for a link.
//
// Lstat and Readlink rather than Stat and ReadFile, so a link silently replaced
// by a copy of its own target — the one corruption that leaves the bytes equal —
// still registers as a change.
func treeSnapshot(t *testing.T, root string) map[string]string {
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

func assertUnchanged(t *testing.T, before, after map[string]string) {
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

// --- JSON -------------------------------------------------------------------

// The JSON form has to carry the same verdict as the text one, including the
// numbers that distinguish "nothing is wrong" from "nothing was looked at".
func TestRunDoctor_JSONCarriesFindingsAndExamined(t *testing.T) {
	base, repoRoot := doctorFixture(t)
	missing := filepath.Join(repoRoot, "agents", "renamed-away.md")
	linkAt(t, projectAgentPath(base, "proj", "renamed-away"), missing)
	mustMkdirT(t, filepath.Join(base, "kept"))
	mustWriteT(t, capturePath(repoRoot, "kept", "stale-one"), "x\n")

	code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, base, "--json")...)
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\nstderr:\n%s", code, exitAtRisk, stderr)
	}

	rep := doctorJSON(t, stdout)
	if rep.Repo != repoRoot {
		// The repo path is resolved, so compare through the same resolution
		// the command applied rather than against the raw fixture path.
		resolved, err := filepath.EvalSymlinks(repoRoot)
		if err != nil || rep.Repo != resolved {
			t.Errorf("repo = %s, want %s", rep.Repo, repoRoot)
		}
	}
	if rep.Examined.Captures != 1 || rep.Examined.CaptureDirs != 1 {
		t.Errorf("examined = %+v, want 1 capture in 1 directory", rep.Examined)
	}
	if rep.Examined.Entries != 1 {
		t.Errorf("examined.entries = %d, want 1", rep.Examined.Entries)
	}

	kinds := make(map[doctor.Kind]bool)
	for _, f := range rep.Findings {
		kinds[f.Kind] = true
		if f.KindName != f.Kind.String() {
			t.Errorf("kind_name %q disagrees with kind %v", f.KindName, f.Kind)
		}
	}
	for _, want := range []doctor.Kind{doctor.KindBrokenLink, doctor.KindStaleCapture} {
		if !kinds[want] {
			t.Errorf("%s missing from the JSON findings: %v", want, rep.Findings)
		}
	}
	if rep.Problems == nil || rep.ScanProblems == nil {
		t.Error("problem arrays marshalled as null; every array this tool emits is [] when empty")
	}
}

// --- usage ------------------------------------------------------------------

func TestRunDoctor_IsListedInUsage(t *testing.T) {
	_, stdout, _ := runCLI(t)
	if !strings.Contains(stdout, "agentsync doctor") {
		t.Errorf("doctor is not listed in the usage block:\n%s", stdout)
	}
}
