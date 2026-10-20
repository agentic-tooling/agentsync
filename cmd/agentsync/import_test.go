package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// --- fixtures ---------------------------------------------------------------

// importFixture builds a fake home holding the agent repo and the projects
// beside it, and points $HOME at it.
//
// Pointing $HOME is not decoration. Every project key is derived from it, and
// import refuses to write when no project keys under it — so a fixture that
// left the developer's real $HOME in place would test the home guard firing,
// not the import.
func importFixture(t *testing.T) (base, repoRoot string) {
	t.Helper()

	base = t.TempDir()
	repoRoot = filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	t.Setenv("HOME", base)
	return base, repoRoot
}

func importArgs(t *testing.T, repoRoot, root string, extra ...string) []string {
	t.Helper()
	inTree(t, repoRoot, root)
	return append([]string{"import", "--" + allFlag}, extra...)
}

// commitRepo makes the fixture's agent repo a git repository and commits
// everything in it.
//
// --update refuses to replace a capture whose current bytes are in no commit
// here, because those bytes then exist nowhere else. So every CLI test that
// expects an update to go through has to commit first — the same thing the
// report tells a user to do.
func commitRepo(t *testing.T, repoRoot string) {
	t.Helper()
	requireGit(t)
	if _, err := os.Lstat(filepath.Join(repoRoot, ".git")); err != nil {
		initRepo(t, repoRoot)
	}
	gitRun(t, repoRoot, "add", "-A", ".")
	gitRun(t, repoRoot, "commit", "-q", "--allow-empty", "-m", "captures")
}

// projectAgent writes a file into a project's .claude/agents directory.
func projectAgent(t *testing.T, base, project, name, content string) string {
	t.Helper()
	p := filepath.Join(base, project, ".claude", "agents", name+".md")
	mustWriteT(t, p, content)
	return p
}

// backupPath is where a capture lands. The repo root is resolved the way
// scan.NewRepo resolves it — on macOS the test's temp dir is reached through
// /var, a symlink to /private/var — so a path from a report compares equal to
// one built here.
func backupPath(t *testing.T, repoRoot, project, name string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("resolving %s: %v", repoRoot, err)
	}
	return filepath.Join(resolved, "project-agents", filepath.FromSlash(project), name+".md")
}

// captured lists every file under the repo's project-agents tree, relative to
// it and sorted.
//
// "Nothing was written" has to be a statement about the whole tree rather than
// about one expected path: the failure this guards — a wrong $HOME — does not
// skip the write, it puts it under a different key.
func captured(t *testing.T, repoRoot string) []string {
	t.Helper()
	root := filepath.Join(repoRoot, "project-agents")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(found)
	return found
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// --- dry run ----------------------------------------------------------------

// TestRunImport_DryRunIsTheDefault: a write path defaults to describing itself.
// The report must name what it would capture, and the disk must be untouched.
func TestRunImport_DryRunIsTheDefault(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "DRY RUN") {
		t.Errorf("the report does not say it wrote nothing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--apply") {
		t.Errorf("the report does not say how to actually write:\n%s", stdout)
	}
	if !strings.Contains(stdout, "orphan") {
		t.Errorf("the report does not name the file it would capture:\n%s", stdout)
	}

	if got := captured(t, repoRoot); len(got) != 0 {
		t.Errorf("a dry run wrote %v; it must change nothing", got)
	}
	if _, err := os.Lstat(backupPath(t, repoRoot, "proj", "orphan")); err == nil {
		t.Error("the destination exists after a dry run")
	}
}

// --- apply and idempotence --------------------------------------------------

func TestRunImport_ApplyCapturesAndIsIdempotent(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")
	projectAgent(t, base, "nested/deeper", "other", "also only here\n")

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}

	want := []string{"nested/deeper/other.md", "proj/orphan.md"}
	if got := captured(t, repoRoot); !equalNames(got, want) {
		t.Fatalf("captured %v, want %v", got, want)
	}
	if got := readFileT(t, backupPath(t, repoRoot, "proj", "orphan")); got != "only here\n" {
		t.Errorf("captured content = %q", got)
	}

	// Second run: the captures are now byte-identical counterparts, so the
	// scan calls them adoptable and there is nothing left to do.
	code, stdout, stderr = runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Fatalf("second run exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "Nothing to import") {
		t.Errorf("second run did not report itself as a no-op:\n%s", stdout)
	}
	if got := captured(t, repoRoot); !equalNames(got, want) {
		t.Errorf("second run changed the tree to %v, want %v", got, want)
	}
}

