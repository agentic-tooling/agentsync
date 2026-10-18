package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points HOME and XDG_CONFIG_HOME at empty temp dirs, so no test can
// read the developer's real config file and pass or fail because of it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentsync-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitFailure)
	}
	home := filepath.Join(dir, "home")
	for _, err := range []error{
		os.Mkdir(home, 0o755),
		os.Setenv("HOME", home),
		os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg")),
	} {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitFailure)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useConfig makes content this test's config file and clears both env vars,
// leaving the file as the only source. Empty content leaves no file at all.
func useConfig(t *testing.T, content string) string {
	t.Helper()
	t.Setenv(envRepo, "")
	t.Setenv(envRoot, "")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	path := filepath.Join(xdg, "agentsync", "config.toml")
	if content != "" {
		mustWriteT(t, path, content)
	}
	return path
}

func TestOpenRepo_Precedence(t *testing.T) {
	flagDir, envDir, fileDir := t.TempDir(), t.TempDir(), t.TempDir()
	tests := []struct {
		name, flag, env, file, want, wantFrom string
	}{
		{"file alone", "", "", fileDir, fileDir, "config file"},
		{"env alone", "", envDir, "", envDir, "$" + envRepo},
		{"flag alone", flagDir, "", "", flagDir, "--repo"},
		{"env beats file", "", envDir, fileDir, envDir, "$" + envRepo},
		{"flag beats env and file", flagDir, envDir, fileDir, flagDir, "--repo"},
		{"blank flag counts as unset", "  ", envDir, "", envDir, "$" + envRepo},
		{"blank env counts as unset", "", "  ", fileDir, fileDir, "config file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := ""
			if tc.file != "" {
				content = fmt.Sprintf("repo = %q\n", tc.file)
			}
			path := useConfig(t, content)
			t.Setenv(envRepo, tc.env)

			repo, from, err := openRepo(tc.flag)
			if err != nil {
				t.Fatalf("openRepo: %v", err)
			}
			if want := mustResolveDir(t, tc.want); repo.Root != want {
				t.Errorf("repo = %s, want %s", repo.Root, want)
			}
			wantFrom := tc.wantFrom
			if wantFrom == "config file" {
				wantFrom += " " + path
			}
			if from != wantFrom {
				t.Errorf("from = %q, want %q", from, wantFrom)
			}
		})
	}
}

// configFixture is projectScopeFixture with the default tree's resolved path
// returned and both env vars left for each test to set.
func configFixture(t *testing.T) (repoRoot, wide string) {
	t.Helper()
	home, repoRoot := projectScopeFixture(t)
	return repoRoot, filepath.Join(mustResolveDir(t, home), "Web")
}

func TestRun_ConfigFileSuppliesRepoAndRoot(t *testing.T) {
	repoRoot, wide := configFixture(t)

	t.Run("both from the file", func(t *testing.T) {
		path := useConfig(t, fmt.Sprintf("repo = %q\nroot = %q\n", repoRoot, wide))
		code, stdout, stderr := runCLI(t, "scan", "--all")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		for _, w := range []string{
			"Repo: ~/Web/agents-repo (from config file " + path + ").",
			"Root: ~/Web (from --all: the whole default tree, config file " + path + ").",
		} {
			if !strings.Contains(stdout, w) {
				t.Errorf("report is missing %q:\n%s", w, stdout)
			}
		}
	})

	t.Run("env repo, file root", func(t *testing.T) {
		path := useConfig(t, fmt.Sprintf("root = %q\n", wide))
		t.Setenv(envRepo, repoRoot)
		code, stdout, stderr := runCLI(t, "scan", "--all")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		for _, w := range []string{"(from $" + envRepo + ")", "config file " + path} {
			if !strings.Contains(stdout, w) {
				t.Errorf("report is missing %q:\n%s", w, stdout)
			}
		}
	})

	t.Run("file repo, env root", func(t *testing.T) {
		path := useConfig(t, fmt.Sprintf("repo = %q\n", repoRoot))
		t.Setenv(envRoot, wide)
		code, stdout, stderr := runCLI(t, "scan", "--all")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		for _, w := range []string{"(from config file " + path + ")", "the whole default tree, $" + envRoot} {
			if !strings.Contains(stdout, w) {
				t.Errorf("report is missing %q:\n%s", w, stdout)
			}
		}
	})

	t.Run("--json keeps both lines off stdout", func(t *testing.T) {
		useConfig(t, fmt.Sprintf("repo = %q\nroot = %q\n", repoRoot, wide))
		code, stdout, stderr := runCLI(t, "scan", "--all", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		var doc any
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Errorf("stdout is not JSON: %v\n%s", err, stdout)
		}
		for _, w := range []string{"Repo: ", "Root: "} {
			if !strings.Contains(stderr, w) {
				t.Errorf("stderr is missing %q:\n%s", w, stderr)
			}
		}
	})
}

// TestRunInventory_ConfigRootIsExplicit: a config root is the default tree,
// like $AGENTSYNC_ROOT, so inventory still ignores the project underfoot.
func TestRunInventory_ConfigRootIsExplicit(t *testing.T) {
	repoRoot, wide := configFixture(t)
	path := useConfig(t, fmt.Sprintf("repo = %q\nroot = %q\n", repoRoot, wide))
	t.Chdir(filepath.Join(wide, "alpha"))

	code, stdout, stderr := runCLI(t, "inventory", "--stdout")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	if w := "Root: ~/Web (the default tree, config file " + path; !strings.Contains(stderr, w) {
		t.Errorf("stderr is missing %q:\n%s", w, stderr)
	}
	if !strings.Contains(stdout, "Web/beta") {
		t.Errorf("inventory narrowed to the project underfoot; Web/beta is missing:\n%s", stdout)
	}
}

