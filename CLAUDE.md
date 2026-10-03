# agentsync

Go CLI that provisions Claude Code agents from a canonical agents repo into
projects as per-file symlinks. `README.md` has the user-facing contract.

## Layout

- `cmd/agentsync/` — flag parsing, root resolution, report output.
- `internal/` — `scan`, `importer`, `linker`, `doctor`, `reconcile`,
  `inventory`, `state`, and `testfs` (test helpers).

## Commands

```bash
mise trust && mise install   # once per clone
mise run check               # full gate: lint, build, staticcheck, golangci-lint, test, race, coverage
mise run format              # gofmt -w
mise run install             # build and put agentsync on PATH
```

`mise run check` must pass before asking for review.

## Rules

- **Never commit `.agentsync-state.json`.** It is gitignored and names links by
  absolute path, so it is meaningless in any other checkout.
- **No guessed defaults.** The canonical repo (`--repo` / `$AGENTSYNC_REPO`) and
  the wide tree (`--root` / `$AGENTSYNC_ROOT`) are refused with exit 2 when
  unset, never filled in from a path on the author's machine.
- **Fail closed.** Every tri-state and enum has its zero value as the refusing
  arm. Keep it that way.
- **Git probes:** no probe may read a non-zero exit as a clean negative answer.
  "No" and "could not answer" must never share a return value.
- **`inventory` never derives its root from the working directory.** It
  describes the whole tree; a project-scoped run would overwrite it with a
  different document under the same name.
- Exit codes: `0` clean, `1` something a human must settle, `2` the tool could
  not do its job. Tests assert them; keep them stable.
- Comments are one to three sentences and say only what the code cannot. No
  issue or PR references in comments.
