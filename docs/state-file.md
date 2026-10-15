# State file

`link --apply` records what it does in `.agentsync-state.json` at the root of
the canonical repo. It is the only thing that separates links agentsync created
from ones made by hand, and nothing is removed without a record.

It names links by absolute path, so it is meaningless in any other checkout.
Keep it out of git. It is also not regenerable from the filesystem: unlike
`inventory.json`, it records what the tool did, and a stale or missing one
limits what `reconcile` will do rather than being fixed by re-running anything.

Top-level fields are `schema`, `links`, and `declined`.

| Field | In | Holds |
|---|---|---|
| `path` | link record | absolute path of the symlink |
| `target` | link record | what it pointed at |
| `hash` | link record | content hash of the target at link time |
| `origin` | link record | `created` by `link`, or `adopted` by `reconcile --adopt` |
| `recorded_at` | link record | when it was recorded |
| `project`, `name` | declined marker | the agent that was deliberately removed |
| `declined_at`, `observation` | declined marker | when, and what was observed |

Who reads and writes it:

- [link](link.md) writes `created` records, and reads declined markers so it
  does not recreate a deleted agent. It never writes a marker.
- [reconcile](reconcile.md) writes `adopted` records and declined markers, reads
  everything, and drops a record after removing its link.
- `link --undecline` clears a marker.
- [doctor](doctor.md) does not read or write it.
