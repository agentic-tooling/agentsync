package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func runHookWith(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	old := hookStdin
	hookStdin = strings.NewReader(stdin)
	t.Cleanup(func() { hookStdin = old })
	return runCLI(t, append([]string{"hook"}, args...)...)
}

func TestHook_RewritesMatchingAgent(t *testing.T) {
	code, stdout, stderr := runHookWith(t,
		`{"tool_name":"Agent","tool_input":{"subagent_type":"probe","prompt":"p","model":"haiku"}}`,
		"claude-agent", "--set", "probe=sonnet/high", "--set", "other=/low")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"effort":"high","model":"sonnet","prompt":"p","subagent_type":"probe"}}}` + "\n"
	if stdout != want {
		t.Errorf("stdout = %s, want %s", stdout, want)
	}

	code, stdout, _ = runHookWith(t, `{"tool_input":{"subagent_type":"bystander"}}`, "claude-agent", "--set", "probe=sonnet/high")
	if code != exitOK || stdout != "" {
		t.Errorf("no match: exit %d, stdout %q; want 0 and nothing", code, stdout)
	}
}

// Exit 2 from a PreToolUse hook blocks the Agent call, so every failure must
// be exit 1.
func TestHook_FailuresNeverExit2(t *testing.T) {
	tests := []struct {
		name, stdin string
		args        []string
	}{
		{"malformed stdin", "not json", []string{"claude-agent", "--set", "a=sonnet/"}},
		{"no tool_input", "{}", []string{"claude-agent", "--set", "a=sonnet/"}},
		{"bad --set", `{"tool_input":{}}`, []string{"claude-agent", "--set", "a=gpt/"}},
		{"unknown target", "{}", []string{"codex-agent"}},
		{"no target", "{}", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runHookWith(t, tc.stdin, tc.args...)
			if code == 2 || code != 1 || stdout != "" || stderr == "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1, nothing, a message", code, stdout, stderr)
			}
		})
	}

	t.Run("unreadable stdin", func(t *testing.T) {
		old := hookStdin
		hookStdin = iotest.ErrReader(errors.New("boom"))
		t.Cleanup(func() { hookStdin = old })
		if code := run([]string{"hook", "claude-agent"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 2 || code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
	})
	t.Run("stdout closed", func(t *testing.T) {
		old := hookStdin
		hookStdin = strings.NewReader(`{"tool_input":{"subagent_type":"a"}}`)
		t.Cleanup(func() { hookStdin = old })
		if code := run([]string{"hook", "claude-agent", "--set", "a=opus/"}, failWriter{}, &bytes.Buffer{}); code == 2 || code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
	})
}

// overrideFixture is a project with agents a and b, and its settings path.
func overrideFixture(t *testing.T) (project, repoRoot, settings string) {
	t.Helper()
	project, repoRoot = optimizeProject(t, map[string]string{
		"a.md": agentFile("a", "haiku", "low"),
		"b.md": agentFile("b", "haiku", "low"),
	})
	return project, repoRoot, filepath.Join(project, ".claude", "settings.local.json")
}

func overrideCLI(t *testing.T, project, repoRoot string, args ...string) (int, string, string) {
	t.Helper()
	return runCLI(t, append([]string{"override", "--root", project, "--repo", repoRoot}, args...)...)
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s should not exist (stat: %v)", path, err)
	}
}

func hookCommand(t *testing.T, path string) string {
	t.Helper()
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct{ Type, Command string }
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFileT(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	pre := doc.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "Agent" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" {
		t.Fatalf("PreToolUse = %+v", pre)
	}
	return pre[0].Hooks[0].Command
}

func TestOverride_SetApplyClear(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)

	code, stdout, stderr := overrideCLI(t, project, repoRoot, "--agent", "b", "--effort", "high")
	if code != exitOK || !strings.Contains(stdout, "after:  agentsync hook claude-agent --set b=/high || exit 1") || !strings.Contains(stdout, "Dry run") {
		t.Fatalf("dry run: exit %d\n%s%s", code, stdout, stderr)
	}
	assertAbsent(t, settings)

	for _, args := range [][]string{
		{"--agent", "b", "--effort", "high", "--apply"},
		{"--agent", "a", "--model", "sonnet", "--effort", "medium", "--apply"},
	} {
		if code, stdout, stderr := overrideCLI(t, project, repoRoot, args...); code != exitOK || !strings.Contains(stdout, "Wrote ") {
			t.Fatalf("%v: exit %d\n%s%s", args, code, stdout, stderr)
		}
	}
	if got, want := hookCommand(t, settings), "agentsync hook claude-agent --set a=sonnet/medium --set b=/high || exit 1"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	before := readFileT(t, settings)

	code, stdout, _ = overrideCLI(t, project, repoRoot, "--agent", "a", "--model", "sonnet", "--effort", "medium", "--apply")
	if code != exitOK || !strings.Contains(stdout, "already has that override") || readFileT(t, settings) != before {
		t.Fatalf("re-apply: exit %d\n%s", code, stdout)
	}

	code, stdout, _ = overrideCLI(t, project, repoRoot)
	if code != exitOK || !strings.Contains(stdout, "a                            sonnet / medium") || !strings.Contains(stdout, "b                            - / high") {
		t.Fatalf("list: exit %d\n%s", code, stdout)
	}

	if code, stdout, _ := overrideCLI(t, project, repoRoot, "--clear", "a", "--apply"); code != exitOK || !strings.Contains(stdout, "Wrote ") {
		t.Fatalf("clear a: exit %d\n%s", code, stdout)
	}
	if got, want := hookCommand(t, settings), "agentsync hook claude-agent --set b=/high || exit 1"; got != want {
		t.Fatalf("after clear: %q, want %q", got, want)
	}
	if code, stdout, _ := overrideCLI(t, project, repoRoot, "--clear", "a"); code != exitOK || !strings.Contains(stdout, "a has no override") {
		t.Fatalf("clear again: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = overrideCLI(t, project, repoRoot, "--clear", "b", "--apply")
	if code != exitOK || !strings.Contains(stdout, "after:  (no agentsync entry)") {
		t.Fatalf("clear b: exit %d\n%s", code, stdout)
	}
	assertAbsent(t, settings)

	if code, stdout, _ := overrideCLI(t, project, repoRoot); code != exitOK || !strings.Contains(stdout, "No overrides in ") {
		t.Fatalf("empty list: exit %d\n%s", code, stdout)
	}
}

func TestOverride_KeepsOtherSettings(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, settings, `{"permissions":{"allow":["Bash(ls)"]},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`)

	for _, args := range [][]string{{"--agent", "a", "--model", "opus", "--apply"}, {"--clear", "a", "--apply"}} {
		if code, stdout, stderr := overrideCLI(t, project, repoRoot, args...); code != exitOK {
			t.Fatalf("%v: exit %d\n%s%s", args, code, stdout, stderr)
		}
	}
	var got any
	if err := json.Unmarshal([]byte(readFileT(t, settings)), &got); err != nil {
		t.Fatal(err)
	}
	var want any
	_ = json.Unmarshal([]byte(`{"permissions":{"allow":["Bash(ls)"]},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`), &want)
	if !jsonEqual(got, want) {
		t.Fatalf("settings changed:\n%s", readFileT(t, settings))
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestOverride_RefusesWithoutWriting(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	tests := []struct {
		name string
		args []string
	}{
		{"unknown agent", []string{"--agent", "nope", "--model", "opus", "--apply"}},
		{"bad model", []string{"--agent", "a", "--model", "gpt", "--apply"}},
		{"full model id", []string{"--agent", "a", "--model", "claude-sonnet-5-5", "--apply"}},
		{"all", []string{"--all", "--agent", "a", "--model", "opus", "--apply"}},
		{"inherit", []string{"--agent", "a", "--model", "inherit", "--apply"}},
		{"bad effort", []string{"--agent", "a", "--effort", "huge", "--apply"}},
		{"nothing to set", []string{"--agent", "a", "--apply"}},
		{"model without agent", []string{"--model", "opus"}},
		{"clear with agent", []string{"--clear", "a", "--agent", "a"}},
		{"apply alone", []string{"--apply"}},
		{"positional", []string{"a"}},
		{"unknown flag", []string{"--bogus"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := overrideCLI(t, project, repoRoot, tc.args...)
			if code != exitFailure || stdout != "" || stderr == "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 2, nothing, a message", code, stdout, stderr)
			}
			assertAbsent(t, settings)
		})
	}

	t.Run("invalid JSON", func(t *testing.T) {
		mustWriteT(t, settings, "{not json")
		code, _, stderr := overrideCLI(t, project, repoRoot, "--agent", "a", "--model", "opus", "--apply")
		if code != exitFailure || !strings.Contains(stderr, "not a JSON object") || readFileT(t, settings) != "{not json" {
			t.Errorf("exit %d, stderr %q, file %q", code, stderr, readFileT(t, settings))
		}
	})

	t.Run("no repo", func(t *testing.T) {
		useConfig(t, "")
		if code, _, _ := runCLI(t, "override", "--root", project); code != exitFailure {
			t.Errorf("exit %d, want 2", code)
		}
	})

	t.Run("help", func(t *testing.T) {
		if code, _, _ := runCLI(t, "override", "-h"); code != exitOK {
			t.Errorf("exit %d, want 0", code)
		}
	})
}

const oneOverride = `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[{"type":"command","command":"agentsync hook claude-agent --set a=sonnet/ --set b=/max || exit 1"}]}]}}`

func TestScan_ShowsOverrides(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, settings, oneOverride)
	code, stdout, stderr := runCLI(t, scanArgs(repoRoot, project)...)
	if code != exitOK || !strings.Contains(stdout, "override: sonnet / -") || !strings.Contains(stdout, "override: - / max") {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}

	mustWriteT(t, settings, "{")
	code, stdout, stderr = runCLI(t, scanArgs(repoRoot, project)...)
	if code != exitOK || strings.Contains(stdout, "override:") || !strings.Contains(stderr, "warning: overrides not shown") {
		t.Fatalf("unreadable settings: exit %d\n%s%s", code, stdout, stderr)
	}
}

func TestOptimize_NotesProjectOverride(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, settings, oneOverride)
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}).start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--agent", "a", "--root", project, "--repo", repoRoot)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Note: Web/proj/a has a project override (sonnet / -) in ~/Web/proj/.claude/settings.local.json") {
		t.Errorf("no override note:\n%s", stdout)
	}
	if strings.Contains(stdout, "/b has a project override") {
		t.Errorf("note for an agent not asked about:\n%s", stdout)
	}
}

// pathWith makes PATH a directory holding only git and, unless script is "",
// an agentsync that runs it.
func pathWith(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if git, err := exec.LookPath("git"); err == nil {
		if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
			t.Fatal(err)
		}
	}
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "agentsync"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// servesHook succeeds only when run with oneOverride's stored arguments and a
// tool_input payload on stdin, so the probe's contract is what is tested.
const servesHook = `[ "$*" = "hook claude-agent --set a=sonnet/ --set b=/max" ] || exit 1
read -r line
case "$line" in *'"tool_input"'*) exit 0 ;; esac
exit 1`

func TestDoctor_OverrideNeedsAgentsyncOnPath(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, settings, oneOverride)

	pathWith(t, servesHook)
	if code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, project)...); code != exitOK || strings.Contains(stdout, "override-cannot-run") {
		t.Fatalf("hook-capable agentsync on PATH: exit %d\n%s%s", code, stdout, stderr)
	}

	for _, tc := range []struct{ name, script, detail string }{
		{"not on PATH", "", "which is not on PATH"},
		{"too old for the hook", `echo 'unknown command "hook"' >&2; exit 2`, "fails as the override hook (exit status 2)"},
		{"rejects the stored sets", `[ "$*" = "hook claude-agent" ] || exit 1`, "fails as the override hook (exit status 1)"},
	} {
		pathWith(t, tc.script)
		code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, project)...)
		if code != exitAtRisk {
			t.Fatalf("%s: exit %d, want 1\n%s%s", tc.name, code, stdout, stderr)
		}
		for _, w := range []string{"!! override-cannot-run (1)", "Web/proj", tc.detail, "1 project overrides agents through a hook that cannot run here"} {
			if !strings.Contains(stdout, w) {
				t.Errorf("%s: missing %q:\n%s", tc.name, w, stdout)
			}
		}
	}

	pathWith(t, servesHook)
	mustWriteT(t, settings, "{")
	code, stdout, _ := runCLI(t, doctorArgs(repoRoot, project)...)
	if code != exitFailure || !strings.Contains(stdout, "settings.local.json") {
		t.Fatalf("unreadable settings: exit %d, want 2\n%s", code, stdout)
	}

	mustRemoveAll(t, settings)
	if code, stdout, _ := runCLI(t, doctorArgs(repoRoot, project)...); code != exitOK {
		t.Fatalf("no overrides: exit %d, want 0\n%s", code, stdout)
	}
}

func TestOverride_RootThatCannotBeScanned(t *testing.T) {
	_, repoRoot, _ := overrideFixture(t)
	missing := filepath.Join(t.TempDir(), "gone")
	if code, _, stderr := overrideCLI(t, missing, repoRoot, "--agent", "a", "--model", "opus"); code != exitFailure || stderr == "" {
		t.Fatalf("exit %d, stderr %q; want 2", code, stderr)
	}
	assertAbsent(t, missing)
}

func TestOverride_MatchesFrontmatterName(t *testing.T) {
	project, repoRoot := optimizeProject(t, map[string]string{
		"reviewer.md": agentFile(`"code-reviewer" # the reviewer`, "haiku", "low"),
		"README.md":   "not an agent\n",
	})
	settings := filepath.Join(project, ".claude", "settings.local.json")
	for _, name := range []string{"reviewer", "README"} {
		if code, _, stderr := overrideCLI(t, project, repoRoot, "--agent", name, "--model", "opus", "--apply"); code != exitFailure || !strings.Contains(stderr, "frontmatter name") {
			t.Fatalf("--agent %s: exit %d, stderr %q; want 2", name, code, stderr)
		}
	}
	assertAbsent(t, settings)
	if code, stdout, stderr := overrideCLI(t, project, repoRoot, "--agent", "code-reviewer", "--model", "opus", "--apply"); code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if code, stdout, _ := runCLI(t, scanArgs(repoRoot, project)...); code != exitOK || !strings.Contains(stdout, "reviewer                      UNPROTECTED  override: opus / -") {
		t.Fatalf("scan: exit %d\n%s", code, stdout)
	}
}