func TestRun_BrokenConfigRefuses(t *testing.T) {
	repoRoot, wide := configFixture(t)
	tests := []struct {
		name, content string
		setup         func(t *testing.T, path string)
		want          string
	}{
		{"malformed", "repo = \"/a\"\nroot = \n", nil, "line 2"},
		{"unknown key", "rpeo = \"/a\"\n", nil, `"rpeo"`},
		{"empty value", "repo = \"\"\n", nil, "is empty"},
		{"relative root", "root = \"Web\"\n", nil, "must be an absolute path"},
		{"is a directory", "", mustMkdirT, "is a directory"},
		{"dangling symlink", "", func(t *testing.T, path string) {
			mustMkdirT(t, filepath.Dir(path))
			if err := os.Symlink(path+".gone", path); err != nil {
				t.Fatal(err)
			}
		}, "dangling symlink"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := useConfig(t, tc.content)
			if tc.setup != nil {
				tc.setup(t, path)
			}
			for _, args := range [][]string{
				{"scan", "--root", wide},          // repo from the file
				{"inventory", "--repo", repoRoot}, // root from the file
			} {
				code, stdout, stderr := runCLI(t, args...)
				if code != exitFailure {
					t.Errorf("%v: exit = %d, want %d", args, code, exitFailure)
				}
				// The want is matched with path removed, since t.TempDir puts the
				// subtest name in it.
				if !strings.Contains(stderr, path) || !strings.Contains(strings.ReplaceAll(stderr, path, "<path>"), tc.want) {
					t.Errorf("%v: refusal does not name %s and say %q:\n%s", args, path, tc.want, stderr)
				}
				if stdout != "" {
					t.Errorf("%v: a refused run wrote a report:\n%s", args, stdout)
				}
			}
		})
	}
}

// TestRun_BrokenConfigUnreadWhenSettled: the file is read only for a value
// still unset, so it cannot block the run a user makes to work around it.
func TestRun_BrokenConfigUnreadWhenSettled(t *testing.T) {
	repoRoot, wide := configFixture(t)
	for _, tc := range []struct {
		name string
		env  bool
		args []string
	}{
		{"both flags", false, []string{"scan", "--repo", repoRoot, "--root", wide}},
		{"inventory, both flags", false, []string{"inventory", "--stdout", "--repo", repoRoot, "--root", wide}},
		{"both env vars", true, []string{"scan", "--all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useConfig(t, "this is = = not toml\n")
			if tc.env {
				t.Setenv(envRepo, repoRoot)
				t.Setenv(envRoot, wide)
			}
			if code, _, stderr := runCLI(t, tc.args...); code != exitOK {
				t.Errorf("exit = %d, want %d: the broken file was read\n%s", code, exitOK, stderr)
			}
		})
	}
}

func TestRun_NothingConfiguredRefuses(t *testing.T) {
	repoRoot, wide := configFixture(t)
	path := useConfig(t, "")

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"scan", "--root", wide},
			"agentsync: no canonical agent repo configured.\n" +
				"  Pass it each run: --repo PATH (or set $AGENTSYNC_REPO)\n" +
				"  Or set repo in " + path + "\n"},
		{[]string{"inventory", "--repo", repoRoot},
			"agentsync: no default tree of projects configured.\n" +
				"  Pass it each run: --root PATH (or set $AGENTSYNC_ROOT)\n" +
				"  Or set root in " + path + "\n"},
		{[]string{"scan", "--all", "--repo", repoRoot},
			"agentsync: --all: no default tree of projects configured.\n" +
				"  Pass it each run: --root PATH (or set $AGENTSYNC_ROOT)\n" +
				"  Or set root in " + path + "\n"},
	}
	for _, tc := range tests {
		code, stdout, stderr := runCLI(t, tc.args...)
		if code != exitFailure {
			t.Errorf("%v: exit = %d, want %d", tc.args, code, exitFailure)
		}
		if stderr != tc.want {
			t.Errorf("%v: stderr =\n%s\nwant\n%s", tc.args, stderr, tc.want)
		}
		if stdout != "" {
			t.Errorf("%v: a refused run wrote a report:\n%s", tc.args, stdout)
		}
	}
}

// TestOpenRepo_BadPathNamesItsSource: a repo that fails to open says which
// source supplied it, so a stale config entry is not mistaken for the env var.
func TestOpenRepo_BadPathNamesItsSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	path := useConfig(t, fmt.Sprintf("repo = %q\n", missing))
	_, _, err := openRepo("")
	if want := "repo from config file " + path + ": "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("err = %v, want it to start %q", err, want)
	}
}

// TestRun_UnlocatableConfigRefuses: with no usable $HOME or $XDG_CONFIG_HOME
// the file cannot be ruled out, so a run that needs it refuses.
func TestRun_UnlocatableConfigRefuses(t *testing.T) {
	useConfig(t, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	code, _, stderr := runCLI(t, "scan", "--root", t.TempDir())
	if code != exitFailure || !strings.Contains(stderr, "cannot locate the config file") {
		t.Errorf("exit = %d, stderr = %q; want %d naming the config file", code, stderr, exitFailure)
	}
}
