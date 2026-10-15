# inventory

`agentsync inventory` regenerates `inventory.json`: one file naming every agent
on the machine. That covers the global tier at `~/.claude/agents`, every
project's agents by project key, and plugin agents agentsync does not manage.
By default it is written to `<repo>/inventory.json`.

```bash
agentsync inventory                 # regenerate
agentsync inventory --check         # exit 1 if the file on disk is stale
agentsync inventory --stdout        # print, leave the file alone
agentsync inventory --out ./inv.json
```

| Flag | Meaning |
|---|---|
| `--out PATH` | write here instead of `<repo>/inventory.json` |
| `--stdout` | write the document to stdout and leave the file alone |
| `--check` | write nothing; exit 1 if the file is not what a regeneration would produce |

`inventory` takes no `--apply`. It only ever rewrites its own derived file,
recomputed from scratch and replaced atomically, so re-running is always safe.
It never derives its root from the working directory; see
[configuration](configuration.md).

## Derived, never authoritative

The filesystem is the source of truth. Nothing decides from `inventory.json`:
`import`, `link`, and `reconcile` re-scan before writing. Read it for
orientation, regenerate it to fix any inconsistency. This is the opposite of the
[state file](state-file.md).

## Stable output

The header carries a schema number and nothing volatile: no timestamp, no tool
version. Regenerating with no change in the tree produces no diff, so committing
the file gives `git log` a history of when a project gained an agent, and
`--check` works as a freshness test in CI. Paths are keys: repo-relative first,
then `~/`-prefixed, absolute only when neither applies. The file is pretty-printed.

## Plugin counts

Plugin agents are counted from the files that decide what loads, not from every
`agents/*.md` under `~/.claude/plugins` (which overcounts, since it includes
every available plugin and leftover cache directories).

- **Installed:** `~/.claude/plugins/installed_plugins.json`. The cache walk is
  narrowed to directories it lists.
- **Enabled:** `enabledPlugins` in `~/.claude/settings.json`, keyed
  `<plugin>@<marketplace>`. A plugin absent from that map counts as disabled.

Unknowns resolve downward: a missing `settings.json` means nothing enabled, a
missing registry means nothing installed. The second reads like a machine with
no plugins, so `inventory` warns on stderr when the registry is absent but the
cache is not.

The summary reports `plugin_agents_installed` and `plugin_agents_enabled`;
quote the second as the cost of a session. `plugins_shipping_agents` and
`plugins_with_agents_enabled` count only plugins that ship at least one agent.