func TestOverride_RefusesNestedProjectAgent(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, filepath.Join(project, "sub", ".claude", "agents", "n.md"), agentFile("n", "haiku", "low"))
	code, _, stderr := overrideCLI(t, project, repoRoot, "--agent", "n", "--model", "opus", "--apply")
	if code != exitFailure || !strings.Contains(stderr, "--agent n") {
		t.Fatalf("exit %d, stderr %q; want 2", code, stderr)
	}
	assertAbsent(t, settings)
	assertAbsent(t, filepath.Join(project, "sub", ".claude", "settings.local.json"))
}

func TestOverride_RefusesSymlinkedSettings(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "shared.json")
	mustWriteT(t, elsewhere, "{}")
	if err := os.Symlink(elsewhere, settings); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := overrideCLI(t, project, repoRoot, "--agent", "a", "--model", "opus", "--apply")
	if code != exitFailure || !strings.Contains(stderr, "symlink") || readFileT(t, elsewhere) != "{}" {
		t.Fatalf("exit %d, stderr %q, target %q", code, stderr, readFileT(t, elsewhere))
	}
}

func TestScan_OverrideStaysInItsProject(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	mustWriteT(t, settings, oneOverride)
	mustWriteT(t, filepath.Join(project, "sub", ".claude", "agents", "a.md"), agentFile("a", "haiku", "low"))
	code, stdout, _ := runCLI(t, scanArgs(repoRoot, project)...)
	if code != exitOK || strings.Count(stdout, "override: sonnet / -") != 1 {
		t.Fatalf("exit %d; want the override on the root project's a only\n%s", code, stdout)
	}
}

func TestSymlinkedSettings_ReadOnlyCommandsFollowIt(t *testing.T) {
	project, repoRoot, settings := overrideFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "shared.json")
	mustWriteT(t, elsewhere, `{"permissions":{"allow":[]}}`)
	if err := os.Symlink(elsewhere, settings); err != nil {
		t.Fatal(err)
	}
	pathWith(t, "")
	if code, stdout, stderr := runCLI(t, doctorArgs(repoRoot, project)...); code != exitOK || strings.Contains(stderr, "settings") {
		t.Fatalf("doctor: exit %d\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runCLI(t, scanArgs(repoRoot, project)...); code != exitOK || strings.Contains(stderr, "warning: overrides") {
		t.Fatalf("scan: exit %d\n%s%s", code, stdout, stderr)
	}

	mustWriteT(t, elsewhere, oneOverride)
	code, stdout, stderr := overrideCLI(t, project, repoRoot)
	if code != exitOK || !strings.Contains(stdout, "sonnet / -") {
		t.Fatalf("list: exit %d\n%s%s", code, stdout, stderr)
	}
}
