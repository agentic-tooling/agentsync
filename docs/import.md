# import

`agentsync import` copies project-only agents into the canonical repo under
`project-agents/<project-key>/`, so nothing is lost when `link` later replaces
the file with a symlink. It writes only into the canonical repo, never into a
project's `.claude/agents/`, and only with `--apply`.

```bash
agentsync import                 # dry run: what a capture would do
agentsync import --apply         # capture it
agentsync import --fail-on-pending   # exit 1 if anything is uncaptured (CI)
agentsync import --apply --update=src/api/planner   # replace one stale capture
```

| Flag | Meaning |
|---|---|
| `--apply` | write the captures; without it `import` is a dry run |
| `--update=KEY` | replace the capture of one file, named as the `<project>/<name>` pair the report prints; repeatable |
| `--update` | bare: replace every capture that differs in the run |
| `--include-tracked` | also capture files committed in their own project's git repo |
| `--fail-on-pending` | exit 1 if anything is left uncaptured |

## What it will not do

- **Overwrite a different capture.** A destination holding different content may
  be the only copy of an older override. It is reported, left alone, and the run
  exits 1. Only `--update` replaces it.
- **Capture files that are not at risk.** A file committed in its own project's
  repo is skipped by default and counted in the report. `--include-tracked`
  captures it anyway.
- **Claim completeness over unread paths.** If the scan could not read part of
  the tree, `import` reports it and exits 1, because a project-only file may sit
  behind it. `import`'s exit 0 is the assertion `link` is gated on.

## `--update`

The per-file form is the one to reach for: you decide after looking at a diff of
that file. Attach the value with `=`. `--update KEY` with a space parses as the
bare form plus a stray argument, so `import` refuses positional arguments.
The bare form replaces every differing capture and so authorises replacements
you may not have reviewed.

Neither form replaces a capture whose current bytes are in no commit of the
canonical repo. `project-agents/` being version controlled is what makes
replacement survivable. The check is on the bytes, not the path: a capture that
was committed once and edited since is refused too, and so is a second
`--update` after the first, since `--update` itself leaves the capture modified
against `HEAD`. The refusal tells you to commit `project-agents/` and re-run.
If git cannot answer (not on `PATH`, `safe.directory` refusal, damaged object
store), the update is refused with a different message.

## `--fail-on-pending`

A dry run exits 0 whether or not captures are pending. `--fail-on-pending` is
the CI form. With `--apply` it fires only on writes that did not happen, so it
can stay on in a pipeline that also writes. It is the `import` analogue of
`scan --fail-on-risk`.
