package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// homeScanFixture builds a fake home, an agent repo inside it, and a project
// holding one file that exists nowhere else.
//
// The project lives under the fake home, so a run with HOME pointed at that
// home is the correct run and a run with HOME pointed anywhere else is the
// wrong one. Both are then reproducible without touching the real $HOME and
// without depending on /var/root existing or being readable in CI.
func homeScanFixture(t *testing.T) (home, repoRoot, root string) {
	t.Helper()

	home = t.TempDir()
	repoRoot = filepath.Join(home, "Web", "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))

	root = filepath.Join(home, "Web")
	mustWriteT(t, filepath.Join(root, "proj", ".claude", "agents", "orphan.md"), "only here\n")
	return home, repoRoot, root
}

func scanArgs(t *testing.T, repoRoot, root string, extra ...string) []string {
	t.Helper()
	inTree(t, repoRoot, root)
	return append([]string{"scan", "--" + allFlag}, extra...)
}

// TestRunScan_ValidButWrongHomeWarns requires a warning when $HOME is valid but
// is not the home these projects live under. A directory validity check alone
// would let the scan silently re-base every key under _outside-home/.
//
// The exit code must not change: warning is the right severity for a
// read-only command, and CI keys off these codes.
func TestRunScan_ValidButWrongHomeWarns(t *testing.T) {
	_, repoRoot, root := homeScanFixture(t)

	// A valid home that has nothing to do with the projects: the /var/root
	// case, without depending on /var/root.
	wrongHome := t.TempDir()
	t.Setenv("HOME", wrongHome)

	code, stdout, stderr := runCLI(t, scanArgs(t, repoRoot, root, "--json")...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d: a suspect $HOME must not change the exit code", code, exitOK)
	}

	// The warning has to be actionable without reading the source: name the
	// cause and print the value in use.
	if !strings.Contains(stderr, "$HOME") {
		t.Errorf("warning does not name $HOME as the cause:\n%s", stderr)
	}
	if !strings.Contains(stderr, wrongHome) {
		t.Errorf("warning does not print the $HOME in use (%q):\n%s", wrongHome, stderr)
	}
	if !strings.Contains(stderr, "--"+scan.AllowOutsideHomeFlag) {
		t.Errorf("warning does not offer the opt-out:\n%s", stderr)
	}

	// And the condition it warns about is real: the keys did get re-based.
	var res scan.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, stdout)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(res.Entries))
	}
	if !strings.HasPrefix(res.Entries[0].Project, "_outside-home/") {
		t.Errorf("project key = %q, want it re-based under _outside-home/ (the condition warned about)",
			res.Entries[0].Project)
	}

	// The scan still ran to completion and still found the file, and
	// --fail-on-risk still means what it meant.
	codeFail, _, _ := runCLI(t, scanArgs(t, repoRoot, root, "--fail-on-risk")...)
	if codeFail != exitAtRisk {
		t.Errorf("--fail-on-risk exit = %d, want %d: the unprotected file is still found",
			codeFail, exitAtRisk)
	}
}

// TestRunScan_WrongHomeAboveTheRootStillWarns is the end-to-end guard for the
// shape that a root-based check gets wrong.
//
// `sudo -i agentsync scan --all` with a default tree of / (or /Users) puts
// $HOME=/var/root UNDER the root being scanned. Anything that asks "is the
// root related to $HOME?" answers yes and stays silent — while every project
// key is still re-based under _outside-home/. The projects, not the root, decide.
func TestRunScan_WrongHomeAboveTheRootStillWarns(t *testing.T) {
	base := t.TempDir()

	// The wrong home lives under the root being scanned, as /var/root does
	// under /.
	wrongHome := filepath.Join(base, "wronghome")
	mustMkdirT(t, wrongHome)
	t.Setenv("HOME", wrongHome)

	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustWriteT(t, filepath.Join(base, "home", "Web", "proj", ".claude", "agents", "orphan.md"), "only here\n")

	code, stdout, stderr := runCLI(t, scanArgs(t, repoRoot, base, "--json")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr, "$HOME") || !strings.Contains(stderr, wrongHome) {
		t.Errorf("a wrong $HOME sitting under the scan root produced no usable warning:\n%s", stderr)
	}

	// The keys really were re-based, which is why the silence was wrong.
	var res scan.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, stdout)
	}
	if len(res.Entries) != 1 || !strings.HasPrefix(res.Entries[0].Project, "_outside-home/") {
		t.Errorf("entries = %+v, want one key under _outside-home/", res.Entries)
	}
}