// --- conflicts --------------------------------------------------------------

// TestRunImport_ConflictIsRefusedAndExitsNonZero. The destination is an earlier
// capture that may be the only copy of an older override, so it is never
// replaced by default — in a dry run or with --apply.
func TestRunImport_ConflictIsRefusedAndExitsNonZero(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "researcher", "the project's current file\n")
	dest := backupPath(t, repoRoot, "proj", "researcher")
	mustWriteT(t, dest, "an older capture nobody else has\n")

	for _, extra := range [][]string{{}, {"--apply"}} {
		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, extra...)...)
		if code != exitConflict {
			t.Errorf("%v: exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
				extra, code, exitConflict, stdout, stderr)
		}
		if !strings.Contains(stdout, "conflict") {
			t.Errorf("%v: the report does not mention the conflict:\n%s", extra, stdout)
		}
		if !strings.Contains(stdout, "--update") {
			t.Errorf("%v: the report does not say how to resolve it:\n%s", extra, stdout)
		}
		if got := readFileT(t, dest); got != "an older capture nobody else has\n" {
			t.Fatalf("%v: the older capture was overwritten with %q", extra, got)
		}
	}

	// The opt-in, and only the opt-in, replaces it — and only once the
	// capture it is about to destroy exists somewhere other than that path.
	commitRepo(t, repoRoot)
	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply", "--update")...)
	if code != exitOK {
		t.Fatalf("--update exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if got := readFileT(t, dest); got != "the project's current file\n" {
		t.Errorf("--update left %q, want the project file", got)
	}
}

// TestRunImport_UpdateRefusesAnUncommittedCapture is N4 at the CLI boundary:
// the refusal has to reach the user as an explanation and a next step, not as
// a flag that silently did nothing.
func TestRunImport_UpdateRefusesAnUncommittedCapture(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "researcher", "the project's current file\n")
	// A repo with history, and a capture written after it — the state every
	// import run leaves behind until someone commits.
	commitRepo(t, repoRoot)
	dest := backupPath(t, repoRoot, "proj", "researcher")
	mustWriteT(t, dest, "an older capture nobody has committed\n")

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply", "--update")...)
	if code != exitConflict {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitConflict, stdout, stderr)
	}
	if got := readFileT(t, dest); got != "an older capture nobody has committed\n" {
		t.Fatalf("the uncommitted capture was destroyed: %q", got)
	}
	// Naming the protection and the remedy is the requirement. "Refused" on
	// its own sends the user to look for a bug in the flag they just passed.
	for _, want := range []string{"in no commit", "git add project-agents"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not mention %q:\n%s", want, stdout)
		}
	}

	// The remedy works.
	commitRepo(t, repoRoot)
	code, stdout, stderr = runCLI(t, importArgs(t, repoRoot, base, "--apply", "--update")...)
	if code != exitOK {
		t.Fatalf("after committing, exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if got := readFileT(t, dest); got != "the project's current file\n" {
		t.Errorf("after committing, capture = %q, want the project file", got)
	}
}

