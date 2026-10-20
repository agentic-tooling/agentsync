# Exit codes

| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | the run completed and found something a human must settle |
| `2` | agentsync could not do its job: an unknown flag or a stray argument, missing configuration, a refused write |

A path the scan could not read is exit 1 for every command that scans, except
`scan`, which exits 1 for it only with `--fail-on-risk`, and `optimize`, which
refuses with 2 before spending anything.

What counts as exit 1, by command:

| Command | Exit 1 when |
|---|---|
| `init` | a config file already exists, no candidate repo was found, or the user stopped at a prompt |
| `scan` | never by itself; with `--fail-on-risk`, any at-risk file or unreadable path |
| `import` | a conflicting capture, an unreadable path, or (with `--fail-on-pending`) anything uncaptured |
| `link` | a blocked project, a broken or mismatched link, or an unreadable path |
| `doctor` | a broken or mismatched link, a stale capture, an orphaned capture directory, an override that cannot run, or a project path the scan could not read (a capture or settings file it could not judge is exit 2) |
| `reconcile` | a refused selector, a conflict, a diverged or dereferenced link, a removable link left in place, or a path the scan could not read |
| `inventory` | a path could not be read or classified (the rest is still written, checked or printed); with `--check`, the file is stale or absent |
| `optimize` | an agent's advice is unresolved, or it was skipped by the cost cap or a stopped run; with `--apply`, a change was refused (a foreign link, a dereferenced link, a read-only or changed file, or frontmatter that cannot be edited safely); a path the scan could not read is exit 2 |
| `override` | never |

One exception: the hidden `agentsync hook claude-agent`, which Claude Code runs
before each subagent call, never exits 2. Claude Code reads exit 2 from that
hook as "block this call", so the hook exits 1 on any failure, and the stored
command ends in `|| exit 1` so an agentsync too old to have the hook cannot
exit 2 either. The subagent then runs on its own settings. See
[override](override.md).

`doctor`'s exit code is the one that asserts nothing has drifted; `reconcile`'s
is deliberately narrower (see [reconcile](reconcile.md)). Tests assert these
codes, so they are stable.
