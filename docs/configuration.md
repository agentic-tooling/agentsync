# Configuration

agentsync does not guess where your trees live. A run that needs a setting that
is unset refuses with exit 2.

## The config file

The usual place to set them is `config.toml`:

```toml
repo = "~/src/my-agents"
root = "~/src"
```

It lives at `$XDG_CONFIG_HOME/agentsync/config.toml`, or at
`~/.config/agentsync/config.toml` when `$XDG_CONFIG_HOME` is unset or not an
absolute path. That holds on macOS too.

`repo` and `root` are the only keys. Each must be an absolute path or start
with `~/`, which expands against `$HOME`; `~` alone means `$HOME`. A missing file
is not an error, but a symlink there whose target is missing is a broken file.

## Flags and environment

| Setting | Flag | Environment | Config key | Used for |
|---|---|---|---|---|
| Canonical agents repo | `--repo PATH` | `$AGENTSYNC_REPO` | `repo` | every command |
| Default tree of projects | `--root PATH` | `$AGENTSYNC_ROOT` | `root` | `--all` and `inventory` |

```bash
export AGENTSYNC_REPO="$HOME/src/my-agents"
export AGENTSYNC_ROOT="$HOME/src"
```

`$AGENTSYNC_ROOT` must be an absolute path.

## Precedence

Each setting is resolved on its own, so the repo can come from the environment
and the root from the file. An empty or blank flag or variable counts as unset.

| Order | Source |
|---|---|
| 1 | flag |
| 2 | environment variable |
| 3 | config file |
| 4 | refuse with exit 2 |

The file is read only when a setting is still unset after the flag and the
environment. A broken file therefore never blocks a run that passes both, which
is how you work around it while fixing it.

Every run that reports prints a `Repo:` line and a `Root:` line naming where
each value came from, including the config file's path.

## A broken config file refuses

When the file has to be read, each of these refuses with exit 2 and names the
file:

- it exists but cannot be read, is a directory, or is a dangling symlink;
- it does not parse as TOML (the message gives the line), or a value has the
  wrong type, such as `repo = 5`;
- it holds a key other than `repo` or `root` (the message names the key);
- a key is present but empty;
- a value is not an absolute path after `~` expansion.

If neither `$XDG_CONFIG_HOME` nor `$HOME` is a usable absolute path, the file
cannot be located, and a run that needs it refuses with exit 2 saying it cannot
locate the config file.

## The canonical repo

The canonical repo holds `agents/<name>.md` (the agents you link) and
`project-agents/<project-key>/<name>.md` (captures of agents that exist in one
project only). A project key is the project's path relative to `$HOME`, for
example `src/api`. Projects outside `$HOME` are keyed `_outside-home/<absolute
path>`.

## Which tree gets scanned

Without `--root`, every command except `inventory` derives its root from the
working directory. It walks up, stopping before `$HOME`, and takes the first
directory holding `.claude/agents/`, else `.claude/`, else `.git/`. All three
are tried at each level before ascending, so a `.claude/` inside a git
repository resolves to that subdirectory, not the repository root. A project
with `.claude/` and no `agents/` yet resolves too, so `link --apply` can
provision its first agent.

If nothing is found before `$HOME`, the run is refused with exit 2 and nothing
is scanned: a new project with no markers yet would otherwise widen to every
project on the machine. Run inside a project, create its `.claude/agents/`, or
pass `--all` or `--root PATH`. A working directory that cannot be determined or
resolved, or an ancestor that cannot be read, also refuses with exit 2 rather
than widening to the whole tree.

- `--root PATH` names a tree explicitly and switches the derivation off.
- `--all` scans the default tree (`$AGENTSYNC_ROOT` or `root` in the config
  file), the machine-wide view. It is refused together with `--root`, not
  resolved by precedence.

A derived root also selects that project for `link`, as `--project <key>`
would. That is what makes `agentsync link --agent NAME --apply` a complete
command inside a project. It is a selection, not an exemption: `--agent` still
requires a project and is refused when the root is the default tree and none
was named.

Every run that reports prints the repo and root it chose and how it decided.
With `--json` (and `inventory --stdout`) those lines go to stderr so stdout
stays machine-readable. A run refused before it reports prints nothing to stdout.

## inventory is the exception

`inventory` never derives a root from the working directory. It describes every
agent on the machine, so a project-scoped run would overwrite the machine-wide
document under the same name. It uses `--root`, else the default tree; `--all` is
accepted and means what the default already means. See [inventory](inventory.md).

## The home guard

A wrong `$HOME` (sudo, cron, a container) re-keys every project into
`_outside-home/...`. On a write path that would build a second backup tree that
a later run with the correct `$HOME` never sees. So:

- `import` and `link` refuse to run (exit 2) before touching disk.
- `scan` and `doctor` only warn.
- `reconcile --apply` refuses; a `reconcile` dry run warns with the same
  sentence the refusal would carry.

If your projects genuinely live outside `$HOME`, pass `--allow-outside-home`.

`doctor` has a second check of its own for capture keys; see [doctor](doctor.md).

## Common flags

| Flag | Meaning |
|---|---|
| `--repo PATH` | canonical agents repo |
| `--root PATH` | tree to scan |
| `--all` | scan the default tree |
| `--json` | machine-readable output |
| `--allow-outside-home` | see above |