// TestRunScan_MixedProjectsDoNotWarn is the companion wolf-guard. A correct
// machine scanned from a root above $HOME finds both home-relative projects
// and genuinely outside ones. Home is doing its job for at least one of them,
// so there is nothing to report.
func TestRunScan_MixedProjectsDoNotWarn(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	mustMkdirT(t, home)
	t.Setenv("HOME", home)

	repoRoot := filepath.Join(home, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustWriteT(t, filepath.Join(home, "Web", "proj", ".claude", "agents", "a.md"), "a\n")
	mustWriteT(t, filepath.Join(base, "opt", "work", ".claude", "agents", "b.md"), "b\n")

	code, stdout, stderr := runCLI(t, scanArgs(t, repoRoot, base)...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if stderr != "" {
		t.Errorf("a correct $HOME with some projects outside it must not warn:\n%s", stderr)
	}
	for _, name := range []string{"a", "b"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("report is missing %q:\n%s", name, stdout)
		}
	}
}

// TestRunScan_EmptyHomeWarnsAndCompletes covers the status the original,
// insufficient fix would have caught — it must still warn, and must still not
// abort the scan.
func TestRunScan_EmptyHomeWarnsAndCompletes(t *testing.T) {
	_, repoRoot, root := homeScanFixture(t)
	t.Setenv("HOME", "")

	code, stdout, stderr := runCLI(t, scanArgs(t, repoRoot, root)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr, "$HOME") {
		t.Errorf("empty $HOME produced no warning naming it:\n%s", stderr)
	}
	if !strings.Contains(stdout, "orphan") {
		t.Errorf("the scan did not complete; the report is missing the file:\n%s", stdout)
	}
}

// TestRunScan_UnresolvableHomeWarns covers a $HOME that names a directory
// which does not exist — valid syntactically, useless as a key base.
func TestRunScan_UnresolvableHomeWarns(t *testing.T) {
	_, repoRoot, root := homeScanFixture(t)
	gone := filepath.Join(t.TempDir(), "no-such-home")
	t.Setenv("HOME", gone)

	code, _, stderr := runCLI(t, scanArgs(t, repoRoot, root)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr, gone) {
		t.Errorf("warning does not print the unresolvable $HOME (%q):\n%s", gone, stderr)
	}
}

// TestRunScan_OutsideHomeOptOutIsSilent is the wolf-crying guard. Projects on
// /opt or an external volume produce all-_outside-home keys on a perfectly
// correct machine, every single run. If the flag did not silence it, the
// warning would be permanent noise for those users and would stop being read.
func TestRunScan_OutsideHomeOptOutIsSilent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A project tree deliberately outside the home, as on /opt.
	repoRoot := filepath.Join(home, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	outside := t.TempDir()
	mustWriteT(t, filepath.Join(outside, "work", ".claude", "agents", "orphan.md"), "only here\n")

	code, stdout, stderr := runCLI(t,
		scanArgs(t, repoRoot, outside, "--"+scan.AllowOutsideHomeFlag)...)
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if stderr != "" {
		t.Errorf("opted-out run wrote to stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "orphan") {
		t.Errorf("the scan did not report the file:\n%s", stdout)
	}

	// Without the flag the same run does warn — otherwise this test would
	// pass even if the check had been removed entirely.
	if _, _, stderr := runCLI(t, scanArgs(t, repoRoot, outside)...); stderr == "" {
		t.Error("without the opt-out the same run must warn")
	}
}

// withoutChoiceLines drops the Repo and Root lines, which every run prints and
// which --json sends to stderr to keep stdout parseable. They are not
// diagnostics, so a test asserting that a correct run raises none has to look
// past them — and only past them: everything else on stderr still fails.
func withoutChoiceLines(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" || strings.HasPrefix(line, "Root: ") || strings.HasPrefix(line, "Repo: ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestRunScan_CorrectHomeIsSilent is the other half of the wolf-crying guard:
// the normal case must raise nothing at all.
func TestRunScan_CorrectHomeIsSilent(t *testing.T) {
	home, repoRoot, root := homeScanFixture(t)
	t.Setenv("HOME", home)

	for _, extra := range [][]string{nil, {"--json"}, {"--at-risk"}, {"--fail-on-risk"}} {
		_, _, stderr := runCLI(t, scanArgs(t, repoRoot, root, extra...)...)
		if got := withoutChoiceLines(stderr); got != "" {
			t.Errorf("correct run with %v wrote to stderr:\n%s", extra, got)
		}
	}
}

// TestRunScan_HomeWarningStaysOffStdout guards the machine-readable contract:
// a diagnostic mixed into stdout would make --json unparseable, which is
// exactly how a warning intended to help turns into a broken pipeline.
func TestRunScan_HomeWarningStaysOffStdout(t *testing.T) {
	_, repoRoot, root := homeScanFixture(t)
	wrongHome := t.TempDir()
	t.Setenv("HOME", wrongHome)

	code, stdout, stderr := runCLI(t, scanArgs(t, repoRoot, root, "--json")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if stderr == "" {
		t.Fatal("expected a warning on stderr")
	}

	var res scan.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json stdout is not parseable with a warning present: %v\n%s", err, stdout)
	}
	for _, leak := range []string{"warning", "$HOME", wrongHome} {
		if strings.Contains(stdout, leak) {
			t.Errorf("%q leaked into stdout:\n%s", leak, stdout)
		}
	}
}

// TestWarnHome_ReportsWhetherItWarned exercises the helper directly, including
// the case where the repo itself is fine.
func TestWarnHome_ReportsWhetherItWarned(t *testing.T) {
	home := t.TempDir()
	repoRoot := filepath.Join(home, "agents-repo")
	mustMkdirT(t, repoRoot)

	repo, err := scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	repo.Home = home

	// Real directories: a scan only ever hands over paths it walked, and
	// ProjectKey resolves them, so a fixture of paths that do not exist
	// would not be keyed the way a real run keys them.
	inHome := filepath.Join(home, "Web", "proj", ".claude", "agents")
	outOfHome := filepath.Join(t.TempDir(), "work", ".claude", "agents")
	mustMkdirT(t, inHome)
	mustMkdirT(t, outOfHome)

	cases := []struct {
		name  string
		dirs  []string
		allow bool
		want  bool
	}{
		{name: "project under home", dirs: []string{inHome}, want: false},
		{name: "no projects at all", dirs: nil, want: false},
		{name: "every project outside home", dirs: []string{outOfHome}, want: true},
		{name: "every project outside home, opted out", dirs: []string{outOfHome}, allow: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			got := warnHome(&buf, repo, home, tc.dirs, tc.allow)
			if got != tc.want {
				t.Errorf("warnHome = %v, want %v", got, tc.want)
			}
			if (buf.Len() > 0) != tc.want {
				t.Errorf("wrote %q but reported %v", buf.String(), got)
			}
		})
	}
}