// TestRunImport_SecondUpdateDoesNotDestroyTheFirst is the defect reproduced
// against the built binary, driven the same way here: two ordinary commands,
// no hand-editing.
//
// --update writes the project file over the capture, which leaves the capture
// modified against HEAD. A gate that only asked "is this path in HEAD" would
// authorise the next --update and destroy bytes that are in no commit.
func TestRunImport_SecondUpdateDoesNotDestroyTheFirst(t *testing.T) {
	base, repoRoot := importFixture(t)
	src := projectAgent(t, base, "proj", "researcher", "v2\n")
	dest := backupPath(t, repoRoot, "proj", "researcher")
	mustWriteT(t, dest, "v1\n")
	commitRepo(t, repoRoot)

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply", "--update")...)
	if code != exitOK {
		t.Fatalf("first update exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if got := readFileT(t, dest); got != "v2\n" {
		t.Fatalf("after the first update the capture is %q, want v2", got)
	}

	// The project moves on; nobody committed the capture of v2.
	mustWriteT(t, src, "v3\n")

	code, stdout, stderr = runCLI(t, importArgs(t, repoRoot, base, "--apply", "--update")...)
	if code != exitConflict {
		t.Errorf("second update exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitConflict, stdout, stderr)
	}
	if got := readFileT(t, dest); got != "v2\n" {
		t.Fatalf("capture = %q — the first update's result was destroyed and is in no commit", got)
	}
}

// TestRunImport_UpdateSelectsOneCapture is the per-item form. The blanket flag
// asks for authorisation over every differing capture in the run, and that is
// not how the decision is made — it is made one file at a time, after a diff.
func TestRunImport_UpdateSelectsOneCapture(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "chosen", "researcher", "chosen current\n")
	projectAgent(t, base, "other", "researcher", "other current\n")
	chosen := backupPath(t, repoRoot, "chosen", "researcher")
	other := backupPath(t, repoRoot, "other", "researcher")
	mustWriteT(t, chosen, "chosen old capture\n")
	mustWriteT(t, other, "other old capture\n")
	commitRepo(t, repoRoot)

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base,
		"--apply", "--update=chosen/researcher")...)
	// The unselected one is still a conflict, so the run still exits 1.
	if code != exitConflict {
		t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitConflict, stdout, stderr)
	}
	if got := readFileT(t, chosen); got != "chosen current\n" {
		t.Errorf("selected capture = %q, want the project file", got)
	}
	if got := readFileT(t, other); got != "other old capture\n" {
		t.Errorf("unselected capture = %q — a selector authorised its neighbour", got)
	}
}

// TestRunImport_UpdateSelectorSyntax covers the two ways a selector can be
// typed wrong. Both have to fail loudly, because both would otherwise be read
// as the blanket form — which replaces every differing capture in the run.
func TestRunImport_UpdateSelectorSyntax(t *testing.T) {
	t.Run("a spaced value is refused, not read as bare --update", func(t *testing.T) {
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "researcher", "current\n")
		dest := backupPath(t, repoRoot, "proj", "researcher")
		mustWriteT(t, dest, "old capture\n")
		commitRepo(t, repoRoot)

		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base,
			"--apply", "--update", "proj/researcher")...)
		if code != exitFailure {
			t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitFailure, stdout, stderr)
		}
		if !strings.Contains(stderr, "--update=proj/researcher") {
			t.Errorf("stderr does not show the form that works:\n%s", stderr)
		}
		if got := readFileT(t, dest); got != "old capture\n" {
			t.Fatalf("the capture was replaced by a command that did not parse: %q", got)
		}
	})

	t.Run("a selector naming no project is refused", func(t *testing.T) {
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "researcher", "current\n")

		for _, arg := range []string{"--update=researcher", "--update="} {
			code, _, stderr := runCLI(t, importArgs(t, repoRoot, base, arg)...)
			if code != exitFailure {
				t.Errorf("%s: exit = %d, want %d\nstderr:\n%s", arg, code, exitFailure, stderr)
			}
			// The flag package would have wrapped an error returned from
			// Set as `invalid boolean value "researcher" for -update`,
			// which names a type nobody asked for, and then printed the
			// whole usage block over the one line that matters.
			if strings.Contains(stderr, "invalid boolean value") {
				t.Errorf("%s: the message blames a boolean:\n%s", arg, stderr)
			}
			if strings.Contains(stderr, "agentsync scan") {
				t.Errorf("%s: the usage block buried the message:\n%s", arg, stderr)
			}
			if !strings.Contains(stderr, "<project>/<name>") {
				t.Errorf("%s: stderr does not name the expected form:\n%s", arg, stderr)
			}
		}
	})

	t.Run("a positional that is not a near-miss gets no selector hint", func(t *testing.T) {
		// Echoing the argument back unconditionally advises
		// `--update=--update`, which is nonsense, and for an empty
		// argument `--update=`, which is the one form that errors.
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "researcher", "current\n")

		for _, arg := range []string{"--", ""} {
			code, _, stderr := runCLI(t, importArgs(t, repoRoot, base, "--", arg)...)
			if code != exitFailure {
				t.Errorf("%q: exit = %d, want %d\nstderr:\n%s", arg, code, exitFailure, stderr)
			}
			if strings.Contains(stderr, "--update=") {
				t.Errorf("%q: stderr suggests a selector that cannot work:\n%s", arg, stderr)
			}
		}
	})

	t.Run("--update=false cancels an inherited --update", func(t *testing.T) {
		// A CI wrapper appending --update=false to a command line it did
		// not write must actually disable updates. Leaving the blanket bit
		// set would authorise replacing every differing capture in the run
		// after the user asked for none.
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "researcher", "current\n")
		dest := backupPath(t, repoRoot, "proj", "researcher")
		mustWriteT(t, dest, "old capture\n")
		commitRepo(t, repoRoot)

		code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base,
			"--apply", "--update", "--update=false")...)
		if code != exitConflict {
			t.Errorf("exit = %d, want %d (the capture is still a conflict)\n%s", code, exitConflict, stdout)
		}
		if got := readFileT(t, dest); got != "old capture\n" {
			t.Errorf("capture = %q — --update=false did not cancel the blanket authorisation", got)
		}
	})

	t.Run("a selector matching nothing is reported without failing the run", func(t *testing.T) {
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "researcher", "only here\n")
		commitRepo(t, repoRoot)

		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base,
			"--apply", "--update=proj/typo")...)
		// Exit 0: the selector is a fact about the command line, and the
		// repo did end up holding everything unprotected. The exit codes
		// assert the second thing, so they do not move for the first.
		if code != exitOK {
			t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
		}
		if !strings.Contains(stdout, "proj/typo") {
			t.Errorf("the report does not name the selector that matched nothing:\n%s", stdout)
		}
	})
}

