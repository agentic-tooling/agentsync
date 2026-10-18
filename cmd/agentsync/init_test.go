package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/config"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// initEnv gives the test its own HOME with two agent repos in it, puts the
// config file under that HOME, and makes init non-interactive.
func initEnv(t *testing.T) (home, cfgPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	agentRepo(t, filepath.Join(home, "Web", "claude-agents"), 3)
	agentRepo(t, filepath.Join(home, "old", "backup"), 1)
	useInput(t, "", false)
	return home, filepath.Join(home, ".config", "agentsync", "config.toml")
}

func useInput(t *testing.T, input string, tty bool) {
	t.Helper()
	old := promptInput
	promptInput = func() (io.Reader, bool) { return strings.NewReader(input), tty }
	t.Cleanup(func() { promptInput = old })
}

// agentRepo makes dir a git repo whose agents/ holds n agent files.
func agentRepo(t *testing.T, dir string, n int) {
	t.Helper()
	mustMkdirT(t, filepath.Join(dir, ".git"))
	mustMkdirT(t, filepath.Join(dir, "agents"))
	for i := range n {
		mustWriteT(t, filepath.Join(dir, "agents", string(rune('a'+i))+".md"), "---\nname: a\ndescription: d\n---\n")
	}
}

func assertNoConfig(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("config file %s exists (%v); want nothing written", path, err)
	}
}

func assertContains(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output is missing %q:\n%s", w, got)
		}
	}
}

const wantFile = "repo = \"~/Web/claude-agents\"\nroot = \"~/Web\"\n"

func TestRunInit_NonInteractiveListsCandidates(t *testing.T) {
	_, cfgPath := initEnv(t)

	code, stdout, stderr := runCLI(t, "init")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	assertContains(t, stdout,
		"Looking for agent repos under ~ (depth 4, at most 50000 directories)...\n"+
			"  - ~/Web/claude-agents  3 agents\n"+
			"  - ~/old/backup  1 agent\n",
		"\nNothing written. To use the first, run:\n  agentsync init --repo ~/Web/claude-agents --root ~/Web --apply\n")
	assertNoConfig(t, cfgPath)

	t.Run("a given root and --force go into the command", func(t *testing.T) {
		_, stdout, _ := runCLI(t, "init", "--root", "~/old", "--force")
		assertContains(t, stdout, "agentsync init --repo ~/Web/claude-agents --root ~/old --apply --force\n")
	})
}

func TestRunInit_NothingFound(t *testing.T) {
	_, cfgPath := initEnv(t)
	code, stdout, _ := runCLI(t, "init", "--search", t.TempDir())
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d", code, exitAtRisk)
	}
	assertContains(t, stdout, "No agent repos found.\n", "agentsync init --repo PATH --root PATH --apply")
	assertNoConfig(t, cfgPath)
}

func TestRunInit_SearchReportsUnreadable(t *testing.T) {
	initEnv(t)
	// Resolved first: the search walks the resolved root, so the fixture's
	// length must be measured against that path.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unreadable, _ := testfs.UnreadableSubtree(t, base)
	search := filepath.Dir(unreadable)
	agentRepo(t, filepath.Join(search, "r"), 1)

	code, stdout, stderr := runCLI(t, "init", "--search", search)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	assertContains(t, stdout,
		"  - "+filepath.Join(search, "r")+"  1 agent\n",
		"Could not read (anything under these was not searched):\n  "+unreadable+": ")
}

func TestRunInit_DryRunWritesNothing(t *testing.T) {
	home, cfgPath := initEnv(t)
	want := "Would write ~/.config/agentsync/config.toml:\n" +
		"  repo = \"~/Web/claude-agents\"\n  root = \"~/Web\"\n" +
		"Dry run: nothing written. To write it:\n" +
		"  agentsync init --repo ~/Web/claude-agents --root ~/Web --apply\n"
	for _, args := range [][]string{
		{"init", "--repo", filepath.Join(home, "Web", "claude-agents"), "--root", filepath.Join(home, "Web")},
		{"init", "--repo", "~/Web/claude-agents"}, // root defaults to the repo's parent
	} {
		code, stdout, stderr := runCLI(t, args...)
		if code != exitOK || stdout != want {
			t.Errorf("%v: exit = %d, stdout =\n%s\nwant %d and\n%s\nstderr: %s", args, code, stdout, exitOK, want, stderr)
		}
		assertNoConfig(t, cfgPath)
	}
}

