# Exit codes

| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | the run completed and found something a human must settle |
| `2` | agentsync could not do its job: bad flags, missing configuration, an unreadable path, a refused write |

What counts as exit 1, by command:

| Command | Exit 1 when |
|---|---|
| `scan` | never by itself; with `--fail-on-risk`, any at-risk file |
| `import` | a conflicting capture, an unreadable path, or (with `--fail-on-pending`) anything uncaptured |
| `link` | a blocked project, a broken or mismatched link, or an unreadable path |
| `doctor` | a broken or mismatched link, a stale capture, an orphaned capture directory, or an unreadable path |
| `reconcile` | a refused selector, a conflict, a diverged or dereferenced link, or a removable link left in place |
| `inventory --check` | the file is stale or absent |

`doctor`'s exit code is the one that asserts nothing has drifted; `reconcile`'s
is deliberately narrower (see [reconcile](reconcile.md)). Tests assert these
codes, so they are stable.