// TestRunImport_JSONReportsTheScopeOfUpdate: the JSON is what an audit reads,
// and it must not understate what a destructive flag authorised.
//
// The two forms combine. `--update --update=nope/typo` names one selector and
// authorises everything, so a reader going by update_items alone would conclude
// the run could touch one file when it could touch all of them.
func TestRunImport_JSONReportsTheScopeOfUpdate(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "researcher", "current\n")
	mustWriteT(t, backupPath(t, repoRoot, "proj", "researcher"), "old capture\n")

	read := func(t *testing.T, extra ...string) map[string]any {
		t.Helper()
		_, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, append(extra, "--json")...)...)
		var rep map[string]any
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("unmarshal report: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		return rep
	}

	t.Run("blanket plus a selector is reported as blanket", func(t *testing.T) {
		rep := read(t, "--update", "--update=nope/typo")
		if rep["update"] != true || rep["update_blanket"] != true {
			t.Errorf("update=%v update_blanket=%v, want both true", rep["update"], rep["update_blanket"])
		}
	})

	t.Run("selectors alone are not blanket", func(t *testing.T) {
		rep := read(t, "--update=proj/researcher")
		if rep["update"] != true || rep["update_blanket"] != false {
			t.Errorf("update=%v update_blanket=%v, want true and false", rep["update"], rep["update_blanket"])
		}
	})

	t.Run("no --update at all", func(t *testing.T) {
		rep := read(t)
		if rep["update"] != false || rep["update_blanket"] != false {
			t.Errorf("update=%v update_blanket=%v, want both false", rep["update"], rep["update_blanket"])
		}
	})
}

// TestRunImport_FailOnPending is the CI form (N8). Without it a dry run over a
// tree full of uncaptured overrides exits 0, so there was no way to assert
// "everything is captured" without also authorising the writes.
func TestRunImport_FailOnPending(t *testing.T) {
	t.Run("a dry run with pending captures exits 1", func(t *testing.T) {
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "orphan", "only here\n")

		// The same run without the flag is the baseline: exit 0, which is
		// the gap the flag exists to close.
		if code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base)...); code != exitOK {
			t.Fatalf("without the flag, exit = %d, want %d\n%s", code, exitOK, stdout)
		}

		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--fail-on-pending")...)
		if code != exitAtRisk {
			t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitAtRisk, stdout, stderr)
		}
		if !strings.Contains(stdout, "--fail-on-pending") {
			t.Errorf("the report does not say why the run failed:\n%s", stdout)
		}
		if got := captured(t, repoRoot); len(got) != 0 {
			t.Errorf("a dry run wrote %v", got)
		}
	})

	t.Run("a clean dry run exits 0", func(t *testing.T) {
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "orphan", "only here\n")
		if code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--apply")...); code != exitOK {
			t.Fatalf("apply exit = %d\n%s", code, stdout)
		}

		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--fail-on-pending")...)
		if code != exitOK {
			t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
		}
	})

	t.Run("a run that captured everything it planned exits 0", func(t *testing.T) {
		// The flag has to be safe to leave on in a pipeline that also
		// writes. Firing on writes that succeeded would make --apply
		// --fail-on-pending permanently red.
		base, repoRoot := importFixture(t)
		projectAgent(t, base, "proj", "orphan", "only here\n")

		code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply", "--fail-on-pending")...)
		if code != exitOK {
			t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
		}
		if got := readFileT(t, backupPath(t, repoRoot, "proj", "orphan")); got != "only here\n" {
			t.Errorf("capture = %q", got)
		}
	})
}