func TestRunInit_ApplyWritesTheFile(t *testing.T) {
	home, cfgPath := initEnv(t)
	code, stdout, stderr := runCLI(t, "init", "--repo", "~/Web/claude-agents", "--root", "~/Web", "--apply")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	want := "Wrote ~/.config/agentsync/config.toml\nNext, inside a project:\n" +
		"  agentsync scan --at-risk\n  agentsync link --agent <name> --apply\n"
	if stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil || string(data) != wantFile {
		t.Errorf("file = %q, %v; want %q", data, err, wantFile)
	}
	if fi, err := os.Stat(cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", fi.Mode(), err)
	}
	got, err := config.Load(cfgPath)
	if c := (config.Config{Repo: filepath.Join(home, "Web", "claude-agents"), Root: filepath.Join(home, "Web")}); err != nil || got != c {
		t.Errorf("Load = %+v, %v; want %+v", got, err, c)
	}
}

func TestRunInit_ApplyNeedsBothValues(t *testing.T) {
	_, cfgPath := initEnv(t)
	for _, args := range [][]string{
		{"init", "--apply"},
		{"init", "--apply", "--repo", "~/Web/claude-agents"},
		{"init", "--apply", "--root", "~/Web"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != exitFailure || !strings.Contains(stderr, "needs both --repo and --root") {
			t.Errorf("%v: exit = %d, stderr = %q; want %d", args, code, stderr, exitFailure)
		}
		assertNoConfig(t, cfgPath)
	}
}

func TestRunInit_ExistingFile(t *testing.T) {
	apply := []string{"init", "--repo", "~/Web/claude-agents", "--root", "~/Web", "--apply"}

	t.Run("refused with its contents shown, interactive too", func(t *testing.T) {
		_, cfgPath := initEnv(t)
		mustWriteT(t, cfgPath, "repo = \"/old\"\n")
		for _, run := range []struct {
			args []string
			tty  bool
		}{{apply, false}, {apply[:len(apply)-1], false}, {[]string{"init"}, false}, {[]string{"init"}, true}} {
			useInput(t, "1\n\ny\n", run.tty)
			code, stdout, stderr := runCLI(t, run.args...)
			if code != exitAtRisk {
				t.Errorf("%v tty=%v: exit = %d, want %d", run.args, run.tty, code, exitAtRisk)
			}
			assertContains(t, stderr, "config file ~/.config/agentsync/config.toml already exists; pass --force to replace it.\n  repo = \"/old\"\n")
			if stdout != "" {
				t.Errorf("%v tty=%v: printed before refusing:\n%s", run.args, run.tty, stdout)
			}
		}
		if data, _ := os.ReadFile(cfgPath); string(data) != "repo = \"/old\"\n" {
			t.Errorf("file = %q, want it untouched", data)
		}
	})

	t.Run("replaced with --force", func(t *testing.T) {
		_, cfgPath := initEnv(t)
		mustWriteT(t, cfgPath, "repo = \"/old\"\n")
		if code, _, stderr := runCLI(t, append(apply, "--force")...); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
		}
		if data, _ := os.ReadFile(cfgPath); string(data) != wantFile {
			t.Errorf("file = %q, want %q", data, wantFile)
		}
	})

	t.Run("a dangling symlink is still a file in the way", func(t *testing.T) {
		_, cfgPath := initEnv(t)
		mustMkdirT(t, filepath.Dir(cfgPath))
		if err := os.Symlink(cfgPath+".gone", cfgPath); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runCLI(t, apply...)
		if code != exitAtRisk || !strings.Contains(stderr, "(could not read it: ") {
			t.Errorf("exit = %d, stderr = %q; want %d saying it could not be read", code, stderr, exitAtRisk)
		}
	})

	t.Run("a directory in the way fails the forced write", func(t *testing.T) {
		_, cfgPath := initEnv(t)
		mustWriteT(t, filepath.Join(cfgPath, "keep"), "x")
		code, _, stderr := runCLI(t, append(apply, "--force")...)
		if code != exitFailure || !strings.Contains(stderr, "writing config file "+cfgPath) {
			t.Errorf("exit = %d, stderr = %q; want %d", code, stderr, exitFailure)
		}
	})

	t.Run("a config path that cannot be checked refuses", func(t *testing.T) {
		initEnv(t)
		xdg := filepath.Join(t.TempDir(), "file")
		mustWriteT(t, xdg, "")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		code, _, stderr := runCLI(t, apply...)
		if code != exitFailure || !strings.Contains(stderr, "not a directory") {
			t.Errorf("exit = %d, stderr = %q; want %d", code, stderr, exitFailure)
		}
	})
}

