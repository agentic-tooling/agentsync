package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentic-tooling/agentsync/internal/discover"
	"github.com/agentic-tooling/agentsync/internal/doctor"
	"github.com/agentic-tooling/agentsync/internal/optimize"
	"github.com/agentic-tooling/agentsync/internal/override"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

// hookStdin is where the hook reads Claude Code's payload; tests replace it.
var hookStdin io.Reader = os.Stdin

// exitHookFailed is the hook's only failure code. Claude Code reads exit 2
// from a PreToolUse hook as "block this call", so a broken override exits 1
// and the subagent runs on its own settings instead.
const exitHookFailed = 1

// runHook is the hidden command Claude Code runs before every Agent call.
func runHook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "claude-agent" {
		fmt.Fprintln(stderr, "agentsync: usage: agentsync hook claude-agent --set NAME=MODEL/EFFORT ...")
		return exitHookFailed
	}
	sets, err := override.ParseArgs(args[1:])
	var out []byte
	if err == nil {
		var in []byte
		if in, err = io.ReadAll(hookStdin); err == nil {
			out, err = override.Rewrite(in, sets)
		}
	}
	if err == nil {
		_, err = stdout.Write(out)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentsync hook: %v\n", err)
		return exitHookFailed
	}
	return exitOK
}

// settingsPath is the settings file beside a project's .claude/agents.
func settingsPath(agentsDir string) string {
	return filepath.Join(filepath.Dir(agentsDir), "settings.local.json")
}

