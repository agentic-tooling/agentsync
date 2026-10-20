package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/reconcile"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

func mustResolveDir(t *testing.T, p string) string {
	t.Helper()
	resolved, err := resolveDir(p)
	if err != nil {
		t.Fatalf("resolving %s: %v", p, err)
	}
	return resolved
}

// --- the walk ---------------------------------------------------------------

// TestDetectProject_Markers covers the whole marker table in one place: which
// entry identifies a project, the order the three are tried in, and the two
// levels the walk refuses to treat as a project.
func TestDetectProject_Markers(t *testing.T) {
	home := t.TempDir()

	// One tree, laid out so every case below is a different corner of it.
	mustMkdirT(t, filepath.Join(home, "Web"))
	mustMkdirT(t, filepath.Join(home, "Web", "withagents", ".claude", "agents"))
	mustMkdirT(t, filepath.Join(home, "Web", "withclaude", ".claude"))
	mustMkdirT(t, filepath.Join(home, "Web", "withclaude", "src", "deep"))
	mustMkdirT(t, filepath.Join(home, "Web", "repo", ".git"))
	mustMkdirT(t, filepath.Join(home, "Web", "repo", "pkg"))
	// A .claude inside a git repository: the subdirectory wins, because every
	// marker is tried at a level before the walk ascends.
	mustMkdirT(t, filepath.Join(home, "Web", "repo", "pkg", "inner", ".claude"))
	// A worktree or submodule leaves a .git file rather than a directory.
	mustWriteT(t, filepath.Join(home, "Web", "worktree", ".git"), "gitdir: /elsewhere\n")
	// A regular file called .claude is not a marker, and must not make the
	// nested probe fail either.
	mustWriteT(t, filepath.Join(home, "Web", "notaproject", ".claude"), "not a directory\n")
	mustWriteT(t, filepath.Join(home, "Web", "filethenrepo", ".claude"), "not a directory\n")
	mustMkdirT(t, filepath.Join(home, "Web", "filethenrepo", ".git"))
	// The home directory itself has one, and is still never a candidate.
	mustMkdirT(t, filepath.Join(home, ".claude", "agents"))

	tests := []struct {
		name       string
		cwd        string
		wantDir    string
		wantMarker string
	}{
		{"agents directory", filepath.Join(home, "Web", "withagents"), filepath.Join(home, "Web", "withagents"), markerAgents},
		{"claude directory", filepath.Join(home, "Web", "withclaude"), filepath.Join(home, "Web", "withclaude"), markerClaude},
		{"git directory", filepath.Join(home, "Web", "repo"), filepath.Join(home, "Web", "repo"), markerGit},
		{"git file", filepath.Join(home, "Web", "worktree"), filepath.Join(home, "Web", "worktree"), markerGit},
		{"ascends from a subdirectory", filepath.Join(home, "Web", "withclaude", "src", "deep"), filepath.Join(home, "Web", "withclaude"), markerClaude},
		{"nested claude beats the enclosing repo", filepath.Join(home, "Web", "repo", "pkg", "inner"), filepath.Join(home, "Web", "repo", "pkg", "inner"), markerClaude},
		{"repo root when the subdirectory has nothing", filepath.Join(home, "Web", "repo", "pkg"), filepath.Join(home, "Web", "repo"), markerGit},
		{"a .claude file is not a marker", filepath.Join(home, "Web", "notaproject"), "", ""},
		{"a .claude file does not hide the git marker", filepath.Join(home, "Web", "filethenrepo"), filepath.Join(home, "Web", "filethenrepo"), markerGit},
		{"the default tree itself is not a project", filepath.Join(home, "Web"), "", ""},
		{"home is never a project", home, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, marker, err := detectProject(mustResolveDir(t, home), tc.cwd)
			if err != nil {
				t.Fatalf("detectProject: %v", err)
			}
			want := tc.wantDir
			if want != "" {
				want = mustResolveDir(t, want)
			}
			if dir != want || marker != tc.wantMarker {
				t.Errorf("detectProject = (%q, %q), want (%q, %q)", dir, marker, want, tc.wantMarker)
			}
		})
	}
}

// TestDetectProject_OutsideHomeStillWalks: the "never above $HOME" rule stops
// the walk escaping home, and has nothing to say about a tree that was never
// inside it. Someone standing in /opt/thing means /opt/thing.
func TestDetectProject_OutsideHomeStillWalks(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	mustMkdirT(t, filepath.Join(outside, "work", ".claude"))
	mustMkdirT(t, filepath.Join(outside, "work", "sub"))

	dir, marker, err := detectProject(mustResolveDir(t, home), filepath.Join(outside, "work", "sub"))
	if err != nil {
		t.Fatalf("detectProject: %v", err)
	}
	if want := mustResolveDir(t, filepath.Join(outside, "work")); dir != want || marker != markerClaude {
		t.Errorf("detectProject = (%q, %q), want (%q, %q)", dir, marker, want, markerClaude)
	}
}