// TestRunImport_ConflictDoesNotBlockTheOtherProjects: a conflict is one file's
// problem. The rest are independent files in independent projects and are
// still captured, while the exit code still reports the conflict.
func TestRunImport_ConflictDoesNotBlockTheOtherProjects(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "blocked", "researcher", "current\n")
	mustWriteT(t, backupPath(t, repoRoot, "blocked", "researcher"), "older capture\n")
	projectAgent(t, base, "fine", "orphan", "only here\n")

	code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitConflict {
		t.Errorf("exit = %d, want %d\n%s", code, exitConflict, stdout)
	}
	if got := readFileT(t, backupPath(t, repoRoot, "fine", "orphan")); got != "only here\n" {
		t.Errorf("the unaffected project was not captured: %q", got)
	}
	if got := readFileT(t, backupPath(t, repoRoot, "blocked", "researcher")); got != "older capture\n" {
		t.Errorf("the conflicting capture was overwritten: %q", got)
	}
}

// --- an incomplete scan -----------------------------------------------------

// TestRunImport_UnreadableSubtreeIsNotACleanRun is the difference between what
// scan asserts and what import asserts.
//
// scan exiting 0 asserts nothing — it is a report, and --fail-on-risk exists
// for a caller who wants an assertion out of it. import exiting 0 IS the
// assertion, that everything unprotected is now captured, and it is the
// precondition link gets gated on. A subtree the walk could not enter may hold
// a state-6/7 file, so that assertion cannot be made: exit 1, the code for
// something only a human can resolve.
//
// The capturable files are still captured. An unreadable directory somewhere
// else in the tree is not a reason to leave a genuinely unprotected file
// unprotected.
//
// The subtree is made unreadable structurally, not with chmod 000 (issue #11).
// The chmod form skipped on the CI gate, which runs as root, and it skipped
// without printing anything — so the assertion that mattered most was verified
// on one laptop and nowhere else. testfs.UnreadableSubtree does not care what
// uid is asking.
func TestRunImport_UnreadableSubtreeIsNotACleanRun(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "readable", "orphan", "only here\n")

	// A project whose agents the walk will never see, holding what may as
	// well be an unprotected override: the point is that nothing can tell.
	locked := filepath.Join(base, "locked")
	mustMkdirT(t, locked)
	unreadable, inside := testfs.UnreadableSubtree(t, locked)
	testfs.PlantAgent(t, inside, "proj", "hidden", "unreachable\n")

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitConflict {
		t.Errorf("exit = %d, want %d: an unreadable subtree cannot be a clean import\nstdout:\n%s\nstderr:\n%s",
			code, exitConflict, stdout, stderr)
	}

	if got := readFileT(t, backupPath(t, repoRoot, "readable", "orphan")); got != "only here\n" {
		t.Errorf("the readable project was not captured: %q", got)
	}

	// The headline must not read as an unqualified success while paths went
	// unread, and the unread path itself has to be named.
	if !strings.Contains(stdout, "Imported 1 of 1 file") {
		t.Errorf("the summary does not report the capture it did make:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NOT proof that everything is captured") {
		t.Errorf("the summary claims a complete capture while a subtree went unread:\n%s", stdout)
	}
	if !strings.Contains(stdout, "could not be classified") {
		t.Errorf("the report does not account for the unclassifiable path:\n%s", stdout)
	}
	// The exact path, not merely the prefix. The walk fails on the deepest
	// directory of the fixture, and asserting on that exact path is what
	// catches a filesystem where the failure surfaces one level up instead
	// — DT_UNKNOWN from readdir (XFS with ftype=0, some NFS) makes Go fall
	// back to lstat on the over-long path, so the parent's ReadDir is what
	// fails. Still correct behaviour, still a reported problem, but a
	// different arm than this test believes it is exercising.
	if !strings.Contains(stdout, unreadable) {
		t.Errorf("the report does not name the unreadable subtree:\n%s", stdout)
	}
}

