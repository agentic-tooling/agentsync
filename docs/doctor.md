# doctor

`agentsync doctor` reports drift the other commands do not. It writes nothing:
no `--apply`, no state file, no exclude line. It takes only the common flags
(see [configuration](configuration.md)). Run it from the project directory;
`--all` checks the whole default tree instead.

```bash
agentsync doctor
agentsync doctor --json
```

It reports six things:

- **Broken links.** A symlink into the canonical repo whose target is gone,
  usually an agent renamed or deleted there. Reported with the missing target.
- **Foreign links.** A symlink pointing outside the canonical repo. Reported for
  completeness and not counted against the exit code: it is someone else's link,
  and failing on it forever would make the exit code useless on a machine
  running another agent tool.
- **Mismatches.** A healthy link to the wrong file: the project is served
  `agents/<name>.md` while its own `project-agents/<key>/<name>.md` sits unused.
  `doctor` and [link](link.md) share one judgement of what a healthy link is, so
  they cannot disagree.
- **Stale captures.** A file in `project-agents/<key>/` with no live file at
  `<project>/.claude/agents/<name>.md`. The project deleted it, or moved (keys
  are path-derived, so a move re-keys it).
- **Orphaned capture directories.** A whole `project-agents/<key>/` whose project
  directory is gone, reported once for the directory.
- **Overrides that cannot run.** A project whose `.claude/settings.local.json`
  has an [override](override.md) hook, while `agentsync` is not on `PATH` or
  fails when run as that hook (an older agentsync, or one that rejects the
  stored overrides). The hook fails and the agents run on their frontmatter
  values. doctor finds out by running the `agentsync` on `PATH` with each
  distinct stored hook command's arguments and a minimal call on stdin.

`doctor` never recommends deleting a capture and never performs one: a stale
capture may be the only surviving copy. Repair is [reconcile](reconcile.md),
which can act because it reads the [state file](state-file.md).

## Second home check

`doctor` resolves every capture key through `$HOME`, so a wrong `$HOME` makes
every capture look like it belongs to a deleted project. When every home-keyed
capture directory comes back orphaned, `doctor` says so on stderr and in the
summary, and sets `home.suspect` in the JSON. `_outside-home/...` keys are
excluded from that evidence, which is why `--allow-outside-home` does not
silence the warning and does not need to.

The JSON `home` block has two independent fields: `status` is the verdict on
the projects the scan walked, and `suspect` is the verdict on capture keys
nothing walked. Read both; `status: "ok"` with `suspect: true` is the normal
shape of the problem this check exists to catch.

Exit 1 on a broken or mismatched link, a stale capture, an orphaned capture
directory, an override that cannot run, or a project path the scan could not
read. Exit 2 when a capture or a project's `settings.local.json` could not be
judged. See [exit codes](exit-codes.md).