// TestDetectProject_UnreadableDirectoryRefuses is the fail-closed rule.
//
// "Could not look" is not "nothing here". Reading it as a clean negative would
// ascend past the project and select whatever project encloses it.
func TestDetectProject_UnreadableDirectoryRefuses(t *testing.T) {
	home := t.TempDir()
	unreadable, _ := testfs.UnreadableSubtree(t, home)

	_, _, err := detectProject(mustResolveDir(t, home), unreadable)
	if err == nil {
		t.Fatal("a directory that cannot be read was treated as a clean negative")
	}
	if !strings.Contains(err.Error(), "could not") {
		t.Errorf("the refusal does not say the walk failed:\n%v", err)
	}
}

// TestDetectProject_UnresolvableCwdRefuses is the deleted-working-directory
// case, which reads as a clean walk rather than a failed one.
//
// os.Getwd can succeed with a path that is no longer there — macOS reports the
// stale path from still-referenced vnodes — and every stat beneath it then
// returns ENOENT, which is a real "nothing here". The walk would ascend through
// all of it to an enclosing project. resolveDir is where that has to be caught,
// because it is the last point at which the two are distinguishable.
func TestDetectProject_UnresolvableCwdRefuses(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(home, "Web", "gone", "sub")
	mustMkdirT(t, gone)
	if err := os.RemoveAll(filepath.Join(home, "Web", "gone")); err != nil {
		t.Fatalf("removing the fixture: %v", err)
	}

	_, _, err := detectProject(mustResolveDir(t, home), gone)
	if err == nil {
		t.Fatal("a working directory that no longer exists was walked as if it did")
	}
	if !strings.Contains(err.Error(), "could not") {
		t.Errorf("the refusal does not say the walk failed:\n%v", err)
	}
}

func TestResolveDir(t *testing.T) {
	dir := t.TempDir()
	if got := mustResolveDir(t, dir); !filepath.IsAbs(got) {
		t.Errorf("resolveDir(%q) = %q, want an absolute path", dir, got)
	}
	// Degrading here would read a deleted working directory as having no
	// markers and climb past it.
	if _, err := resolveDir(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a path that does not resolve came back without an error")
	}
}

// --- resolving the root ----------------------------------------------------

func TestResolveRoot(t *testing.T) {
	home := t.TempDir()
	wide := filepath.Join(home, "Web")
	mustMkdirT(t, wide)
	t.Chdir(wide)
	t.Setenv(envRoot, wide)

	t.Run("no project marker refuses rather than widening", func(t *testing.T) {
		got, err := resolveRoot(home, false)
		if err == nil {
			t.Fatalf("resolve = %+v, want a refusal: a markerless directory widened to the whole tree", got)
		}
		for _, w := range []string{"no project found", "--" + allFlag} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("refusal does not name %q:\n%v", w, err)
			}
		}
	})

	t.Run("--all is the whole default tree", func(t *testing.T) {
		got, err := resolveRoot(home, true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Path != wide || got.Origin != rootFromAll || got.Project != "" {
			t.Errorf("resolve = %+v, want %q from --all", got, wide)
		}
	})

	t.Run("no home refuses the walk but not --all", func(t *testing.T) {
		_, err := resolveRoot("", false)
		if err == nil || !strings.Contains(err.Error(), "no home directory") || !strings.Contains(err.Error(), "--"+allFlag) {
			t.Errorf("err = %v, want the no-home refusal naming --%s", err, allFlag)
		}
		if got, err := resolveRoot("", true); err != nil || got.Origin != rootFromAll {
			t.Errorf("resolve = %+v, %v; want --all to need no home", got, err)
		}
	})

	t.Run("no default tree refuses --all", func(t *testing.T) {
		t.Setenv(envRoot, "")
		_, err := resolveRoot(home, true)
		if err == nil || !strings.Contains(err.Error(), "$"+envRoot) || !strings.Contains(err.Error(), "--"+allFlag) {
			t.Errorf("err = %v, want a refusal naming --%s and $%s", err, allFlag, envRoot)
		}
	})
}

