# link

`agentsync link` replaces a project's real agent files with per-file symlinks
into the canonical repo, so one copy serves every project. It is the only
command that writes into a project's `.claude/agents/` ([override](override.md)
writes only `.claude/settings.local.json`), and it is a dry run without
`--apply`.

```bash
agentsync link                              # dry run
agentsync link --agent go-reviewer --apply  # from inside a project: link one agent
agentsync link --project src/api --agent go-reviewer --apply
agentsync link --exclude-local --apply      # project's own git tracks .claude/agents
```

| Flag | Meaning |
|---|---|
| `--apply` | create the links; without it `link` is a dry run |
| `--project KEY` | restrict to this project key; repeatable; use the key `scan` reports |
| `--agent NAME` | also link this agent into the selected projects even if no file is present; repeatable; requires a selected project |
| `--exclude-local` | add the agents directory to the project's `.git/info/exclude` before linking |
| `--undecline=KEY` | clear the declined marker for one `<project>/<name>`; repeatable; needs `--apply` |

`--agent` with no project selected is refused: it would provision every project
under the scan root. Inside a project the derived root selects it (see
[configuration](configuration.md)). `link` does not decide which agents a
project should have; it provisions what it is told to.

## Rules

- **Import before link.** A project holding a file whose content exists nowhere
  else (no copy in the canonical repo, not committed in its own repo) is refused
  as a whole: nothing in it is linked, adopted, or created, and the run exits 1.
  Other projects in the same run are unaffected. Run [import](import.md) first.
- **The target is the project's counterpart.** `project-agents/<key>/<name>.md`
  wins over `agents/<name>.md`, so a customized agent is never silently replaced
  by the same-named global one.
- **Only two states are written.** A real file byte-identical to its counterpart
  is adopted (replaced by a link); an agent named with `--agent` that is absent
  is created. Broken links, foreign links, overrides, and unadopted files are
  reported and left as they are. `link` never unlinks and never repoints.
- **Two gates on git.** A project whose own git tracks the agents directory is
  refused, because a symlink names an absolute path that resolves on this
  machine only and committing one breaks every other clone. `--exclude-local`
  adds the directory to `.git/info/exclude` first (not `.gitignore`, which is
  tracked and shared). Excluding does not untrack what is already committed, so
  replacing a committed file with a link still shows as a change in that project.

## State and declined agents

`link --apply` records each symlink it creates, with the target's content hash,
in `.agentsync-state.json` in the canonical repo. See [state file](state-file.md).
If a recorded link was deleted on purpose, `reconcile` writes a declined marker
and `link` reports that agent without recreating it. `--undecline` is the only
override.

## After a link changes

When `link --apply` or `reconcile --apply` creates or removes a symlink, the
last line of the run says the Claude Code session should be restarted, since
agents are read when a session starts. In practice Claude Code has picked up
newly linked agents mid-session. A new agent may be usable right away; restart
if it is not. The notice does not print on a dry run, on a no-op, or when the
only writes were exclude lines, state records, declined markers, captures, or a
`--rekey`.