// TestRunImport_UnreadableSubtreeBlocksAnOtherwiseEmptyRun: the same rule when
// there was nothing to capture at all. "Nothing to import" would be the most
// reassuring possible sentence to print about a tree we could not read.
func TestRunImport_UnreadableSubtreeBlocksAnOtherwiseEmptyRun(t *testing.T) {
	base, repoRoot := importFixture(t)

	locked := filepath.Join(base, "locked")
	mustMkdirT(t, locked)
	_, inside := testfs.UnreadableSubtree(t, locked)
	testfs.PlantAgentsDir(t, inside, "proj")

	code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitConflict {
		t.Errorf("exit = %d, want %d\n%s", code, exitConflict, stdout)
	}
	if strings.Contains(stdout, "every project agent is already captured") {
		t.Errorf("the report claims the whole tree is captured after failing to read part of it:\n%s", stdout)
	}
	if got := captured(t, repoRoot); len(got) != 0 {
		t.Errorf("captured %v, want nothing", got)
	}
}

// --- the home guard ---------------------------------------------------------

// TestRunImport_WrongHomeWritesNothing is the write-path guard from issue #3,
// and the reason RequireHome exists.
//
// $HOME is a real, readable directory that is simply not the one these projects
// live under — the sudo case. Nothing on disk looks wrong; the run would
// succeed, and would write a second parallel backup tree under _outside-home/
// that a later correct-$HOME link would never see. So it must not run at all.
func TestRunImport_WrongHomeWritesNothing(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")

	wrongHome := t.TempDir()
	t.Setenv("HOME", wrongHome)

	code, stdout, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d: a wrong $HOME must stop a write path", code, exitFailure)
	}
	if !strings.Contains(stderr, "$HOME") || !strings.Contains(stderr, wrongHome) {
		t.Errorf("the refusal does not name $HOME and the value in use:\n%s", stderr)
	}
	if !strings.Contains(stderr, "refusing to write") {
		t.Errorf("the refusal does not say that nothing was written:\n%s", stderr)
	}
	if got := captured(t, repoRoot); len(got) != 0 {
		t.Fatalf("wrote %v despite a wrong $HOME; the backup namespace was split", got)
	}
	if stdout != "" {
		t.Errorf("a refused run must not emit a report:\n%s", stdout)
	}

	// The legitimate case the guard must not break: projects that really do
	// live outside $HOME, opted in explicitly.
	code, _, stderr = runCLI(t, importArgs(t, repoRoot, base, "--apply", "--allow-outside-home")...)
	if code != exitOK {
		t.Fatalf("opted-in exit = %d, want %d\nstderr:\n%s", code, exitOK, stderr)
	}
	got := captured(t, repoRoot)
	if len(got) != 1 || !strings.HasPrefix(got[0], "_outside-home/") {
		t.Errorf("captured %v, want one file under _outside-home/", got)
	}
}

// TestRunImport_NoHomeWritesNothingEvenWhenOptedIn: --allow-outside-home says
// "my projects live outside $HOME", which is not a claim that there is no
// $HOME. An absent home re-keys everything just as silently and is never a
// correct configuration, so the flag must not suppress it.
func TestRunImport_NoHomeWritesNothingEvenWhenOptedIn(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")
	t.Setenv("HOME", "")

	code, _, stderr := runCLI(t, importArgs(t, repoRoot, base, "--apply", "--allow-outside-home")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "$HOME") {
		t.Errorf("the refusal does not name $HOME:\n%s", stderr)
	}
	if got := captured(t, repoRoot); len(got) != 0 {
		t.Errorf("wrote %v with no $HOME at all", got)
	}
}

// --- tracked in its own repo ------------------------------------------------