// TestResolveWide: inventory's resolver differs from resolveRoot in exactly one
// place, what the absence of --all means.
func TestResolveWide(t *testing.T) {
	home := t.TempDir()
	wide := filepath.Join(home, "Web")
	project := filepath.Join(home, "Web", "toolbox")
	mustMkdirT(t, filepath.Join(project, ".claude", "agents"))
	// Standing squarely inside a project, which is what the other resolver
	// would narrow to.
	t.Chdir(project)
	t.Setenv(envRoot, wide)

	t.Run("no flags is the whole tree, not the project underfoot", func(t *testing.T) {
		got, err := resolveWide(false)
		if err != nil {
			t.Fatalf("resolveWide: %v", err)
		}
		if got.Path != wide || got.Origin != rootFromWide {
			t.Errorf("resolveWide = %+v, want %q as the machine-wide default", got, wide)
		}
		if got.Project != "" {
			t.Errorf("implied project = %q, want none: this command selects no project", got.Project)
		}
		// The other resolver, same directory, proves the fixture really narrows.
		derived, err := resolveRoot(mustResolveDir(t, home), false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if derived.Origin != rootFromProject {
			t.Fatalf("the fixture does not narrow under the deriving resolver (%+v); this proves nothing", derived)
		}
	})

	t.Run("--all is accepted and means the default", func(t *testing.T) {
		got, err := resolveWide(true)
		if err != nil {
			t.Fatalf("resolveWide: %v", err)
		}
		if got.Path != wide || got.Origin != rootFromAll {
			t.Errorf("resolveWide = %+v, want %q from --all", got, wide)
		}
	})

	t.Run("no default tree refuses", func(t *testing.T) {
		t.Setenv(envRoot, "")
		if _, err := resolveWide(false); err == nil {
			t.Fatal("an unset default tree produced a root anyway")
		}
	})

	t.Run("relative default tree refuses", func(t *testing.T) {
		t.Setenv(envRoot, ".")
		if _, err := resolveWide(false); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("err = %v, want a refusal naming an absolute path", err)
		}
	})
}

// TestResolveRoot_DerivesAndImpliesTheProject: the derived root carries the
// project key with it, which is what lets link fill --project in rather than
// relax the rule that requires it.
func TestResolveRoot_DerivesAndImpliesTheProject(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "Web", "toolbox")
	mustMkdirT(t, filepath.Join(project, ".claude"))
	t.Chdir(project)

	got, err := resolveRoot(mustResolveDir(t, home), false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Origin != rootFromProject || got.Path != mustResolveDir(t, project) {
		t.Fatalf("resolve = %+v, want the project directory", got)
	}
	if got.Project != "Web/toolbox" {
		t.Errorf("implied project = %q, want %q", got.Project, "Web/toolbox")
	}
	if got.marker != markerClaude {
		t.Errorf("marker = %q, want %q", got.marker, markerClaude)
	}
}

func TestRegisterAll(t *testing.T) {
	for name, register := range map[string]func(*flag.FlagSet) *bool{"registerAll": registerAll, "registerWideAll": registerWideAll} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(&bytes.Buffer{})
		all := register(fs)
		if err := fs.Parse([]string{"--" + allFlag}); err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if !*all {
			t.Errorf("%s did not bind --%s", name, allFlag)
		}
	}
}

// --- what gets printed ------------------------------------------------------

func TestEmitRootChoice(t *testing.T) {
	home := "/home/u"
	tests := []struct {
		name    string
		choice  rootChoice
		want    []string
		notWant []string
	}{
		{
			name:    "derived names the marker and how to widen",
			choice:  rootChoice{Path: "/home/u/Web/toolbox", Origin: rootFromProject, marker: markerClaude},
			want:    []string{"Root: ~/Web/toolbox (from the current directory, which is in a project: .claude) — pass --all to widen."},
			notWant: []string{"--root"},
		},
		{
			name:   "all",
			choice: rootChoice{Path: "/home/u/Web", Origin: rootFromAll, from: "$AGENTSYNC_ROOT"},
			want:   []string{"Root: ~/Web (from --all: the whole default tree, $AGENTSYNC_ROOT)."},
		},
		{
			name:   "all names the config file it came from",
			choice: rootChoice{Path: "/home/u/Web", Origin: rootFromAll, from: "config file /c.toml"},
			want:   []string{"~/Web", "--all", "config file /c.toml"},
		},
		{
			name:   "wide default names the config file it came from",
			choice: rootChoice{Path: "/home/u/Web", Origin: rootFromWide, from: "config file /c.toml"},
			want:   []string{"~/Web", "config file /c.toml", "never narrows"},
		},
		{
			name:    "wide default says it never narrows",
			choice:  rootChoice{Path: "/home/u/Web", Origin: rootFromWide},
			want:    []string{"~/Web", "whole machine", "never narrows"},
			notWant: []string{"pass --root"},
		},
		{
			// The zero value named nothing, so it says nothing.
			name:    "unresolved",
			choice:  rootChoice{},
			notWant: []string{"Root:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			emitRootChoice(&buf, home, tc.choice)
			got := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("line is missing %q:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("line should not contain %q:\n%s", w, got)
				}
			}
			if n := strings.Count(got, "\n"); n > 1 {
				t.Errorf("expected at most one line, got %d:\n%s", n, got)
			}
		})
	}
}

func TestDisplayPath(t *testing.T) {
	tests := []struct{ home, path, want string }{
		{"/home/u", "/home/u/Web/x", "~/Web/x"},
		{"/home/u", "/home/u", "~"},
		{"/home/u", "/opt/x", "/opt/x"},
		{"", "/opt/x", "/opt/x"},
		{"/home/u", "relative/path", "relative/path"},
	}
	for _, tc := range tests {
		if got := displayPath(tc.home, tc.path); got != tc.want {
			t.Errorf("displayPath(%q, %q) = %q, want %q", tc.home, tc.path, got, tc.want)
		}
	}
}

