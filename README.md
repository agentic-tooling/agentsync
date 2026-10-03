# agentsync

`agentsync` provisions [Claude Code](https://claude.com/claude-code) agents
from one canonical agents repository into your projects, as per-file symlinks.

Keep every agent definition in one git repo (`agents/<name>.md`) and link only
the agents each project needs into its `.claude/agents/`. Per-file symlinks,
rather than one directory symlink or a large global tier, keep each project's
agent set small: every agent Claude Code loads costs context in every session.
The canonical repo also holds `project-agents/<project-key>/`, a backup of
agents that exist in one project only, so nothing is lost when a file is
replaced by a link.

## Install

```bash
go install github.com/agentic-tooling/agentsync/cmd/agentsync@latest
```

Or from a clone (installs to `$INSTALL_DIR`, default `~/.local/bin`):

```bash
mise trust && mise install
just install
```

## Configuration

agentsync does not guess where your trees live. Both settings are required
where they apply, and a run that needs one that is unset refuses with exit 2.

| Setting | Flag | Environment | Used for |
|---|---|---|---|
| Canonical agents repo | `--repo PATH` | `$AGENTSYNC_REPO` | every command |
| Default tree of projects | `--root PATH` | `$AGENTSYNC_ROOT` | `--all`, `inventory`, and runs outside any project |

```bash
export AGENTSYNC_REPO="$HOME/src/my-agents"
export AGENTSYNC_ROOT="$HOME/src"
```

Run inside a project, the scan root is that project: agentsync walks up from
the working directory, stopping before `$HOME`, and takes the first directory
holding `.claude/agents/`, else `.claude/`, else `.git/`. If none is found it
falls back to `$AGENTSYNC_ROOT`. `--all` scans `$AGENTSYNC_ROOT` explicitly and
is refused together with `--root`. Every run prints the root it chose and why.

`inventory` never derives a root from the working directory: it describes the
whole tree, so it uses `--root` or `$AGENTSYNC_ROOT`.

## Commands

Every command that writes is a dry run by default; `--apply` writes. The exception is `inventory`, which only ever rewrites its own derived file.

| Command | Does |
|---|---|
| `scan` | classify project agents against the canonical repo (read-only) |
| `import` | capture project-only agents into `project-agents/` |
| `link` | replace project agent files with symlinks into the canonical repo |
| `doctor` | report broken, foreign, or mismatched links and stale captures; never writes |
| `reconcile` | act on that drift, gated on what agentsync itself recorded |
| `inventory` | regenerate `inventory.json`, a record of every agent on the machine |

```bash
agentsync scan                                  # the project you are in
agentsync scan --all --at-risk                  # files protected nowhere else
agentsync import --apply                        # back up project-only agents
agentsync link --agent go-reviewer --apply      # link one agent into this project
agentsync doctor                                # what has drifted
agentsync reconcile --prune --apply             # remove broken links agentsync created
agentsync inventory --check                     # exit 1 if inventory.json is stale
```

Links agentsync creates are recorded in `.agentsync-state.json` at the root of
the canonical repo. It names links by absolute path, so keep it out of git.
After a run that creates or removes a symlink, restart your Claude Code
session: agents are loaded when a session starts.

`agentsync help` lists every flag.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | the run completed and found something a human must settle |
| `2` | agentsync could not do its job (bad flags, missing config, unreadable path) |

## Development

```bash
mise trust && mise install   # once per clone: installs the pinned toolchain
just check                   # full gate: vet, gofmt, build, staticcheck, golangci-lint, test, race, coverage
```

## Changelog

See [CHANGELOG.md](CHANGELOG.md). Commits follow [Conventional Commits](https://www.conventionalcommits.org/) and the changelog is generated with [git-cliff](https://git-cliff.org/).

## License

MIT. See [LICENSE](LICENSE).
