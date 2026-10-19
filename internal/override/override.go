// Package override keeps per-checkout model and effort overrides for Claude
// agents in one PreToolUse hook entry of .claude/settings.local.json. The
// overrides live in the hook's own command line, so there is no second file to
// drift from it.
package override

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/optimize"
)

// Prefix starts the one hook command agentsync owns in a settings file.
const Prefix = "agentsync hook claude-agent"

// Guard ends the stored command. Claude Code blocks the subagent call when the
// hook exits 2, as an agentsync too old to know "hook" does, so the shell
// turns every failure into exit 1.
const Guard = "|| exit 1"

// Value is one agent's override. An empty part leaves the agent's own value.
type Value struct {
	Model, Effort string
}

// String is "model / effort", with "-" for a part left alone.
func (v Value) String() string {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	return dash(v.Model) + " / " + dash(v.Effort)
}

// safe limits names to characters a shell passes through unquoted, since
// Claude Code runs the hook command through one.
func safe(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return s != ""
}

// Validate refuses an agent name or value the hook could not carry or Claude
// Code would not accept.
func Validate(name string, v Value) error {
	if !safe(name) {
		return fmt.Errorf("agent name %q: only letters, digits, '-', '_' and '.' can go in the hook command", name)
	}
	if v.Model == "" && v.Effort == "" {
		return fmt.Errorf("%s: an override needs a model, an effort or both", name)
	}
	if err := validModel(v.Model); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if v.Effort != "" && !slices.Contains(optimize.Efforts, v.Effort) {
		return fmt.Errorf("%s: effort %q is not one of %s", name, v.Effort, strings.Join(optimize.Efforts, ", "))
	}
	return nil
}

func validModel(m string) error {
	if m == "" {
		return nil
	}
	if m == "inherit" {
		return errors.New("model inherit means no override; use --clear instead")
	}
	// Aliases only: the Agent tool's model parameter is an enum of them.
	var aliases []string
	for _, f := range optimize.Families {
		if f.Alias == m {
			return nil
		}
		aliases = append(aliases, f.Alias)
	}
	return fmt.Errorf("model %q is not one of %s", m, strings.Join(aliases, ", "))
}

// ParseArgs reads the hook's arguments, "--set NAME=MODEL/EFFORT" pairs with
// an optional trailing Guard. It is the one parser for both the running hook
// and the command stored in settings.
func ParseArgs(args []string) (map[string]Value, error) {
	sets := map[string]Value{}
	if g := strings.Fields(Guard); len(args) >= len(g) && slices.Equal(args[len(args)-len(g):], g) {
		args = args[:len(args)-len(g)]
	}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "--set" || i+1 == len(args) {
			return nil, fmt.Errorf("want --set NAME=MODEL/EFFORT, got %q", strings.Join(args[i:], " "))
		}
		name, rest, ok := strings.Cut(args[i+1], "=")
		model, effort, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 {
			return nil, fmt.Errorf("--set %s: want NAME=MODEL/EFFORT", args[i+1])
		}
		if _, dup := sets[name]; dup {
			return nil, fmt.Errorf("--set %s: %s is set twice", args[i+1], name)
		}
		v := Value{Model: model, Effort: effort}
		if err := Validate(name, v); err != nil {
			return nil, err
		}
		sets[name] = v
	}
	return sets, nil
}