func TestRunInit_InvalidValuesRefuse(t *testing.T) {
	home, cfgPath := initEnv(t)
	file := filepath.Join(home, "file")
	mustWriteT(t, file, "")
	mustMkdirT(t, filepath.Join(home, "noagents"))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing repo", []string{"--repo", "~/nope"}, "repo: stat "},
		{"repo is a file", []string{"--repo", file}, "is not a directory"},
		{"repo without agents/", []string{"--repo", "~/noagents"}, "has no agents/ directory"},
		{"relative repo", []string{"--repo", "Web/claude-agents"}, "repo must be an absolute path"},
		{"missing root", []string{"--repo", "~/Web/claude-agents", "--root", "~/nope"}, "root: stat "},
		{"root is a file", []string{"--repo", "~/Web/claude-agents", "--root", file}, "is not a directory"},
		{"relative root", []string{"--root", "Web"}, "root must be an absolute path"},
		{"relative search", []string{"--search", "Web"}, "--search must be an absolute path"},
		{"missing repo with --apply", []string{"--repo", "~/nope", "--root", "~/Web", "--apply"}, "repo: stat "},
		{"positional argument", []string{"extra"}, "init takes no arguments"},
		{"unknown flag", []string{"--nope"}, "flag provided but not defined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append([]string{"init"}, tc.args...)...)
			if code != exitFailure || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit = %d, stderr = %q; want %d saying %q", code, stderr, exitFailure, tc.want)
			}
			if strings.Contains(stdout, "Would write") {
				t.Errorf("previewed a file it refused:\n%s", stdout)
			}
			assertNoConfig(t, cfgPath)
		})
	}
}

func TestRunInit_HelpAndUnlocatableConfig(t *testing.T) {
	initEnv(t)
	if code, _, _ := runCLI(t, "init", "-h"); code != exitOK {
		t.Errorf("-h: exit = %d, want %d", code, exitOK)
	}
	t.Setenv("HOME", "")
	if code, _, stderr := runCLI(t, "init"); code != exitFailure || !strings.Contains(stderr, "cannot locate the config file") {
		t.Errorf("no HOME: exit = %d, stderr = %q", code, stderr)
	}
	// With the config file located through XDG, the default search of ~ is
	// what cannot be resolved.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if code, _, stderr := runCLI(t, "init"); code != exitFailure || !strings.Contains(stderr, "--search: cannot expand ~") {
		t.Errorf("no HOME, XDG set: exit = %d, stderr = %q", code, stderr)
	}
}