func TestRunImport_TrackedInItsOwnRepoNeedsTheFlag(t *testing.T) {
	requireGit(t)

	base, repoRoot := importFixture(t)
	tracked := filepath.Join(base, "example")
	initRepo(t, tracked)
	projectAgent(t, base, "example", "planner", "committed in example\n")
	gitRun(t, tracked, "add", ".")
	gitRun(t, tracked, "commit", "-qm", "agents")

	code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--apply")...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if got := captured(t, repoRoot); len(got) != 0 {
		t.Errorf("captured %v; a file committed in its own repo is not at risk", got)
	}
	// Skipped, but never silently: the report has to say what it passed over
	// and how to change its mind.
	if !strings.Contains(stdout, "planner") || !strings.Contains(stdout, "--include-tracked") {
		t.Errorf("the report does not account for the skipped file:\n%s", stdout)
	}

	code, stdout, _ = runCLI(t, importArgs(t, repoRoot, base, "--apply", "--include-tracked")...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if got := readFileT(t, backupPath(t, repoRoot, "example", "planner")); got != "committed in example\n" {
		t.Errorf("captured content = %q", got)
	}
}

// --- JSON -------------------------------------------------------------------

func TestRunImport_JSONReportsTheWholePlan(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "aaa", "orphan", "only here\n")
	projectAgent(t, base, "bbb", "researcher", "current\n")
	mustWriteT(t, backupPath(t, repoRoot, "bbb", "researcher"), "older capture\n")

	code, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--json")...)
	if code != exitConflict {
		t.Errorf("exit = %d, want %d", code, exitConflict)
	}

	var rep struct {
		Apply bool `json:"apply"`
		Items []struct {
			Dest       string `json:"dest"`
			ActionName string `json:"action_name"`
			Entry      struct {
				Project string `json:"project"`
				Name    string `json:"name"`
			} `json:"entry"`
		} `json:"items"`
		Counts  map[string]int `json:"counts"`
		Written []string       `json:"written"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, stdout)
	}
	if rep.Apply {
		t.Error("apply = true for a dry run")
	}
	if len(rep.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(rep.Items))
	}
	if rep.Items[0].ActionName != "import" || rep.Items[1].ActionName != "conflict" {
		t.Errorf("actions = %q/%q, want import/conflict",
			rep.Items[0].ActionName, rep.Items[1].ActionName)
	}
	if rep.Items[0].Dest != backupPath(t, repoRoot, "aaa", "orphan") {
		t.Errorf("dest = %q, want the repo backup path", rep.Items[0].Dest)
	}
	if rep.Counts["import"] != 1 || rep.Counts["conflict"] != 1 {
		t.Errorf("counts = %v, want one import and one conflict", rep.Counts)
	}
	if len(rep.Written) != 0 {
		t.Errorf("written = %v for a dry run, want empty", rep.Written)
	}
}

// TestRunImport_DiagnosticsStayOffStdout keeps --json machine-parseable: the
// report is stdout, everything else is stderr.
func TestRunImport_DiagnosticsStayOffStdout(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")

	_, stdout, _ := runCLI(t, importArgs(t, repoRoot, base, "--json", "--apply")...)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, stdout)
	}
}

// --- flag surface -----------------------------------------------------------

func TestRunImport_BadFlagReportsOnceAndWritesNothing(t *testing.T) {
	base, repoRoot := importFixture(t)
	projectAgent(t, base, "proj", "orphan", "only here\n")

	code, _, stderr := runCLI(t, importArgs(t, repoRoot, base, "--nonexistent")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if n := strings.Count(stderr, "not defined"); n != 1 {
		t.Errorf("flag error reported %d times, want 1:\n%s", n, stderr)
	}
	if got := captured(t, repoRoot); len(got) != 0 {
		t.Errorf("wrote %v while failing to parse its flags", got)
	}
}

func TestRunImport_MissingRepoFails(t *testing.T) {
	base, _ := importFixture(t)
	code, _, stderr := runCLI(t, importArgs(t, filepath.Join(base, "no-such-repo"), base, "--apply")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "agentsync:") {
		t.Errorf("expected an error on stderr, got:\n%s", stderr)
	}
}

func TestUsage_DocumentsImport(t *testing.T) {
	_, stdout, _ := runCLI(t, "help")
	for _, want := range []string{"agentsync import", "--apply", "--update", "--include-tracked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("usage does not document %q:\n%s", want, stdout)
		}
	}
}