func TestEmitRestartNotice(t *testing.T) {
	var changed, unchanged bytes.Buffer
	emitRestartNotice(&changed, true)
	emitRestartNotice(&unchanged, false)

	if !strings.Contains(changed.String(), "Restart") {
		t.Errorf("a run that changed a link said nothing:\n%s", changed.String())
	}
	if unchanged.Len() != 0 {
		t.Errorf("a run that changed no link still told the user to restart:\n%s", unchanged.String())
	}
}

func TestReportStream(t *testing.T) {
	var out, errBuf bytes.Buffer
	if got := reportStream(false, &out, &errBuf); got != &out {
		t.Error("the text form must write its asides beside the report on stdout")
	}
	if got := reportStream(true, &out, &errBuf); got != &errBuf {
		t.Error("a machine-readable stdout must stay clean; asides belong on stderr")
	}
}

// --- end to end -------------------------------------------------------------

// projectScopeFixture builds a home with an agent repo and two sibling projects
// under ~/Web, each holding one agent that exists nowhere else.
func projectScopeFixture(t *testing.T) (home, repoRoot string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)

	repoRoot = filepath.Join(home, "Web", "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustWriteT(t, filepath.Join(home, "Web", "alpha", ".claude", "agents", "a.md"), "a\n")
	mustWriteT(t, filepath.Join(home, "Web", "beta", ".claude", "agents", "b.md"), "b\n")
	// Resolved, so the root displays under ~ the way the resolved home does.
	t.Setenv(envRoot, filepath.Join(mustResolveDir(t, home), "Web"))
	t.Setenv(envRepo, repoRoot)
	return home, repoRoot
}

func TestRun_RepoIsRequired(t *testing.T) {
	t.Setenv(envRepo, "")
	for _, cmd := range []string{"scan", "import", "link", "doctor", "reconcile", "inventory"} {
		code, stdout, stderr := runCLI(t, cmd, "--"+allFlag)
		if code != exitFailure {
			t.Errorf("%s: exit = %d, want %d with no repo named", cmd, code, exitFailure)
		}
		if !strings.Contains(stderr, "$"+envRepo) || !strings.Contains(stderr, "agentsync init") {
			t.Errorf("%s: the refusal does not name $%s and agentsync init:\n%s", cmd, envRepo, stderr)
		}
		if stdout != "" {
			t.Errorf("%s: a refused run must not emit a report:\n%s", cmd, stdout)
		}
	}
}

func scanProjects(t *testing.T, stdout string) []string {
	t.Helper()
	var res scan.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, stdout)
	}
	seen := make(map[string]bool)
	var out []string
	for _, e := range res.Entries {
		if !seen[e.Project] {
			seen[e.Project] = true
			out = append(out, e.Project)
		}
	}
	sort.Strings(out)
	return out
}

