package override

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name, agent string
		v           Value
		wantErr     string
	}{
		{"alias and effort", "rev", Value{"sonnet", "medium"}, ""},
		{"model alone", "rev", Value{"fable", ""}, ""},
		{"effort alone", "rev", Value{"", "max"}, ""},
		{"nothing set", "rev", Value{}, "needs a model, an effort or both"},
		{"inherit", "rev", Value{"inherit", ""}, "use --clear"},
		{"unknown alias", "rev", Value{"gpt", ""}, `model "gpt" is not one of haiku, sonnet, opus, fable`},
		{"full model id", "rev", Value{"claude-sonnet-5-5", ""}, `model "claude-sonnet-5-5" is not one of`},
		{"unknown effort", "rev", Value{"", "huge"}, `effort "huge" is not one of low, medium, high, xhigh, max`},
		{"empty name", "", Value{"sonnet", ""}, "agent name"},
		{"name a shell would split", "a;rm", Value{"sonnet", ""}, "agent name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.agent, tc.v)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	got, err := ParseArgs([]string{"--set", "b=/high", "--set", "a=sonnet/medium"})
	want := map[string]Value{"a": {"sonnet", "medium"}, "b": {"", "high"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseArgs = %v, %v; want %v", got, err, want)
	}
	got, err = ParseArgs([]string{"--set", "a=sonnet/medium", "||", "exit", "1"})
	if err != nil || !reflect.DeepEqual(got, map[string]Value{"a": {"sonnet", "medium"}}) {
		t.Fatalf("ParseArgs with the guard = %v, %v", got, err)
	}
	if got, err := ParseArgs(nil); err != nil || len(got) != 0 {
		t.Fatalf("ParseArgs(nil) = %v, %v; want empty", got, err)
	}

	for _, args := range [][]string{
		{"--set"},
		{"--sett", "a=sonnet/"},
		{"--set", "a=sonnet"},
		{"--set", "sonnet/high"},
		{"--set", "a=sonnet/", "--set", "a=/high"},
		{"--set", "a=gpt/"},
		{"--set", "a=sonnet/", "||", "exit", "2"},
		{"--set", "a=sonnet/", "||", "true"},
	} {
		if got, err := ParseArgs(args); err == nil {
			t.Errorf("ParseArgs(%q) = %v, want an error", args, got)
		}
	}
}

func TestCommand_SortedAndRoundTrips(t *testing.T) {
	sets := map[string]Value{"zeta": {"", "high"}, "alpha": {"opus", ""}}
	cmd := Command(sets)
	if want := "agentsync hook claude-agent --set alpha=opus/ --set zeta=/high || exit 1"; cmd != want {
		t.Fatalf("Command = %q, want %q", cmd, want)
	}
	back, err := ParseArgs(strings.Fields(cmd)[3:])
	if err != nil || !reflect.DeepEqual(back, sets) {
		t.Fatalf("round trip = %v, %v; want %v", back, err, sets)
	}
	if Command(nil) != "" {
		t.Error("Command(nil) should be empty")
	}
	if got := (Value{"sonnet", ""}).String(); got != "sonnet / -" {
		t.Errorf("String = %q", got)
	}
}

func TestRewrite(t *testing.T) {
	sets := map[string]Value{"probe": {"sonnet", "high"}, "half": {"", "low"}}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{` +
		`"description":"d","prompt":"a <b> & c","subagent_type":"probe","model":"haiku",` +
		`"run_in_background":false,"n":1.50,"nested":{"k":[1,2]}}}`

	out, err := Rewrite([]byte(payload), sets)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		HookSpecificOutput map[string]json.RawMessage `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	if _, ok := got.HookSpecificOutput["permissionDecision"]; ok {
		t.Error("output must not carry a permissionDecision")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(got.HookSpecificOutput["updatedInput"], &input); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"description": `"d"`, "prompt": `"a <b> & c"`, "subagent_type": `"probe"`,
		"model": `"sonnet"`, "effort": `"high"`, "run_in_background": "false",
		"n": "1.50", "nested": `{"k":[1,2]}`,
	}
	if len(input) != len(want) {
		t.Errorf("updatedInput has %d fields, want %d: %s", len(input), len(want), out)
	}
	for k, v := range want {
		if string(input[k]) != v {
			t.Errorf("updatedInput[%s] = %s, want %s", k, input[k], v)
		}
	}
	if !strings.Contains(string(out), `"hookEventName":"PreToolUse"`) {
		t.Errorf("no hookEventName in %s", out)
	}

	// One part set: the other is left as the parent passed it.
	out, err = Rewrite([]byte(`{"tool_input":{"subagent_type":"half","model":"opus"}}`), sets)
	if err != nil || !strings.Contains(string(out), `"model":"opus"`) || !strings.Contains(string(out), `"effort":"low"`) {
		t.Errorf("half override = %s, %v", out, err)
	}

	for _, in := range []string{
		`{"tool_input":{"subagent_type":"other"}}`,
		`{"tool_input":{"prompt":"no agent named"}}`,
	} {
		if out, err := Rewrite([]byte(in), sets); out != nil || err != nil {
			t.Errorf("Rewrite(%s) = %s, %v; want nothing", in, out, err)
		}
	}

	for _, in := range []string{
		``, `not json`, `[]`, `{}`, `{"tool_input":null}`, `{"tool_input":"x"}`,
		`{"tool_input":{"subagent_type":7}}`, `{"tool_input":{}} trailing`,
	} {
		if _, err := Rewrite([]byte(in), sets); err == nil {
			t.Errorf("Rewrite(%q) = nil error, want one", in)
		}
	}
}

func settings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, path string) *File {
	t.Helper()
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	return m
}