func TestRunInit_Interactive(t *testing.T) {
	tests := []struct {
		name, input string
		wantFile    string
		wantOut     []string
	}{
		{"pick by number, accept the suggested root", "1\n\ny\n", wantFile, []string{
			"  1) ~/Web/claude-agents  3 agents\n  2) ~/old/backup  1 agent\n",
			"Repo [1-2, or a path; q to quit]: Default tree for --all and inventory [~/Web; q to quit]: ",
			"Will write ~/.config/agentsync/config.toml:\n  repo = \"~/Web/claude-agents\"\n  root = \"~/Web\"\nWrite it? [y/N]: Wrote ~/.config/agentsync/config.toml\n",
		}},
		{"pick by path, type a root", "~/old/backup\n~/old\nYES\n", "repo = \"~/old/backup\"\nroot = \"~/old\"\n", []string{
			"[~/old; q to quit]: ",
		}},
		{"bad answers re-ask", "9\n\nnope\n~/old\n2\nWeb\n~/nope\n\ny\n", "repo = \"~/old/backup\"\nroot = \"~/old\"\n", []string{
			"There is no candidate 9.\n",
			"repo must be an absolute path",
			"has no agents/ directory",
			"root must be an absolute path",
			"root: stat ",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgPath := initEnv(t)
			useInput(t, tc.input, true)
			code, stdout, stderr := runCLI(t, "init")
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
			}
			assertContains(t, stdout, tc.wantOut...)
			if data, err := os.ReadFile(cfgPath); err != nil || string(data) != tc.wantFile {
				t.Errorf("file = %q, %v; want %q", data, err, tc.wantFile)
			}
		})
	}
}

// boundedPrompts is init's stdout. It fails the test once init asks more
// questions than max, so a prompt loop that ignores end of input fails rather
// than hangs.
type boundedPrompts struct {
	t   *testing.T
	max int
	out strings.Builder
}

func (w *boundedPrompts) Write(p []byte) (int, error) {
	w.out.Write(p)
	if n := strings.Count(w.out.String(), "]: "); n > w.max {
		w.t.Fatalf("asked %d questions, want at most %d:\n%s", n, w.max, w.out.String())
	}
	return len(p), nil
}

func TestRunInit_InteractiveWritesNothingUnlessConfirmed(t *testing.T) {
	tests := []struct {
		name, input string
		asks        int
		previewed   bool
	}{
		{"q at repo", "q\n", 1, false},
		{"EOF at repo", "", 1, false},
		{"q at root", "1\nq\n", 2, false},
		{"EOF at root", "1\n", 2, false},
		{"q at confirm", "1\n\nq\n", 3, true},
		{"EOF at confirm", "1\n\n", 3, true},
		{"empty confirm", "1\n\n\n", 3, true},
		{"n at confirm", "1\n\nN\n", 3, true},
		{"anything but yes", "1\n\nsure\n", 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgPath := initEnv(t)
			useInput(t, tc.input, true)
			stdout := &boundedPrompts{t: t, max: tc.asks}
			var stderr strings.Builder
			code := run([]string{"init"}, stdout, &stderr)
			// A stop must end the run at once, not re-ask until input runs out.
			if n := strings.Count(stdout.out.String(), "]: "); n != tc.asks {
				t.Errorf("asked %d questions, want %d:\n%s", n, tc.asks, stdout.out.String())
			}
			if code != exitAtRisk || !strings.Contains(stderr.String(), "init aborted; nothing written.") {
				t.Errorf("exit = %d, stderr = %q; want %d and an abort", code, stderr.String(), exitAtRisk)
			}
			if got := strings.Contains(stdout.out.String(), "Will write"); got != tc.previewed {
				t.Errorf("previewed = %v, want %v:\n%s", got, tc.previewed, stdout.out.String())
			}
			assertNoConfig(t, cfgPath)
		})
	}
}

func TestRunInit_RootWithoutRepoNeverPrompts(t *testing.T) {
	_, cfgPath := initEnv(t)
	useInput(t, "", true)
	code, stdout, stderr := runCLI(t, "init", "--root", "~/Web")
	if code != exitOK || strings.Contains(stdout, "Repo [") {
		t.Fatalf("exit = %d, want %d and no prompt\n%s\n%s", code, exitOK, stdout, stderr)
	}
	assertContains(t, stdout, "  - ~/Web/claude-agents  3 agents\n",
		"agentsync init --repo ~/Web/claude-agents --root ~/Web --apply\n")
	assertNoConfig(t, cfgPath)
}

