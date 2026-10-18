# init

`agentsync init` writes the [config file](configuration.md): it finds your
canonical agents repo, suggests a default tree of projects, and writes both to
`~/.config/agentsync/config.toml` (or under `$XDG_CONFIG_HOME`). It is the
command every other command names when nothing is configured.

```bash
agentsync init                                              # in a terminal: walks you through it
agentsync init                                              # elsewhere: lists candidates and prints the command
agentsync init --repo ~/src/my-agents --root ~/src          # dry run: shows the exact file
agentsync init --repo ~/src/my-agents --root ~/src --apply  # writes it
```

| Flag | Meaning |
|---|---|
| `--repo PATH` | the canonical agents repo to write; must be a directory holding `agents/` |
| `--root PATH` | the default tree to write; must be a directory. Defaults to the repo's parent in a dry run |
| `--apply` | write the file; needs both `--repo` and `--root` |
| `--force` | replace an existing config file |
| `--search PATH` | where to look for agent repos when `--repo` is not given (default `~`) |

Paths must be absolute, `~`, or start with `~/`. A value under `$HOME` is written as
`~/…`, so the file works on another machine with the same layout.

## In a terminal

When stdin and stdout are both terminals and neither `--repo` nor `--root` is
given, `init` asks (`/dev/null` does not count as a terminal):

```
$ agentsync init
Looking for agent repos under ~ (depth 4, at most 50000 directories)...
  1) ~/src/my-agents  33 agents
  2) ~/old/agents-backup  4 agents
Repo [1-2, or a path; q to quit]: 1
Default tree for --all and inventory [~/src; q to quit]:
Will write ~/.config/agentsync/config.toml:
  repo = "~/src/my-agents"
  root = "~/src"
Write it? [y/N]: y
Wrote ~/.config/agentsync/config.toml
Next, inside a project:
  agentsync scan --at-risk
  agentsync link --agent <name> --apply
```

Pick a repo by number or type a path. Press Enter at the second prompt to
accept the suggested tree, or type another. Nothing is written until you answer
`y`; any other answer, `q` at any prompt, or end of input stops with exit
1 and writes nothing. An invalid answer explains why and asks again.

## Anywhere else

Scripts, pipes and agents never get a prompt. Without `--repo`, `init` lists
what it found and prints the exact command for the best candidate:

```
$ agentsync init
Looking for agent repos under ~ (depth 4, at most 50000 directories)...
  - ~/src/my-agents  33 agents

Nothing written. To use the first, run:
  agentsync init --repo ~/src/my-agents --root ~/src --apply
```

With `--repo` it shows the file it would write, and writes it only with
`--apply`. `--apply` without both `--repo` and `--root` is refused with exit 2,
so nothing reaches the file that you did not name.

## What counts as an agent repo

The search walks `--search` (default `$HOME`) four levels deep. A candidate is a
directory with a `.git/` directory and an `agents/` directory holding at least
one `*.md` file whose frontmatter has `name:` and `description:`. Candidates are
listed by agent count, most first.

The search skips dot-directories, `node_modules`, `vendor` and `Library`, and
never follows a symlink below the search root. A search root that is itself a
symlink, such as a symlinked `$HOME`, is resolved once, and results are shown
under the path you gave. A directory it cannot read, including a search root
that is missing or not a directory, is listed under "Could not read" rather
than skipped silently. It stops after 50,000 directories and says so; pass
`--search` to look somewhere narrower.

On macOS the home search may make the system ask whether your terminal can
read folders such as Documents or Desktop. A denied folder is listed under
"Could not read", and `--search PATH` narrows the walk to where your repos are.

## Rules

- **An existing config file is refused** with exit 1 and its contents shown,
  before any prompt. `--force` replaces it.
- **Everything is checked before writing.** A repo that is missing, not a
  directory, or has no `agents/`, or a root that is missing or not a directory,
  exits 2 and writes nothing.
- **The write is atomic.** The file is created with mode `0600` (its directory
  `0700` if missing) from a temporary file beside it, so a reader never sees a
  half-written file, and a file that appears meanwhile is not overwritten
  without `--force`.

## Exit codes

| Code | When |
|---|---|
| `0` | wrote the file, showed a dry run, or listed candidates |
| `1` | a config file already exists, no candidate was found, or you stopped at a prompt |
| `2` | bad flags, an invalid repo or root, `--apply` without both values, or a failed write |
