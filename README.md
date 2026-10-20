# agentsync

Every agent Claude Code loads costs context in every session, so a project
should carry only the agents it needs. Copying agent files between projects
drifts, and the copies you customized get overwritten or lost.

`agentsync` keeps every agent in one canonical git repo and links only the ones
each project needs into its `.claude/agents/`, as per-file symlinks. Agents that
exist in a single project are first captured into the canonical repo, so
replacing the file with a link loses nothing.

It is built to be safe to run:

- Every command that writes is a dry run until you pass `--apply`.
- It refuses to link a project holding a file that is backed up nowhere.
- It removes a symlink only if it recorded creating it and the link is broken,
  and deletes a captured file only if git still holds its bytes.
- Anything it cannot determine is a refusal with exit 2, never a guessed default.

## Install

```bash
go install github.com/agentic-tooling/agentsync/cmd/agentsync@latest
```

Or from a clone (installs to `$INSTALL_DIR`, default `~/.local/bin`):

```bash
mise trust && mise install
just install
```

## Quick start

```bash
agentsync init                                # find your agents repo, write the config file
```

`init` writes `~/.config/agentsync/config.toml`:

```toml
repo = "~/src/my-agents"   # your canonical agents repo
root = "~/src"             # the tree holding your projects
```

`--repo` and `--root`, or `$AGENTSYNC_REPO` and `$AGENTSYNC_ROOT`, work too and
take precedence; see [configuration](docs/configuration.md) and [init](docs/init.md).

```bash
cd ~/src/api
agentsync scan --at-risk                      # files protected nowhere else
agentsync import --apply                      # back them up into the repo
agentsync link --agent go-reviewer --apply    # link one agent into this project
agentsync doctor                              # what has drifted
```

Run without `--apply` first to see what a command would do.

## Commands

| Command | Does |
|---|---|
| `init` | find your agents repo and write the config file |
| `scan` | classify project agents against the canonical repo (read-only) |
| `import` | capture project-only agents into `project-agents/` |
| `link` | replace project agent files with symlinks into the canonical repo |
| `doctor` | report broken, foreign, or mismatched links and stale captures; never writes |
| `reconcile` | act on that drift, gated on what agentsync itself recorded |
| `inventory` | regenerate `inventory.json`, a record of every agent on the machine |
| `optimize` | ask an OpenRouter advisor which model and effort each agent should use; `--apply` writes the suggested changes |
| `override` | set one checkout's model and effort for a Claude agent without forking it |

`agentsync help` lists every flag.

## Documentation

- [Configuration](docs/configuration.md): repo and root settings, which tree is scanned, the home guard
- [init](docs/init.md)
- [scan](docs/scan.md)
- [import](docs/import.md)
- [link](docs/link.md)
- [doctor](docs/doctor.md)
- [reconcile](docs/reconcile.md)
- [inventory](docs/inventory.md)
- [optimize](docs/optimize.md)
- [override](docs/override.md)
- [State file](docs/state-file.md)
- [Exit codes](docs/exit-codes.md)
- [Development](docs/development.md): toolchain, CI, releases

## Changelog

Generated from Conventional Commits with git-cliff; see `just changelog-preview`
and [development](docs/development.md#changelog).

## License

MIT. See [LICENSE](LICENSE).