func TestRunInit_InteractiveOneCandidate(t *testing.T) {
	_, cfgPath := initEnv(t)
	useInput(t, "1\n\ny\n", true)
	code, stdout, stderr := runCLI(t, "init", "--search", "~/Web")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	assertContains(t, stdout, "  1) ~/Web/claude-agents  3 agents\nRepo [1, or a path; q to quit]: ")
	if data, _ := os.ReadFile(cfgPath); string(data) != wantFile {
		t.Errorf("file = %q, want %q", data, wantFile)
	}
}

func TestRunInit_HomeThroughASymlink(t *testing.T) {
	target := t.TempDir()
	agentRepo(t, filepath.Join(target, "Web", "claude-agents"), 3)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	useInput(t, "", false)

	code, stdout, stderr := runCLI(t, "init")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s\n%s", code, exitOK, stdout, stderr)
	}
	assertContains(t, stdout, "  - ~/Web/claude-agents  3 agents\n",
		"agentsync init --repo ~/Web/claude-agents --root ~/Web --apply\n")
}

func TestRunInit_SearchRootThatIsNotADirectory(t *testing.T) {
	home, cfgPath := initEnv(t)
	mustWriteT(t, filepath.Join(home, "file"), "")
	if err := os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, "dangling")); err != nil {
		t.Fatal(err)
	}
	for search, want := range map[string]string{
		"~/file":     "  ~/file: not a directory\n",
		"~/dangling": "  ~/dangling: lstat ",
	} {
		code, stdout, _ := runCLI(t, "init", "--search", search)
		if code != exitAtRisk {
			t.Errorf("--search %s: exit = %d, want %d", search, code, exitAtRisk)
		}
		assertContains(t, stdout, "Could not read (anything under these was not searched):\n"+want)
	}
	assertNoConfig(t, cfgPath)
}

func TestRunInit_InteractiveWithNothingFoundTakesAPath(t *testing.T) {
	home, cfgPath := initEnv(t)
	useInput(t, "~/Web/claude-agents\n\ny\n", true)
	code, stdout, stderr := runCLI(t, "init", "--search", filepath.Join(home, "old", "backup", "agents"))
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	assertContains(t, stdout, "No agent repos found.\nRepo [a path; q to quit]: ")
	if data, _ := os.ReadFile(cfgPath); string(data) != wantFile {
		t.Errorf("file = %q, want %q", data, wantFile)
	}
}

func TestShellPath(t *testing.T) {
	r := initRun{home: "/h"}
	for in, want := range map[string]string{
		"/h/Web/agents":   "~/Web/agents",
		"/h":              "~",
		"/h/My Projects":  "~/'My Projects'",
		"/opt/it's here":  `'/opt/it'\''s here'`,
		"/opt/a-b_c.d+@~": "/opt/a-b_c.d+@~",
	} {
		if got := r.shellPath(in); got != want {
			t.Errorf("shellPath(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestPromptInput_NeedsBothTerminals runs the real detection, with /dev/zero
// standing in for a terminal: a character device that is not the null device.
func TestPromptInput_NeedsBothTerminals(t *testing.T) {
	open := func(name string) *os.File {
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	null, zero := open(os.DevNull), open("/dev/zero")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	oldIn, oldOut := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	for _, tc := range []struct {
		name    string
		in, out *os.File
		want    bool
	}{
		{"both terminals", zero, zero, true},
		{"stdout a pipe", zero, w, false},
		{"stdin a pipe", r, zero, false},
		{"stdin the null device", null, zero, false},
		{"stdout the null device", zero, null, false},
	} {
		os.Stdin, os.Stdout = tc.in, tc.out
		in, tty := promptInput()
		os.Stdin, os.Stdout = oldIn, oldOut
		if tty != tc.want || in != tc.in {
			t.Errorf("%s: prompting = %v, want %v (reader is stdin: %v)", tc.name, tty, tc.want, in == tc.in)
		}
	}
}