// runOverride lists, sets or clears a project's agent overrides. Only --apply
// writes, and only <root>/.claude/settings.local.json.
func runOverride(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("override", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	all := registerAll(fs)
	agent := fs.String("agent", "", "agent to override")
	model := fs.String("model", "", "model the agent runs on in this project")
	effort := fs.String("effort", "", "effort the agent runs at in this project")
	clearName := fs.String("clear", "", "remove this agent's override")
	apply := fs.Bool("apply", false, "write the settings file; without it override is a dry run")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFailure
	}
	set := override.Value{Model: *model, Effort: *effort}
	err := overrideFlagsErr(fs, *agent, *clearName, set, *apply)
	if err == nil && *all {
		err = fmt.Errorf("override works on one project, so --%s is refused; run it inside the project", allFlag)
	}
	var repo *scan.Repo
	var repoFrom string
	var root rootChoice
	var f *override.File
	if err == nil {
		repo, repoFrom, err = openRepo()
	}
	if err == nil {
		root, err = resolveProject(repo.Home)
	}
	agentsDir := filepath.Join(root.Path, ".claude", "agents")
	if err == nil {
		f, err = override.Load(settingsPath(agentsDir))
	}
	if err == nil && *agent != "" {
		err = requireAgent(repo, root.Path, agentsDir, *agent)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	emitRepoChoice(stdout, repo, repoFrom)
	emitRootChoice(stdout, repo.Home, root)
	shown := displayPath(repo.Home, f.Path)
	if *agent == "" && *clearName == "" {
		emitOverrides(stdout, shown, f.Sets)
		return exitOK
	}

	sets := maps.Clone(f.Sets)
	if *agent != "" {
		sets[*agent] = set
	} else if _, ok := sets[*clearName]; ok {
		delete(sets, *clearName)
	} else {
		fmt.Fprintf(stdout, "%s has no override in %s; nothing to change.\n", *clearName, shown)
		return exitOK
	}
	after := override.Command(sets)
	if after == f.Command {
		fmt.Fprintf(stdout, "%s already has that override in %s; nothing to change.\n", *agent, shown)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s, PreToolUse hook:\n  before: %s\n  after:  %s\n", shown, orNoEntry(f.Command), orNoEntry(after))
	if !*apply {
		fmt.Fprintln(stdout, "Dry run: nothing written. Pass --apply to write it.")
		return exitOK
	}
	if err := f.Save(sets); err != nil {
		fmt.Fprintf(stderr, "agentsync: writing %s: %v\n", f.Path, err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "Wrote %s. A Claude Code session that is already running may need a restart to pick it up.\n", shown)
	return exitOK
}

func overrideFlagsErr(fs *flag.FlagSet, agent, clear string, set override.Value, apply bool) error {
	switch {
	case fs.NArg() > 0:
		return errors.New(strayArg("override", fs.Arg(0), nil))
	case clear != "" && (agent != "" || set != override.Value{}):
		return errors.New("--clear takes no --agent, --model or --effort")
	case agent == "" && set != override.Value{}:
		return errors.New("--model and --effort need --agent")
	case agent != "":
		return override.Validate(agent, set)
	case clear == "" && apply:
		return errors.New("--apply needs --agent or --clear; without them override only lists")
	}
	return nil
}

// requireAgent refuses a name no agent in the root's own agents directory
// declares, so a typo cannot write an override that never matches.
func requireAgent(repo *scan.Repo, root, agentsDir, name string) error {
	res, err := repo.Scan(root)
	if err != nil {
		return err
	}
	for _, e := range res.Entries {
		if filepath.Dir(e.Path) != agentsDir {
			continue
		}
		if n, _ := agentName(e.Path); n == name {
			return nil
		}
	}
	return fmt.Errorf("--agent %s: no agent in %s has that frontmatter name", name, agentsDir)
}

// agentName is the frontmatter name Claude Code matches subagent_type
// against, and false for a file that is not an agent.
func agentName(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !discover.IsAgent(string(data)) {
		return "", false
	}
	return optimize.Field(string(data), "name"), true
}

func emitOverrides(w io.Writer, shown string, sets map[string]override.Value) {
	if len(sets) == 0 {
		fmt.Fprintf(w, "No overrides in %s.\n", shown)
		return
	}
	names := make([]string, 0, len(sets))
	for n := range sets {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "Overrides in %s (model / effort, - keeps the agent's own):\n", shown)
	for _, n := range names {
		fmt.Fprintf(w, "  %-28s %s\n", n, sets[n])
	}
}

func orNoEntry(cmd string) string {
	if cmd == "" {
		return "(no agentsync entry)"
	}
	return cmd
}

// entryOverrides maps each scanned agent file that has an override to it. A
// settings file it cannot read is warned about, never shown as "no override".
func entryOverrides(res scan.Result, stderr io.Writer) map[string]override.Value {
	out := map[string]override.Value{}
	for _, d := range res.Dirs {
		f, err := override.Load(settingsPath(d))
		if err != nil {
			fmt.Fprintf(stderr, "agentsync: warning: overrides not shown: %v\n", err)
			continue
		}
		if len(f.Sets) == 0 {
			continue
		}
		for _, e := range res.Entries {
			if filepath.Dir(e.Path) != d {
				continue
			}
			// A non-agent's name is "", which no override can carry.
			n, _ := agentName(e.Path)
			if v, ok := f.Sets[n]; ok {
				out[e.Path] = v
			}
		}
	}
	return out
}

// hookProbe runs the agentsync on PATH with a stored hook command's arguments
// and returns why it cannot serve them, or "" when it can.
func hookProbe(command string) string {
	bin, err := exec.LookPath("agentsync")
	if err != nil {
		return "the override hook runs agentsync, which is not on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := strings.Fields(strings.TrimSuffix(command, override.Guard))[1:]
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(`{"tool_input":{"subagent_type":"x"}}`)
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("%s fails as the override hook (%v); it may predate overrides", bin, err)
	}
	return ""
}

// overrideFindings reports projects whose override hook cannot run, which
// Claude Code treats as no hook: the agents run unchanged. Each distinct hook
// command is probed once.
func overrideFindings(home string, dirs []string) ([]doctor.Finding, []scan.ScanProblem) {
	var findings []doctor.Finding
	var problems []scan.ScanProblem
	probed := map[string]string{}
	for _, d := range dirs {
		f, err := override.Load(settingsPath(d))
		if err != nil {
			problems = append(problems, scan.ScanProblem{Path: settingsPath(d), Err: err.Error()})
			continue
		}
		if f.Command == "" {
			continue
		}
		why, ok := probed[f.Command]
		if !ok {
			why = hookProbe(f.Command)
			probed[f.Command] = why
		}
		if why == "" {
			continue
		}
		findings = append(findings, doctor.Finding{
			Kind:     doctor.KindOverrideCannotRun,
			KindName: doctor.KindOverrideCannotRun.String(),
			Project:  scan.ProjectKey(home, d),
			Path:     f.Path,
			Detail:   why,
		})
	}
	return findings, problems
}
