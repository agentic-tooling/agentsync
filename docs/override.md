# override

`agentsync override` sets the model and effort one Claude agent runs on in one
checkout, without forking the agent. The agent stays a link to the canonical
repo and keeps receiving its updates. Claude Code only; Codex has no way to do
this without a fork.

```bash
agentsync override                                        # list this project's overrides
agentsync override --agent go-merge-reviewer --model sonnet --effort medium
agentsync override --agent go-merge-reviewer --effort high --apply
agentsync override --clear go-merge-reviewer --apply
```

Root and repo resolve as for every other command (see
[configuration](configuration.md)). Without `--apply` it prints the change and
writes nothing.

## Flags

| Flag | Meaning |
|---|---|
| `--agent NAME` | the agent to override, by the `name:` in its frontmatter; it must be in the root's `.claude/agents/` |
| `--model M` | `haiku`, `sonnet`, `opus` or `fable` |
| `--effort E` | `low`, `medium`, `high`, `xhigh` or `max` |
| `--clear NAME` | remove that agent's override |
| `--apply` | write the change |

`--agent` needs `--model`, `--effort` or both. Each set replaces the agent's
whole override, so `--agent x --effort high` leaves the model to the agent's
frontmatter. `inherit` is refused: clearing the override does that. Full model
IDs are refused because the Agent tool accepts only the aliases. `--all` is
refused: an override belongs to one project.

## How it works

The overrides live in one `PreToolUse` hook in
`<root>/.claude/settings.local.json`:

```json
{"matcher": "Agent", "hooks": [{"type": "command",
  "command": "agentsync hook claude-agent --set go-merge-reviewer=sonnet/medium || exit 1"}]}
```

Before each subagent call, Claude Code runs that command. If the call's
`subagent_type` has an override, the hook hands back the call's input with
`model` and `effort` replaced, which Claude Code uses over the agent's
frontmatter and over a model the parent passed. Other agents are untouched.

agentsync owns only the hook whose command starts with
`agentsync hook claude-agent`, and changes only its `command`, so fields such
as a `timeout` you add stay. Every other setting and hook in the file is kept,
though the file is rewritten with its keys sorted and two-space indents. A file
that is not valid JSON is refused with exit 2 and left alone. A symlinked file
is read through the link, but `--apply` refuses to write through it (exit 2).
Clearing the last override removes the hook, and the file too if nothing else
is in it.

The hook runs `agentsync` from `PATH`. If it is missing, or too old to have the
hook, the agent runs on its frontmatter values; [doctor](doctor.md) reports
this. The hook exits 1 on any failure and never 2, because exit 2 would block
the subagent call; the `|| exit 1` keeps an old agentsync from exiting 2.

A running Claude Code session may need a restart to pick up a change.

## Limits

- Per user, per checkout: teammates and other worktrees do not get the
  override. Claude Code adds `settings.local.json` to your global git ignore
  only once it has written one itself, so if `git status` shows the file, add
  it to `.gitignore`.
- `$CLAUDE_CODE_EFFORT_LEVEL` still beats the effort override, and
  `$CLAUDE_CODE_SUBAGENT_MODEL_FORCE` beats both.
- Forked subagents ignore a model override.
- Resuming a subagent with SendMessage does not run the hook again.

[scan](scan.md) shows each override beside its agent, and
[optimize](optimize.md) notes when one masks the agent's own values.

## Exit codes

- `0`: listed, changed, or nothing to change.
- `2`: bad flags, an unknown agent, a model or effort Claude Code does not
  accept, no repo or root configured, a settings file that cannot be read or
  edited, or a failed write.

See [exit codes](exit-codes.md).