// TestRunScan_RootFollowsTheWorkingDirectory is the complaint itself: standing
// in one project and getting a machine-wide report.
func TestRunScan_RootFollowsTheWorkingDirectory(t *testing.T) {
	home, _ := projectScopeFixture(t)

	t.Run("inside a project reports that project alone", func(t *testing.T) {
		t.Chdir(filepath.Join(home, "Web", "alpha"))
		code, stdout, stderr := runCLI(t, "scan", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		if got := scanProjects(t, stdout); len(got) != 1 || got[0] != "Web/alpha" {
			t.Errorf("projects = %v, want [Web/alpha] only", got)
		}
		if !strings.Contains(stderr, "Root:") || !strings.Contains(stderr, "current directory") {
			t.Errorf("the run did not say which root it chose or why:\n%s", stderr)
		}
	})

	// A new project has no marker yet; widening from it would scan every
	// project on the machine, so every deriving command refuses before scanning.
	t.Run("outside any project refuses rather than widening", func(t *testing.T) {
		t.Chdir(filepath.Join(home, "Web"))
		for _, args := range [][]string{
			{"scan"}, {"import"}, {"link", "--agent", "shared"}, {"doctor"}, {"reconcile"},
		} {
			code, stdout, stderr := runCLI(t, args...)
			if code != exitFailure {
				t.Errorf("%s: exit = %d, want %d\n%s\n%s", args[0], code, exitFailure, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("%s: a refused run must not emit a report:\n%s", args[0], stdout)
			}
			if !strings.Contains(stderr, "no project found") || !strings.Contains(stderr, "--"+allFlag) {
				t.Errorf("%s: the refusal does not say why or name --%s:\n%s", args[0], allFlag, stderr)
			}
		}
	})

	t.Run("--all from outside any project scans everything", func(t *testing.T) {
		t.Chdir(filepath.Join(home, "Web"))
		code, stdout, stderr := runCLI(t, "scan", "--json", "--"+allFlag)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		if got := scanProjects(t, stdout); len(got) != 2 {
			t.Errorf("projects = %v, want both", got)
		}
	})

	t.Run("--all widens from inside a project", func(t *testing.T) {
		t.Chdir(filepath.Join(home, "Web", "alpha"))
		code, stdout, stderr := runCLI(t, "scan", "--json", "--"+allFlag)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		if got := scanProjects(t, stdout); len(got) != 2 {
			t.Errorf("projects = %v, want both", got)
		}
	})
}

// TestRunScan_ProjectKeysDoNotDependOnTheRoot is the invariant the whole change
// rests on. ProjectKey is home-relative, so narrowing the root must not rename
// anything — a key printed by a narrow run has to be the key a wide run prints,
// or a capture would land under a different key depending on where the user was
// standing when they ran the command.
func TestRunScan_ProjectKeysDoNotDependOnTheRoot(t *testing.T) {
	home, _ := projectScopeFixture(t)
	project := filepath.Join(home, "Web", "alpha")

	t.Chdir(project)
	_, narrow, _ := runCLI(t, "scan", "--json")
	narrowKeys := scanProjects(t, narrow)

	t.Chdir(home)
	_, wide, _ := runCLI(t, "scan", "--json", "--"+allFlag)
	wideKeys := scanProjects(t, wide)

	if len(narrowKeys) != 1 || narrowKeys[0] != "Web/alpha" {
		t.Fatalf("narrow run keys = %v, want [Web/alpha]", narrowKeys)
	}
	found := false
	for _, k := range wideKeys {
		if k == narrowKeys[0] {
			found = true
		}
	}
	if !found {
		t.Errorf("the wide run keys %v do not contain the narrow run's key %q; the root renamed the project",
			wideKeys, narrowKeys[0])
	}
}

// TestRun_RepoAndRootFlagsAreGone: the project is where you stand and the repo
// is configured, so naming either per run is an unknown flag.
func TestRun_RepoAndRootFlagsAreGone(t *testing.T) {
	_, repoRoot := projectScopeFixture(t)

	for _, cmd := range []string{"scan", "import", "link", "doctor", "reconcile", "inventory", "optimize", "override"} {
		for _, f := range []string{"--repo", "--root"} {
			code, stdout, stderr := runCLI(t, cmd, f, repoRoot)
			if code != exitFailure {
				t.Errorf("%s %s: exit = %d, want %d\n%s", cmd, f, code, exitFailure, stderr)
			}
			if !strings.Contains(stderr, "flag provided but not defined: "+strings.TrimPrefix(f, "-")) {
				t.Errorf("%s %s: refusal does not name the flag:\n%s", cmd, f, stderr)
			}
			if stdout != "" {
				t.Errorf("%s %s: a refused run must not emit a report:\n%s", cmd, f, stdout)
			}
		}
	}
}

// TestRun_StrayArgumentRefused: every command but init takes flags only, and a
// path-shaped argument is someone trying to name a project, so it is told to cd.
func TestRun_StrayArgumentRefused(t *testing.T) {
	home, _ := projectScopeFixture(t)
	t.Chdir(filepath.Join(home, "Web"))
	const cdHint = "Run it from the project directory instead."

	for _, cmd := range []string{"scan", "import", "link", "doctor", "reconcile", "inventory", "optimize", "override"} {
		for _, tc := range []struct {
			arg    string
			wantCD bool
		}{
			{"someagent", false},
			{filepath.Join(home, "Web", "alpha"), true},
			{"./alpha", true},
			{"~/Web/alpha", true},
			{"..", true},
			{"alpha", true}, // an existing directory, relative to the cwd
		} {
			code, stdout, stderr := runCLI(t, cmd, tc.arg)
			if code != exitFailure {
				t.Errorf("%s %s: exit = %d, want %d", cmd, tc.arg, code, exitFailure)
			}
			if want := fmt.Sprintf("agentsync: unexpected argument %q; %s takes flags only.", tc.arg, cmd); !strings.HasPrefix(stderr, want) {
				t.Errorf("%s %s: stderr = %q, want it to start %q", cmd, tc.arg, stderr, want)
			}
			if got := strings.Contains(stderr, cdHint); got != tc.wantCD {
				t.Errorf("%s %s: cd hint = %v, want %v:\n%s", cmd, tc.arg, got, tc.wantCD, stderr)
			}
			if stdout != "" {
				t.Errorf("%s %s: a refused run must not emit a report:\n%s", cmd, tc.arg, stdout)
			}
		}
	}
}

// TestRun_StrayArgumentHintsAreExclusive: the selector hints fire for a
// <project>/<name> pair and never for a path, which gets the cd hint alone.
func TestRun_StrayArgumentHintsAreExclusive(t *testing.T) {
	projectScopeFixture(t)
	for _, tc := range []struct{ cmd, flag string }{
		{"link", "--" + linker.UndeclineFlag + "="},
		{"import", "--update="},
		{"reconcile", "--" + reconcile.DropCaptureFlag + "="},
	} {
		_, _, stderr := runCLI(t, tc.cmd, "Web/proj/a")
		if !strings.Contains(stderr, tc.flag+"Web/proj/a") || strings.Contains(stderr, "project directory") {
			t.Errorf("%s with a selector: want the %s hint alone:\n%s", tc.cmd, tc.flag, stderr)
		}
		_, _, stderr = runCLI(t, tc.cmd, "/abs/path")
		if strings.Contains(stderr, tc.flag) || !strings.Contains(stderr, "project directory") {
			t.Errorf("%s with a path: want the cd hint alone:\n%s", tc.cmd, stderr)
		}
	}
}

// TestRunScan_PathArgumentDoesNotScanTheCwd: scan once ignored a stray argument,
// so naming another project from inside one silently scanned the wrong one.
func TestRunScan_PathArgumentDoesNotScanTheCwd(t *testing.T) {
	home, _ := projectScopeFixture(t)
	t.Chdir(filepath.Join(home, "Web", "alpha"))

	code, stdout, stderr := runCLI(t, "scan", "--fail-on-risk", filepath.Join(home, "Web", "beta"))
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	if stdout != "" || !strings.Contains(stderr, "Run it from the project directory instead.") {
		t.Errorf("want a refusal and no report:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// TestRun_RelativeRepoEnvRefused: a relative $AGENTSYNC_REPO names a different
// repo from every directory, so every command refuses it rather than guess.
func TestRun_RelativeRepoEnvRefused(t *testing.T) {
	home, _ := projectScopeFixture(t)
	t.Chdir(filepath.Join(home, "Web", "alpha"))
	t.Setenv(envRepo, "Web/agents-repo")

	for _, cmd := range []string{"scan", "import", "link", "doctor", "reconcile", "inventory", "override"} {
		code, stdout, stderr := runCLI(t, cmd)
		if code != exitFailure || !strings.Contains(stderr, "$"+envRepo+" must be an absolute path") {
			t.Errorf("%s: exit = %d, want %d naming $%s:\n%s", cmd, code, exitFailure, envRepo, stderr)
		}
		if stdout != "" {
			t.Errorf("%s: a refused run must not emit a report:\n%s", cmd, stdout)
		}
	}
}

// TestRunLink_DerivedRootImpliesProject is the ergonomic point of the change:
// `agentsync link --agent <name> --apply` becomes a complete command inside a
// project — and stays refused outside one, because --agent still requires a
// project and the wide root selects none.
func TestRunLink_DerivedRootImpliesProject(t *testing.T) {
	home, repoRoot := projectScopeFixture(t)
	target := globalAgent(t, repoRoot, "shared", "canonical\n")

	// The case that matters most: a project with .claude/ and no agents
	// directory at all, which is exactly where someone stands to provision a
	// first agent.
	project := filepath.Join(home, "Web", "fresh")
	mustMkdirT(t, filepath.Join(project, ".claude"))
	agentsDir := filepath.Join(project, ".claude", "agents")
	link := filepath.Join(agentsDir, "shared.md")

	t.Chdir(project)

	t.Run("the dry run creates nothing", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "link", "--agent", "shared")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		if !strings.Contains(stdout, "Web/fresh") {
			t.Errorf("the report does not name the implied project:\n%s", stdout)
		}
		if _, err := os.Lstat(agentsDir); err == nil {
			t.Error("a dry run created the agents directory")
		}
		if strings.Contains(stdout, restartNotice) {
			t.Error("a dry run told the user to restart their session")
		}
	})

	t.Run("apply creates the missing agents directory and the link", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "link", "--agent", "shared", "--apply")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		assertIsLink(t, link, target)

		fi, err := os.Stat(agentsDir)
		if err != nil {
			t.Fatalf("stat %s: %v", agentsDir, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o755 {
			t.Errorf("agents directory mode = %v, want 0755", perm)
		}

		// The notice, and last: a notice above a wall of per-file output is a
		// notice nobody reads.
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if last := lines[len(lines)-1]; last != restartNotice {
			t.Errorf("the restart notice is not the last line printed:\n%s", stdout)
		}
	})

	t.Run("a second apply changes nothing and says nothing about restarting", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "link", "--agent", "shared", "--apply")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		if strings.Contains(stdout, restartNotice) {
			t.Error("a no-op run told the user to restart their session")
		}
	})
}

// TestRunLink_WideRootStillRefusesAgentWithoutProject: filling --project in
// from a derived root must not have loosened the rule. --all selects no
// project, so naming an agent is refused exactly as before.
func TestRunLink_WideRootStillRefusesAgentWithoutProject(t *testing.T) {
	home, repoRoot := projectScopeFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical\n")
	t.Chdir(filepath.Join(home, "Web"))

	code, stdout, stderr := runCLI(t, "link", "--agent", "shared", "--apply", "--"+allFlag)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d: --agent with no project selected must be refused", code, exitFailure)
	}
	if !strings.Contains(stderr, "--"+linker.ProjectFlag) {
		t.Errorf("the refusal does not name the flag that is missing:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("a refused run must not emit a report:\n%s", stdout)
	}
}

// TestRunReconcile_RestartNoticeOnlyForLinkChanges: the notice fires on the one
// op that changes what a session would load, and on nothing else. A declined
// marker is a state write about a link somebody already removed — the session
// lost that agent when they removed it, not now.
func TestRunReconcile_RestartNoticeOnlyForLinkChanges(t *testing.T) {
	t.Run("a prune says restart, last", func(t *testing.T) {
		base, repoRoot := reconcileFixture(t)
		brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

		code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag, "--apply")...)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if last := lines[len(lines)-1]; last != restartNotice {
			t.Errorf("the restart notice is not the last line printed:\n%s", stdout)
		}
	})

	t.Run("a dry run does not", func(t *testing.T) {
		base, repoRoot := reconcileFixture(t)
		brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")

		_, stdout, _ := runCLI(t, reconcileArgs(t, repoRoot, base, "--"+reconcile.PruneFlag)...)
		if strings.Contains(stdout, restartNotice) {
			t.Errorf("a dry run told the user to restart their session:\n%s", stdout)
		}
	})

	t.Run("a declined marker does not", func(t *testing.T) {
		base, repoRoot := reconcileFixture(t)
		path, _ := brokenRecordedLink(t, base, repoRoot, "Web/proj", "a")
		// Somebody deleted the link themselves; the agents directory stays,
		// which is what makes this row three rather than a moved project.
		if err := os.Remove(path); err != nil {
			t.Fatalf("removing the link: %v", err)
		}

		code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, "--apply")...)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
		}
		if !strings.Contains(stdout, "decline") {
			t.Fatalf("the fixture did not produce a declined marker, so this proves nothing:\n%s", stdout)
		}
		if strings.Contains(stdout, restartNotice) {
			t.Errorf("a state-only write told the user to restart their session:\n%s", stdout)
		}
	})
}

