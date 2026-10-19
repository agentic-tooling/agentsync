package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// --- helpers ---------------------------------------------------------------

func entry(project, name string, st scan.State, tracked scan.Tracked) scan.Entry {
	return scan.Entry{
		Project: project,
		Name:    name,
		Path:    filepath.Join("/somewhere", project, ".claude", "agents", name+".md"),
		State:   st,
		Tracked: tracked,
	}
}

// textEntryNames extracts the agent names listed as entries in a text report.
//
// It deliberately skips the problems section, whose lines share the same
// indentation but are paths, not entries.
func textEntryNames(t *testing.T, out string) []string {
	t.Helper()
	var (
		names      []string
		inProblems bool
	)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "?? problems") {
			inProblems = true
			continue
		}
		if strings.TrimSpace(line) == "" {
			inProblems = false
			continue
		}
		if inProblems || !strings.HasPrefix(line, "     ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("unparseable entry line %q", line)
		}
		names = append(names, fields[1])
	}
	sort.Strings(names)
	return names
}

func jsonEntryNames(t *testing.T, out string) []string {
	t.Helper()
	var res scan.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal JSON report: %v\n%s", err, out)
	}
	names := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

func atRiskNames(res scan.Result) []string {
	risk := res.AtRisk()
	names := make([]string, 0, len(risk))
	for _, e := range risk {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

func renderText(t *testing.T, res scan.Result, atRiskOnly bool) string {
	t.Helper()
	var buf bytes.Buffer
	emitText(&buf, res, "/repo", "/root", atRiskOnly, nil)
	return buf.String()
}

func renderJSON(t *testing.T, res scan.Result, atRiskOnly bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := emitJSON(&buf, res, atRiskOnly); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}
	return buf.String()
}

func result(entries ...scan.Entry) scan.Result {
	return scan.Result{
		Entries:  entries,
		Dirs:     []string{"/root/proj/.claude/agents"},
		Problems: []scan.ScanProblem{},
	}
}

// --- BLOCKER 1: --at-risk must mean the same thing everywhere ---------------

// TestAtRisk_TextJSONAndExitCodeAgree is the regression guard for the bug that
// this package's zero coverage let through: emitText filtered by *state*
// (State.AtRisk — "the agent repo has no copy") while emitJSON filtered by
// *entry* (Entry.AtRisk — "nothing anywhere protects this file"). The same flag
// gave two different answers, and only the JSON one matched --fail-on-risk.
//
// A file that is an override but is committed in its own project is NOT at
// risk. It must be absent from both --at-risk views and must not trip
// --fail-on-risk.
func TestAtRisk_TextJSONAndExitCodeAgree(t *testing.T) {
	cases := []struct {
		name string
		res  scan.Result
		want []string
	}{
		{
			name: "override committed in its own repo is not at risk",
			res: result(
				entry("example", "planner", scan.StateOverride, scan.TrackedYes),
				entry("example", "writer", scan.StateOverride, scan.TrackedYes),
			),
			want: []string{},
		},
		{
			name: "unadopted but committed is not at risk",
			res: result(
				entry("example", "bespoke", scan.StateUnadopted, scan.TrackedYes),
			),
			want: []string{},
		},
		{
			name: "mixed group: only the uncommitted member is at risk",
			res: result(
				entry("example", "safe", scan.StateOverride, scan.TrackedYes),
				entry("example", "losable", scan.StateOverride, scan.TrackedNo),
			),
			want: []string{"losable"},
		},
		{
			name: "unknown tracking counts as unprotected",
			res: result(
				entry("proj", "mystery", scan.StateUnadopted, scan.TrackedUnknown),
			),
			want: []string{"mystery"},
		},
		{
			name: "states with a repo copy are never at risk however tracked",
			res: result(
				entry("proj", "linked", scan.StateHealthyLink, scan.TrackedNo),
				entry("proj", "same", scan.StateAdoptable, scan.TrackedNo),
				entry("proj", "broken", scan.StateBrokenLink, scan.TrackedNo),
				entry("proj", "theirs", scan.StateForeignLink, scan.TrackedNo),
			),
			want: []string{},
		},
		{
			name: "everything at once",
			res: result(
				entry("a", "linked", scan.StateHealthyLink, scan.TrackedNo),
				entry("a", "same", scan.StateAdoptable, scan.TrackedNo),
				entry("b", "safeoverride", scan.StateOverride, scan.TrackedYes),
				entry("b", "riskyoverride", scan.StateOverride, scan.TrackedNo),
				entry("c", "safeunadopted", scan.StateUnadopted, scan.TrackedYes),
				entry("c", "riskyunadopted", scan.StateUnadopted, scan.TrackedNo),
			),
			want: []string{"riskyoverride", "riskyunadopted"},
		},
		{
			name: "no entries at all",
			res:  result(),
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			sort.Strings(want)

			// The three views must be the same judgement.
			if got := atRiskNames(tc.res); !equalNames(got, want) {
				t.Fatalf("Result.AtRisk() = %v, want %v", got, want)
			}

			gotText := textEntryNames(t, renderText(t, tc.res, true))
			if !equalNames(gotText, want) {
				t.Errorf("--at-risk text listed %v, want %v", gotText, want)
			}

			gotJSON := jsonEntryNames(t, renderJSON(t, tc.res, true))
			if !equalNames(gotJSON, want) {
				t.Errorf("--at-risk --json listed %v, want %v", gotJSON, want)
			}

			if !equalNames(gotText, gotJSON) {
				t.Errorf("text and JSON disagree: text=%v json=%v", gotText, gotJSON)
			}

			// --fail-on-risk keys off the same set, so its verdict must
			// match whether either view listed anything.
			wantFail := len(want) > 0
			if got := len(tc.res.AtRisk()) > 0; got != wantFail {
				t.Errorf("--fail-on-risk would exit %v, but the reports listed %v", got, want)
			}
		})
	}
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEmitJSON_EmptyEntriesMarshalToArray keeps consumers from having to
// special-case null.
func TestEmitJSON_EmptyEntriesMarshalToArray(t *testing.T) {
	res := result(entry("p", "safe", scan.StateOverride, scan.TrackedYes))
	out := renderJSON(t, res, true)

	if strings.Contains(out, "null") {
		t.Errorf("JSON report contains null:\n%s", out)
	}
	if !strings.Contains(out, `"entries": []`) {
		t.Errorf("empty entries must marshal as [], got:\n%s", out)
	}
}

// TestEmitText_AtRiskOnlySaysSoWhenEmpty guards the wording: an --at-risk view
// with nothing to show must not read like "no agents exist".
func TestEmitText_AtRiskOnlySaysSoWhenEmpty(t *testing.T) {
	res := result(entry("p", "safe", scan.StateOverride, scan.TrackedYes))
	out := renderText(t, res, true)

	if !strings.Contains(out, "No at-risk agents found.") {
		t.Errorf("expected an explicit at-risk-empty message, got:\n%s", out)
	}
	if strings.Contains(out, "safe") {
		t.Errorf("a protected entry leaked into the --at-risk view:\n%s", out)
	}
	// The header still reports the full scan, so the reader knows the file
	// was seen and judged safe rather than missed.
	if !strings.Contains(out, "1 file") {
		t.Errorf("expected the full file count in the header, got:\n%s", out)
	}
}

// --- BLOCKER 2: problems must be visible in both formats -------------------

func TestEmitText_ProblemsAreReportedAndBlockTheAllClear(t *testing.T) {
	res := result(entry("p", "safe", scan.StateAdoptable, scan.TrackedNo))
	res.Problems = []scan.ScanProblem{
		{Path: "/root/p/.claude/agents/weird.md", Err: "is neither a regular file nor a symlink"},
	}

	out := renderText(t, res, false)

	if !strings.Contains(out, "?? problems (1)") {
		t.Errorf("problems section missing:\n%s", out)
	}
	if !strings.Contains(out, "weird.md") {
		t.Errorf("the problem path is not shown:\n%s", out)
	}
	if strings.Contains(out, "No at-risk files: every project agent is captured in the repo.") {
		t.Errorf("an incomplete scan must not print the unqualified all-clear:\n%s", out)
	}
	if !strings.Contains(out, "could not be checked") {
		t.Errorf("the verdict must qualify itself when paths were skipped:\n%s", out)
	}
}

func TestEmitText_ProblemsShownEvenWithAtRiskEntries(t *testing.T) {
	res := result(entry("p", "losable", scan.StateUnadopted, scan.TrackedNo))
	res.Problems = []scan.ScanProblem{{Path: "/root/x.md", Err: "symlink loop"}}

	out := renderText(t, res, true)
	if !strings.Contains(out, "?? problems (1)") {
		t.Errorf("problems must appear in the --at-risk view too:\n%s", out)
	}
	if !strings.Contains(out, "may hide more") {
		t.Errorf("the risk verdict must note the unchecked paths:\n%s", out)
	}
}

func TestEmitJSON_CarriesProblemsThroughTheAtRiskFilter(t *testing.T) {
	res := result(entry("p", "safe", scan.StateOverride, scan.TrackedYes))
	res.Problems = []scan.ScanProblem{{Path: "/root/x.md", Err: "symlink loop"}}

	var decoded scan.Result
	if err := json.Unmarshal([]byte(renderJSON(t, res, true)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Problems) != 1 || decoded.Problems[0].Path != "/root/x.md" {
		t.Errorf("problems = %v, want the one recorded problem", decoded.Problems)
	}
}

// --- report structure -------------------------------------------------------

// TestStateOrder_CoversEveryState makes sure a newly added state cannot fall
// out of the report silently.
func TestStateOrder_CoversEveryState(t *testing.T) {
	all := []scan.State{
		scan.StateHealthyLink,
		scan.StateBrokenLink,
		scan.StateForeignLink,
		scan.StateAdoptable,
		scan.StateOverride,
		scan.StateUnadopted,
	}
	if len(stateOrder) != len(all) {
		t.Fatalf("stateOrder has %d states, want %d", len(stateOrder), len(all))
	}

	inOrder := make(map[scan.State]bool, len(stateOrder))
	for _, st := range stateOrder {
		if inOrder[st] {
			t.Errorf("state %v appears twice in stateOrder", st)
		}
		inOrder[st] = true
		if stateNote[st] == "" {
			t.Errorf("state %v has no explanatory note", st)
		}
	}
	for _, st := range all {
		if !inOrder[st] {
			t.Errorf("state %v is missing from stateOrder and would never be printed", st)
		}
	}

	// The unprotected states must be reported before the benign ones.
	for i, st := range stateOrder {
		if !st.AtRisk() {
			continue
		}
		if i > 1 {
			t.Errorf("at-risk state %v is reported at position %d; urgent states must come first", st, i)
		}
	}
}

func TestEmitText_MarksUnprotectedEntriesIndividually(t *testing.T) {
	res := result(
		entry("p", "safe", scan.StateOverride, scan.TrackedYes),
		entry("p", "losable", scan.StateOverride, scan.TrackedNo),
	)
	out := renderText(t, res, false)

	if !strings.Contains(out, "!! override (2) — 1 unprotected") {
		t.Errorf("group header must break out the unprotected count:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, " losable"):
			if !strings.Contains(line, "UNPROTECTED") {
				t.Errorf("uncommitted override not marked UNPROTECTED: %q", line)
			}
		case strings.Contains(line, " safe"):
			if !strings.Contains(line, "committed in its own repo") {
				t.Errorf("committed override not marked as protected: %q", line)
			}
		}
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n              int
		one, many, out string
	}{
		{0, "", "s", "s"},
		{1, "", "s", ""},
		{2, "", "s", "s"},
		{1, "y", "ies", "y"},
		{3, "y", "ies", "ies"},
	}
	for _, tc := range cases {
		if got := plural(tc.n, tc.one, tc.many); got != tc.out {
			t.Errorf("plural(%d, %q, %q) = %q, want %q", tc.n, tc.one, tc.many, got, tc.out)
		}
	}
}

// --- run() / CLI surface ----------------------------------------------------

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestRun_UsageAndUnknownCommand(t *testing.T) {
	t.Run("no args prints usage on stdout", func(t *testing.T) {
		code, stdout, _ := runCLI(t)
		if code != exitOK {
			t.Errorf("exit = %d, want %d", code, exitOK)
		}
		if !strings.Contains(stdout, "agentsync scan") {
			t.Errorf("usage not printed:\n%s", stdout)
		}
	})

	t.Run("help prints usage", func(t *testing.T) {
		for _, arg := range []string{"help", "-h", "--help"} {
			code, stdout, _ := runCLI(t, arg)
			if code != exitOK {
				t.Errorf("%s: exit = %d, want %d", arg, code, exitOK)
			}
			if !strings.Contains(stdout, "agentsync scan") {
				t.Errorf("%s: usage not printed", arg)
			}
		}
	})

	t.Run("version reports the running build", func(t *testing.T) {
		for _, arg := range []string{"version", "-v", "--version"} {
			code, stdout, _ := runCLI(t, arg)
			if code != exitOK {
				t.Errorf("%s: exit = %d, want %d", arg, code, exitOK)
			}
			// Asserted by shape, not by value. The exact version, commit and
			// build time depend on how and where the test binary was built —
			// a tagged checkout, a dirty tree and a -buildvcs=false build all
			// produce different correct output — so pinning the string would
			// make this test a report of the build environment rather than of
			// the code. readBuildDetails is where the values are pinned, from
			// inputs the test controls.
			first, _, _ := strings.Cut(stdout, "\n")
			if !strings.HasPrefix(first, "agentsync ") || first == "agentsync " {
				t.Errorf("%s: first line = %q, want %q plus a non-empty version",
					arg, first, "agentsync ")
			}
			// The go line is the one field always available, since it comes
			// from the runtime rather than from anything the build recorded.
			if !strings.Contains(stdout, "\n  go        go") {
				t.Errorf("%s: stdout = %q, want a go line", arg, stdout)
			}
		}
	})

	t.Run("unknown command fails once", func(t *testing.T) {
		code, _, stderr := runCLI(t, "destroy")
		if code != exitFailure {
			t.Errorf("exit = %d, want %d", code, exitFailure)
		}
		if n := strings.Count(stderr, "unknown command"); n != 1 {
			t.Errorf("error reported %d times, want 1:\n%s", n, stderr)
		}
	})
}

// TestRunScan_BadFlagReportsOnce guards the nit where the flag package wrote
// the error and main wrapped and printed it again.
func TestRunScan_BadFlagReportsOnce(t *testing.T) {
	code, _, stderr := runCLI(t, "scan", "--nonexistent")
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if n := strings.Count(stderr, "not defined"); n != 1 {
		t.Errorf("flag error reported %d times, want exactly 1:\n%s", n, stderr)
	}
}

func TestRunScan_MissingRepoFails(t *testing.T) {
	code, _, stderr := runCLI(t, "scan",
		"--repo", filepath.Join(t.TempDir(), "no-such-repo"),
		"--root", t.TempDir())
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "agentsync:") {
		t.Errorf("expected an error on stderr, got:\n%s", stderr)
	}
}

// TestRunScan_EndToEnd_AtRiskConsistency reproduces the reported symptom on a
// real tree: a project whose overrides are committed in its own git repo, plus
// a project holding a genuinely unprotected file. Text, JSON, and the
// --fail-on-risk exit code must all tell the same story.
func TestRunScan_EndToEnd_AtRiskConsistency(t *testing.T) {
	requireGit(t)

	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustWriteT(t, filepath.Join(repoRoot, "agents", "planner.md"), "canonical planner\n")
	mustWriteT(t, filepath.Join(repoRoot, "agents", "writer.md"), "canonical writer\n")

	// example: overrides that ARE committed in the project's own repo.
	example := filepath.Join(base, "example")
	initRepo(t, example)
	mustWriteT(t, filepath.Join(example, ".claude", "agents", "planner.md"), "example planner\n")
	mustWriteT(t, filepath.Join(example, ".claude", "agents", "writer.md"), "example writer\n")
	gitRun(t, example, "add", ".")
	gitRun(t, example, "commit", "-qm", "agents")

	// loose: a file in no repository at all.
	mustWriteT(t, filepath.Join(base, "loose", ".claude", "agents", "orphan.md"), "nowhere else\n")

	commonArgs := []string{"scan", "--repo", repoRoot, "--root", base}

	codeText, stdoutText, _ := runCLI(t, append(append([]string{}, commonArgs...), "--at-risk")...)
	codeJSON, stdoutJSON, _ := runCLI(t, append(append([]string{}, commonArgs...), "--at-risk", "--json")...)
	if codeText != exitOK || codeJSON != exitOK {
		t.Fatalf("scan without --fail-on-risk must exit 0: text=%d json=%d", codeText, codeJSON)
	}

	gotText := textEntryNames(t, stdoutText)
	gotJSON := jsonEntryNames(t, stdoutJSON)
	want := []string{"orphan"}

	if !equalNames(gotText, want) {
		t.Errorf("--at-risk text listed %v, want %v\n%s", gotText, want, stdoutText)
	}
	if !equalNames(gotJSON, want) {
		t.Errorf("--at-risk --json listed %v, want %v\n%s", gotJSON, want, stdoutJSON)
	}
	if !equalNames(gotText, gotJSON) {
		t.Errorf("text and JSON disagree: text=%v json=%v", gotText, gotJSON)
	}

	// The committed overrides must appear nowhere in the --at-risk views.
	for _, name := range []string{"planner", "writer"} {
		if strings.Contains(stdoutText, " "+name+" ") {
			t.Errorf("committed override %q leaked into the --at-risk text view:\n%s", name, stdoutText)
		}
	}

	// And the exit code agrees: risk exists, so --fail-on-risk must be 1.
	codeFail, _, _ := runCLI(t, append(append([]string{}, commonArgs...), "--fail-on-risk")...)
	if codeFail != exitAtRisk {
		t.Errorf("--fail-on-risk exit = %d, want %d", codeFail, exitAtRisk)
	}

	// Remove the only unprotected file: everything else is committed, so the
	// exit code must flip to 0 and both views must be empty.
	if err := os.Remove(filepath.Join(base, "loose", ".claude", "agents", "orphan.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	codeFail, _, _ = runCLI(t, append(append([]string{}, commonArgs...), "--fail-on-risk")...)
	if codeFail != exitOK {
		t.Errorf("--fail-on-risk exit = %d with nothing at risk, want %d", codeFail, exitOK)
	}
	_, stdoutText, _ = runCLI(t, append(append([]string{}, commonArgs...), "--at-risk")...)
	_, stdoutJSON, _ = runCLI(t, append(append([]string{}, commonArgs...), "--at-risk", "--json")...)
	if got := textEntryNames(t, stdoutText); len(got) != 0 {
		t.Errorf("--at-risk text listed %v, want nothing\n%s", got, stdoutText)
	}
	if got := jsonEntryNames(t, stdoutJSON); len(got) != 0 {
		t.Errorf("--at-risk --json listed %v, want nothing\n%s", got, stdoutJSON)
	}
}

// TestRunScan_EndToEnd_ProblemDoesNotHideRisk is the CLI-level BLOCKER 2 guard.
func TestRunScan_EndToEnd_ProblemDoesNotHideRisk(t *testing.T) {
	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))

	// A project that sorts first and contains an unclassifiable entry.
	mustMkdirT(t, filepath.Join(base, "aaa", ".claude", "agents", "adirectory.md"))

	// A later project holding the file we must not miss.
	mustWriteT(t, filepath.Join(base, "zzz", ".claude", "agents", "precious.md"), "unique\n")

	code, stdout, stderr := runCLI(t, "scan", "--repo", repoRoot, "--root", base, "--fail-on-risk")
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d (an unprotected file exists)\nstdout:\n%s\nstderr:\n%s",
			code, exitAtRisk, stdout, stderr)
	}
	if !strings.Contains(stdout, "precious") {
		t.Errorf("the unprotected file in a later project was not reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "adirectory.md") {
		t.Errorf("the unclassifiable entry was not surfaced as a problem:\n%s", stdout)
	}
}

// --- local fs helpers (the scan package's own are unexported) --------------

func mustMkdirT(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteT(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirT(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requireGit skips the test when git is not usable, so the suite still runs in
// a container without it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig-system"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	mustMkdirT(t, dir)
	gitRun(t, dir, "init", "-q", "-b", "main")
}