// Command is the hook command carrying sets, sorted by agent name, or "" for
// none.
func Command(sets map[string]Value) string {
	if len(sets) == 0 {
		return ""
	}
	names := make([]string, 0, len(sets))
	for n := range sets {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(Prefix)
	for _, n := range names {
		fmt.Fprintf(&b, " --set %s=%s/%s", n, sets[n].Model, sets[n].Effort)
	}
	return b.String() + " " + Guard
}

// Rewrite returns the PreToolUse hook output that applies sets to an Agent
// call, or nil when the call's agent has no override. Every other tool_input
// field is passed back byte for byte.
func Rewrite(payload []byte, sets map[string]Value) ([]byte, error) {
	var in struct {
		ToolInput map[string]json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, fmt.Errorf("hook input: %w", err)
	}
	if in.ToolInput == nil {
		return nil, errors.New("hook input has no tool_input object")
	}
	var agent string
	if raw, ok := in.ToolInput["subagent_type"]; ok {
		if err := json.Unmarshal(raw, &agent); err != nil {
			return nil, fmt.Errorf("hook input subagent_type: %w", err)
		}
	}
	v, ok := sets[agent]
	if !ok {
		return nil, nil
	}
	// Strings always marshal.
	if v.Model != "" {
		in.ToolInput["model"], _ = json.Marshal(v.Model)
	}
	if v.Effort != "" {
		in.ToolInput["effort"], _ = json.Marshal(v.Effort)
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  in.ToolInput,
	}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(out)
	return buf.Bytes(), err
}

// File is a settings file and the agentsync entry found in it.
type File struct {
	Path string
	// Command is the agentsync hook command as written, "" when there is none.
	Command string
	// Sets are the overrides Command carries; never nil.
	Sets map[string]Value

	doc    map[string]any
	hook   map[string]any
	exists bool
	mode   fs.FileMode
	index  int
}

// Load reads a settings file, following a symlink as Claude Code does; a
// missing one is empty. Anything it cannot read as a settings file is an
// error rather than "no overrides".
func Load(path string) (*File, error) {
	f := &File{Path: path, Sets: map[string]Value{}, doc: map[string]any{}, mode: 0o600, index: -1}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	f.exists, f.mode = true, fi.Mode().Perm()

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&f.doc); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s has data after its JSON object", path)
	}
	if f.doc == nil {
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	pre, err := preToolUse(f.doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, e := range pre {
		entry, _ := e.(map[string]any)
		hooks, _ := entry["hooks"].([]any)
		for _, h := range hooks {
			hook, _ := h.(map[string]any)
			cmd, _ := hook["command"].(string)
			fields := strings.Fields(cmd)
			if len(fields) < 3 || strings.Join(fields[:3], " ") != Prefix {
				continue
			}
			switch {
			case f.index >= 0:
				return nil, fmt.Errorf("%s has more than one %q hook; remove all but one", path, Prefix)
			case len(hooks) != 1:
				return nil, fmt.Errorf("%s: the %q hook shares its PreToolUse entry with other hooks; give it an entry of its own", path, Prefix)
			}
			if f.Sets, err = ParseArgs(fields[3:]); err != nil {
				return nil, fmt.Errorf("%s: %q hook: %w", path, Prefix, err)
			}
			f.index, f.Command, f.hook = i, cmd, hook
		}
	}
	return f, nil
}

// preToolUse returns doc's hooks.PreToolUse list, refusing a shape agentsync
// could not edit without losing something.
func preToolUse(doc map[string]any) ([]any, error) {
	h, ok := doc["hooks"]
	if !ok {
		return nil, nil
	}
	hooks, ok := h.(map[string]any)
	if !ok {
		return nil, errors.New("hooks is not an object")
	}
	p, ok := hooks["PreToolUse"]
	if !ok {
		return nil, nil
	}
	pre, ok := p.([]any)
	if !ok {
		return nil, errors.New("hooks.PreToolUse is not a list")
	}
	return pre, nil
}

// Save writes the file with sets as its agentsync entry, or without the entry
// when sets is empty. An existing entry keeps every field but its command.
// Containers that end up empty go too, and a file left holding nothing is
// removed. Everything else is kept, though not its key order or formatting.
func (f *File) Save(sets map[string]Value) error {
	// Writing through a symlink would edit a file outside the project.
	if fi, err := os.Lstat(f.Path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; agentsync writes only a regular file", f.Path)
	}
	pre, _ := preToolUse(f.doc) // Load already refused a bad shape.
	switch {
	case len(sets) == 0 && f.index >= 0:
		pre = slices.Delete(pre, f.index, f.index+1)
	case f.index >= 0:
		f.hook["command"] = Command(sets)
	case len(sets) > 0:
		pre = append(pre, map[string]any{"matcher": "Agent", "hooks": []any{
			map[string]any{"type": "command", "command": Command(sets)},
		}})
	}
	hooks, _ := f.doc["hooks"].(map[string]any)
	if len(pre) > 0 {
		if hooks == nil {
			hooks = map[string]any{}
			f.doc["hooks"] = hooks
		}
		hooks["PreToolUse"] = pre
	} else if hooks != nil {
		delete(hooks, "PreToolUse")
		if len(hooks) == 0 {
			delete(f.doc, "hooks")
		}
	}

	if len(f.doc) == 0 {
		if !f.exists {
			return nil
		}
		return os.Remove(f.Path)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f.doc); err != nil {
		return err
	}
	return writeAtomic(f.Path, buf.Bytes(), f.mode)
}

// writeAtomic replaces path through a temp file in the same directory, so a
// reader sees the old file or the new one and never half of either.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agentsync-settings-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}