// TestRunInventory_NeverNarrowsToTheProjectYouAreIn pins the one exemption.
//
// The reproduction is the obvious way to run this command: from inside the
// agent repo, which is itself a project. Deriving a root there would rebuild
// inventory.json to describe that one directory — and --check would tell the
// user to run exactly the command that overwrites the machine-wide file.
func TestRunInventory_NeverNarrowsToTheProjectYouAreIn(t *testing.T) {
	home, repoRoot := projectScopeFixture(t)
	// The repo is a project too, which is what makes running from inside it a
	// narrowing case rather than a hypothetical one.
	mustMkdirT(t, filepath.Join(repoRoot, ".claude", "agents"))

	t.Chdir(home)
	code, stdout, stderr := runCLI(t, "inventory")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
	}
	written := readFileT(t, inventoryPath(repoRoot))
	for _, key := range []string{"Web/alpha", "Web/beta"} {
		if !strings.Contains(written, key) {
			t.Fatalf("the machine-wide document is missing %q, so this proves nothing:\n%s", key, written)
		}
	}

	for _, dir := range []string{repoRoot, filepath.Join(home, "Web", "alpha")} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Chdir(dir)

			// The control: a deriving command in this very directory does
			// narrow, so the assertion below is about the exemption and not
			// about a walk that failed to find anything. Read off stderr,
			// which is where --json puts the root line.
			_, _, scanned := runCLI(t, "scan", "--json")
			if !strings.Contains(scanned, "current directory") {
				t.Fatalf("scan did not narrow here, so the fixture proves nothing:\n%s", scanned)
			}

			code, stdout, stderr := runCLI(t, "inventory", "--check")
			if code != exitOK {
				t.Errorf("exit = %d, want %d: --check narrowed and called the machine-wide file stale\n%s\n%s",
					code, exitOK, stdout, stderr)
			}
			if strings.Contains(stdout, "current directory") {
				t.Errorf("inventory derived a root from the working directory:\n%s", stdout)
			}
			if !strings.Contains(stdout, "whole machine") {
				t.Errorf("the root line does not say this command never narrows:\n%s", stdout)
			}
			if readFileT(t, inventoryPath(repoRoot)) != written {
				t.Error("--check rewrote the document")
			}
		})
	}
}

