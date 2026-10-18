# scan

`agentsync scan` classifies every project agent file against the canonical
repo. It is read-only.

```bash
agentsync scan                  # the project you are in
agentsync scan --all            # every project under the default tree
agentsync scan --at-risk        # only files protected nowhere else
agentsync scan --all --fail-on-risk   # exit 1 if any at-risk file exists (CI)
```

An entry is "at risk" when its content exists nowhere else: no copy in the
canonical repo and not committed in its own project's git repository. Those are
the files `import` captures and the reason `link` refuses a project until they
are captured.

| Flag | Meaning |
|---|---|
| `--at-risk` | list only entries whose content is protected nowhere |
| `--fail-on-risk` | exit 1 if any at-risk entry is found |

`scan` exits 0 even when it reports at-risk files, because its exit code is a
report. Use `--fail-on-risk` when you want an assertion. A path the scan could
not read or classify is reported and qualifies the summary, but does not
change the exit code.

Root selection and `--all` are covered in [configuration](configuration.md).
