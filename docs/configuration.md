# Configuration

agentsync does not guess where your trees live. A run that needs a setting that
is unset refuses with exit 2.

| Setting | Flag | Environment | Used for |
|---|---|---|---|
| Canonical agents repo | `--repo PATH` | `$AGENTSYNC_REPO` | every command |
| Default tree of projects | `--root PATH` | `$AGENTSYNC_ROOT` | `--all` and `inventory` |

```bash
export AGENTSYNC_REPO="$HOME/src/my-agents"
export AGENTSYNC_ROOT="$HOME/src"
```

`$AGENTSYNC_ROOT` must be an absolute path.

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
- `--all` scans `$AGENTSYNC_ROOT`, the machine-wide view. It is refused
  together with `--root`, not resolved by precedence.

A derived root also selects that project for `link`, as `--project <key>`
would. That is what makes `agentsync link --agent NAME --apply` a complete
command inside a project. It is a selection, not an exemption: `--agent` still
requires a project and is refused when the root is the default tree and none
was named.

Every run that reports prints the root it chose and how it decided. With
`--json` (and `inventory --stdout`) that line goes to stderr so stdout stays
machine-readable. A run refused before it reports prints nothing to stdout.

## inventory is the exception

`inventory` never derives a root from the working directory. It describes every
agent on the machine, so a project-scoped run would overwrite the machine-wide
document under the same name. It uses `--root` or `$AGENTSYNC_ROOT`; `--all` is
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
| `--all` | scan `$AGENTSYNC_ROOT` |
| `--json` | machine-readable output |
| `--allow-outside-home` | see above |