// TestRunReconcile_RekeyDoesNotAskForARestart: a re-keyed link is repointed at
// the same bytes under a new path, so the running session already has exactly
// the right agent loaded. Firing here would train the reader to skip the notice
// on the run that took an agent away.
func TestRunReconcile_RekeyDoesNotAskForARestart(t *testing.T) {
	base, repoRoot := reconcileFixture(t)

	oldCapture := filepath.Join(repoRoot, "project-agents", "Web", "old-name", "a.md")
	mustWriteT(t, oldCapture, "captured\n")
	path := projectAgentPath(base, "Web/new-name", "a")
	mustMkdirT(t, filepath.Dir(path))
	if err := os.Symlink(oldCapture, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	recordInState(t, repoRoot, "Web/new-name", "a", path, oldCapture)

	move := "--" + reconcile.RekeyFlag + "=Web/old-name=Web/new-name"
	code, stdout, stderr := runCLI(t, reconcileArgs(t, repoRoot, base, move, "--apply")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
	}
	newCapture := filepath.Join(repoRoot, "project-agents", "Web", "new-name", "a.md")
	assertIsLink(t, path, newCapture)

	if strings.Contains(stdout, restartNotice) {
		t.Errorf("a repointed link asked for a session restart:\n%s", stdout)
	}
}

// TestRun_RootDerivationRefusalsReachTheExitCode checks that resolve reports the
// walk's own error rather than "no project found", which also exits 2. Each case
// asserts the "could not" wording, since exit 2 alone cannot tell them apart.
func TestRun_RootDerivationRefusalsReachTheExitCode(t *testing.T) {
	// Two symlinks pointing at each other make os.Stat return ELOOP, which is
	// not fs.ErrNotExist, on every platform and as root, which chmod 000 is not.
	// The enclosing .git makes a swallowed error climb to a project and exit 0.
	t.Run("a marker that cannot be looked at", func(t *testing.T) {
		home, _ := projectScopeFixture(t)
		enclosing := filepath.Join(home, "Web", "outer")
		mustMkdirT(t, filepath.Join(enclosing, ".git"))
		dir := filepath.Join(enclosing, "loop")
		mustMkdirT(t, dir)
		loop, other := filepath.Join(dir, ".claude"), filepath.Join(dir, ".claude-other")
		if err := os.Symlink(other, loop); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := os.Symlink(loop, other); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := os.Stat(loop); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the fixture does not produce an unreadable marker (%v); this proves nothing", err)
		}

		t.Chdir(dir)
		code, stdout, stderr := runCLI(t, "scan")
		if code != exitFailure {
			t.Errorf("exit = %d, want %d: an unreadable marker must refuse, not climb to the enclosing project\n%s\n%s",
				code, exitFailure, stdout, stderr)
		}
		if strings.Contains(stdout, "Root:") {
			t.Errorf("a refused run named a root it never settled on:\n%s", stdout)
		}
		assertWalkRefusal(t, stderr)
	})

	// cd into a directory, delete it, and os.Getwd may still hand back the path.
	// Getwd failing and resolveDir failing on the stale path both say "could not".
	t.Run("a working directory that was deleted underneath us", func(t *testing.T) {
		home, _ := projectScopeFixture(t)
		gone := filepath.Join(home, "Web", "gone", "sub")
		mustMkdirT(t, gone)

		t.Chdir(gone)
		if err := os.RemoveAll(filepath.Join(home, "Web", "gone")); err != nil {
			t.Fatalf("removing the working directory: %v", err)
		}

		code, stdout, stderr := runCLI(t, "scan")
		if code != exitFailure {
			t.Errorf("exit = %d, want %d: a deleted working directory must refuse\n%s\n%s",
				code, exitFailure, stdout, stderr)
		}
		if strings.Contains(stdout, "Root:") {
			t.Errorf("a refused run named a root it never settled on:\n%s", stdout)
		}
		assertWalkRefusal(t, stderr)
	})
}

