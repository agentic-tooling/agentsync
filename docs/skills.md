# Project skills

A Claude project skill lives at `.claude/skills/<name>/` and has a regular
`SKILL.md`. The directory can also hold scripts, templates, and other support
files. Agent Sync compares a SHA-256 digest of the sorted relative paths and
file bytes, including nested files and empty directories. Captures preserve
executable bits. A nested symlink, special file, or unreadable tree prevents a
capture.

Put shared skills in `skills/<name>/` in the canonical checkout, or set a
different checkout-relative `skills_root`. A captured skill in
`project-skills/<project-key>/<name>/` takes precedence for that project. The
project key is the same one Agent Sync uses for agents. [Configuration](configuration.md)

## Link a canonical skill

Run from inside the project. Named skills are selected independently of agent
files, even when they share a name.

```bash
agentsync link --skill release-check --skill csv-format
agentsync link --skill release-check --skill csv-format --exclude-local --apply
```

Use `--all --project <key>` to select a project from the default tree. An
existing real directory, including one with matching content, is reported as
occupied. A healthy link to the selected counterpart reports a match. Broken,
foreign, or misdirected links stay in place for manual resolution.

The Git gate refuses a path with an index or HEAD entry at or below it, or a
tracked ancestor file. If the skills directory contains tracked siblings,
`--exclude-local` allows a new symlink only after Git confirms that the
specific link path is ignored. Agent Sync adds a path-specific rule to
`info/exclude` when needed. In an otherwise visible skills directory,
`--exclude-local` keeps the link local.

## Capture a project skill

```bash
agentsync import --skill release-check
agentsync import --skill release-check --apply
```

The apply captures a complete directory under `project-skills/`. The source
stays in the project. Repeating the import reports matching content. If the
project capture already holds different bytes, review the differences and
resolve them manually. Use `--all` to select skills by name throughout the
default tree, or `--fail-on-pending` to check whether a preview would write a
capture.

## Check existing skills

`scan`, `doctor`, and `reconcile` report linked, broken, foreign, local-only,
canonical matches, and content drift. Their JSON includes `skills` and
`skill_problems` fields when applicable. With `--fail-on-risk`, scan returns 1
for local-only skills, broken canonical links, drift, or an unclassifiable skill.
Doctor and reconcile report those findings for manual resolution. Reconcile
keeps skill directories and links in place, including with `--apply`.
