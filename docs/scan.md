# scan

`agentsync scan` classifies every project agent file against the canonical
repo. It is read-only.

```bash
agentsync scan                  # the project you are in
agentsync scan --all            # every project under the default tree
agentsync scan --at-risk        # only files protected nowhere else
agentsync scan --all --fail-on-risk   # exit 1 on any at-risk file or unread path (CI)
```

An entry is "at risk" when its content exists nowhere else: no copy in the
canonical repo and not committed in its own project's git repository. Those are
the files `import` captures and the reason `link` refuses a project until they
are captured.

| Flag | Meaning |
|---|---|
| `--at-risk` | list only entries whose content is protected nowhere |
| `--fail-on-risk` | exit 1 if any at-risk entry is found or a path could not be read or classified |

`scan` exits 0 even when it reports at-risk files, because its exit code is a
report. Use `--fail-on-risk` when you want an assertion. A path the scan could
not read or classify is reported and qualifies the summary; it changes the exit
code only with `--fail-on-risk`, which exits 1 for it, as it does with `--json`.

An agent with an [override](override.md) in its project's
`.claude/settings.local.json` shows it on its line, as `override: sonnet /
medium` (`-` for a part the override leaves alone). A settings file that cannot
be read is a warning on stderr. The JSON output does not include overrides.

Run it from the project directory; `--all` scans the whole default tree instead.
Root selection is covered in [configuration](configuration.md).