// TestRunLink_AgentRefusalNamesTheRoot: the refusal predates this branch and
// its wording is unchanged, but the reason it fired is now the root — and the
// remedy is to run the same command one directory down. An error that does not
// say so hides the ergonomic this whole change exists for.
func TestRunLink_AgentRefusalNamesTheRoot(t *testing.T) {
	home, repoRoot := projectScopeFixture(t)
	globalAgent(t, repoRoot, "shared", "canonical\n")
	t.Chdir(filepath.Join(home, "Web"))

	code, _, stderr := runCLI(t, "link", "--agent", "shared", "--apply", "--"+allFlag)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"~/Web", "selects no project", "from inside the project"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, stderr)
		}
	}

	// And it stays quiet when the root is not the reason: a bad agent name is
	// refused for its own reason and needs no advice about where to stand.
	_, _, stderr = runCLI(t, "link", "--project", "Web/alpha", "--agent", "../escape")
	if strings.Contains(stderr, "selects no project") {
		t.Errorf("the root hint fired on an unrelated refusal:\n%s", stderr)
	}
}

// assertWalkRefusal fails unless stderr carries the walk's own error, not the
// no-project refusal that shares its exit code.
func assertWalkRefusal(t *testing.T, stderr string) {
	t.Helper()
	if strings.Contains(stderr, "no project found") || !strings.Contains(stderr, "could not") {
		t.Errorf("the walk's error was replaced by a different refusal:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--"+allFlag) {
		t.Errorf("the refusal does not name a way out:\n%s", stderr)
	}
}
