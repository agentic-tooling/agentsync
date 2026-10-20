# reconcile

`agentsync reconcile` acts on the drift [doctor](doctor.md) reports, and only
where a fact recorded in the [state file](state-file.md) or provable from git
makes the act safe. It is a dry run without `--apply`, and every operation that
removes something needs its own flag on top. Run it from the project directory;
`--all` covers the whole default tree instead.

```bash
agentsync reconcile                              # report: what it could do and why not
agentsync reconcile --adopt --apply              # record links that predate the state file
agentsync reconcile --prune --apply              # remove broken links this tool created
agentsync reconcile --drop-capture=src/api/planner --apply
agentsync reconcile --rekey=src/old=src/new --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | make the changes |
| `--adopt` | record healthy, correctly aimed links with no record, with origin `adopted` |
| `--prune` | remove a recorded link that is broken and still points where the record says |
| `--prune-adopted` | widen `--prune` to adopted records; requires `--prune` |
| `--rekey=OLD=NEW` | move `project-agents/OLD/` to `project-agents/NEW/` and repoint links; repeatable |
| `--drop-capture=KEY` | delete one capture, named `<project>/<name>`; repeatable |

## Operations

- **`--adopt`** exists for links made before the state file existed. An adopt
  pass cannot tell such a link from one made by hand, since they are identical
  on disk, so an adopted record is a weaker claim than a created one; removing
  it later needs `--prune-adopted`. Only healthy links aimed at the project's
  own counterpart are adoptable: never a foreign link, a broken one, a real
  file, or a mismatch.
- **`--prune`** is the only thing in the toolchain that removes a symlink. It
  requires a record for that exact path with origin `created`, and that the path
  is still a symlink, still points where the record says, still points inside
  the canonical repo, and the target is gone. The disk facts are re-checked
  immediately before unlinking. A working link is never removed. The link is
  removed first and the record second, so a crash between them leaves a visible,
  undoable state rather than an unowned link.
- **Declined marker.** The one write no flag guards, because it records an
  observation, not a choice (see the table below). Reverse it with
  `link --undecline`.
- **`--rekey OLD=NEW`** is explicit only. `reconcile` proposes candidates in its
  report, with the exact command, and applies none. It refuses if `NEW` already
  holds captures, if `OLD` contains subdirectories, or if a link outside the
  destination project points into `OLD`.
- **`--drop-capture`** has no bare or bulk form. It is refused unless the
  capture's current bytes are in the canonical repo's `HEAD` (so `git show` still
  has them), and a git failure refuses too. It also refuses a capture a live
  symlink still points at. No flag deletes an orphaned capture directory.

## What it decides from the record

For a recorded path, the recorded content hash is the merge base:

| Observed | Recorded hash | Meaning | `reconcile` |
|---|---|---|---|
| link gone, real file present | matches | dereferenced | reports it; `link --apply` re-links |
| link gone, real file present | differs | edited after diverging | reports a conflict; capture it with `import`; never resolved automatically |
| link gone, nothing present | none | deleted on purpose | writes a declined marker so `link` does not recreate it |

The third row applies only when the agents directory still exists. If the
directory itself is gone (a project moved), the project is reported as
`project-gone` and nothing is declined or removed.

## Exit code

Narrower than `doctor`'s: `reconcile` exits 0 when there is nothing for it to do
or settle, which is not the same as nothing having drifted. A stale capture is
drift no flag here removes; `doctor` is where that assertion lives. Exit 1 for a
refused selector, a conflict, a diverged or dereferenced link, a removable
link left in place, or a path the scan could not read. Exit 2 for an unjudgeable path or a failed write. An
unrecorded healthy link is not exit 1, since dozens of them are normal on a
machine this tool has been run on.