func TestLoad_Refusals(t *testing.T) {
	ours := `{"type":"command","command":"agentsync hook claude-agent --set a=sonnet/"}`
	tests := map[string]string{
		"not json":            `{`,
		"not an object":       `[]`,
		"null":                `null`,
		"trailing data":       `{} {}`,
		"hooks not an object": `{"hooks":[]}`,
		"PreToolUse not list": `{"hooks":{"PreToolUse":{}}}`,
		"two of ours": `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[` + ours + `]},` +
			`{"matcher":"Agent","hooks":[` + ours + `]}]}}`,
		"ours shares an entry": `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[` + ours + `,{"type":"command","command":"echo"}]}]}}`,
		"ours has a bad value": `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[{"type":"command","command":"agentsync hook claude-agent --set a=gpt/"}]}]}}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if f, err := Load(settings(t, content)); err == nil {
				t.Fatalf("Load = %+v, want an error", f)
			}
		})
	}
	t.Run("unreadable path", func(t *testing.T) {
		if _, err := Load(t.TempDir()); err == nil {
			t.Fatal("Load of a directory should fail")
		}
	})
}

func TestLoad_FindsOursAmongOthers(t *testing.T) {
	path := settings(t, `{"hooks":{"PreToolUse":["odd",{"matcher":"Bash","hooks":"odd"},`+
		`{"matcher":"Bash","hooks":[{"type":"command","command":"agentsync hook claude-agentx"}]},`+
		`{"matcher":"Agent","hooks":[{"type":"command","command":"agentsync  hook claude-agent --set a=sonnet/high"}]}]}}`)
	f := mustLoad(t, path)
	if f.index != 3 || !reflect.DeepEqual(f.Sets, map[string]Value{"a": {"sonnet", "high"}}) {
		t.Fatalf("Load = index %d, sets %v", f.index, f.Sets)
	}
}

func TestSave_NewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	f := mustLoad(t, path)
	if f.Command != "" || len(f.Sets) != 0 {
		t.Fatalf("missing file loaded as %+v", f)
	}
	if err := f.Save(map[string]Value{"b": {"", "high"}, "a": {"sonnet", "medium"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := `{
  "hooks": {
    "PreToolUse": [
      {
        "hooks": [
          {
            "command": "agentsync hook claude-agent --set a=sonnet/medium --set b=/high || exit 1",
            "type": "command"
          }
        ],
        "matcher": "Agent"
      }
    ]
  }
}
`
	if string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// Clearing the only override of a file holding nothing else removes it.
	if err := mustLoad(t, path).Save(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file should be gone, stat = %v", err)
	}
	// A file that never existed and would hold nothing stays absent.
	if err := mustLoad(t, path).Save(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file should still be absent, stat = %v", err)
	}
}

func TestSave_KeepsEverythingElse(t *testing.T) {
	path := settings(t, `{"permissions":{"allow":["Bash(ls)"]},"n":1.0,"big":12345678901234567890,`+
		`"hooks":{"Stop":[{"hooks":[]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo a"}]}]}}`)
	if err := mustLoad(t, path).Save(map[string]Value{"a": {"opus", ""}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, s := range []string{`"n": 1.0`, `"big": 12345678901234567890`, `"Bash(ls)"`, `"Stop"`, `"echo a"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("lost %s:\n%s", s, b)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the existing 0644", fi.Mode().Perm())
	}
	pre := readJSON(t, path)["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 || pre[0].(map[string]any)["matcher"] != "Bash" {
		t.Fatalf("PreToolUse = %v; the user's hook must stay first", pre)
	}

	// Replaced in place, then removed, leaving the user's entries untouched.
	if err := mustLoad(t, path).Save(map[string]Value{"a": {"", "low"}}); err != nil {
		t.Fatal(err)
	}
	if f := mustLoad(t, path); f.index != 1 || f.Command != "agentsync hook claude-agent --set a=/low || exit 1" {
		t.Fatalf("after replace: index %d, command %q", f.index, f.Command)
	}
	if err := mustLoad(t, path).Save(nil); err != nil {
		t.Fatal(err)
	}
	pre = readJSON(t, path)["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 || strings.Contains(readFile(t, path), Prefix) {
		t.Fatalf("after clear: %s", readFile(t, path))
	}
}

func TestSave_PrunesContainersItEmptied(t *testing.T) {
	path := settings(t, `{"model":"opus"}`)
	if err := mustLoad(t, path).Save(map[string]Value{"a": {"opus", ""}}); err != nil {
		t.Fatal(err)
	}
	if err := mustLoad(t, path).Save(nil); err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, path); !reflect.DeepEqual(got, map[string]any{"model": "opus"}) {
		t.Fatalf("file = %v, want only the user's key", got)
	}

	path = settings(t, `{"hooks":{"Stop":[]}}`)
	if err := mustLoad(t, path).Save(map[string]Value{"a": {"opus", ""}}); err != nil {
		t.Fatal(err)
	}
	if err := mustLoad(t, path).Save(nil); err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, path); !reflect.DeepEqual(got, map[string]any{"hooks": map[string]any{"Stop": []any{}}}) {
		t.Fatalf("file = %v, want the user's Stop hooks kept", got)
	}
}

func TestSave_Errors(t *testing.T) {
	// A dangling .claude symlink reads as "no file" but cannot be created.
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	f := mustLoad(t, filepath.Join(dir, ".claude", "settings.local.json"))
	if err := f.Save(map[string]Value{"a": {"opus", ""}}); err == nil {
		t.Error("Save under a dangling .claude should fail")
	}

	// The file turned into a non-empty directory after Load: neither rename
	// nor remove can replace it.
	path := settings(t, `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[{"type":"command","command":"agentsync hook claude-agent --set a=opus/"}]}]}}`)
	f = mustLoad(t, path)
	g := mustLoad(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(map[string]Value{"a": {"sonnet", ""}}); err == nil {
		t.Error("Save over a directory should fail")
	}
	if err := g.Save(nil); err == nil {
		t.Error("removing a non-empty directory should fail")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSymlink_ReadButNeverWritten(t *testing.T) {
	content := `{"hooks":{"PreToolUse":[{"matcher":"Agent","hooks":[{"type":"command","command":"agentsync hook claude-agent --set a=opus/ || exit 1"}]}]}}`
	real := settings(t, content)
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.local.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f := mustLoad(t, link)
	if !reflect.DeepEqual(f.Sets, map[string]Value{"a": {"opus", ""}}) {
		t.Fatalf("Load through the link = %v", f.Sets)
	}
	for _, sets := range []map[string]Value{{"a": {"sonnet", ""}}, nil} {
		if err := f.Save(sets); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Save(%v) = %v, want a symlink refusal", sets, err)
		}
	}
	if readFile(t, real) != content {
		t.Error("the link's target was changed")
	}

	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := mustLoad(t, dangling).Save(map[string]Value{"a": {"opus", ""}}); err == nil {
		t.Error("Save through a dangling link should be refused")
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a file was created through the dangling link")
	}
}

func TestSave_KeepsFieldsOnItsHook(t *testing.T) {
	path := settings(t, `{"hooks":{"PreToolUse":[{"matcher":"Agent|Task","hooks":[{"type":"command","timeout":30,`+
		`"command":"agentsync hook claude-agent --set a=opus/ || exit 1"}]}]}}`)
	if err := mustLoad(t, path).Save(map[string]Value{"a": {"sonnet", ""}}); err != nil {
		t.Fatal(err)
	}
	b := readFile(t, path)
	for _, w := range []string{`"timeout": 30`, `"matcher": "Agent|Task"`, `--set a=sonnet/ || exit 1`} {
		if !strings.Contains(b, w) {
			t.Errorf("missing %s:\n%s", w, b)
		}
	}
}
