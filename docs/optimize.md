# optimize

`agentsync optimize` asks an OpenRouter decision model which Claude model and
reasoning effort each of a project's Claude agents should run on, and reports
the advice. With `--apply` it writes the suggested changes into the agent files
(see [Applying](#applying)). It is the only agentsync command that uses the
network.

```bash
export OPENROUTER_API_KEY=...          # required
agentsync optimize                     # the project you are in
agentsync optimize --agent go-reviewer # one agent
agentsync optimize --advisor cloudflare/clef --max-cost 0.05
agentsync optimize --json
agentsync optimize --apply             # write the suggested changes
```

Use a dedicated OpenRouter key with a credit limit set on OpenRouter's side.
That limit is the backstop if anything here undercounts.

## What it reads and sends

The agents are the ones [scan](scan.md) finds under the root (the project's
`.claude/agents/`, linked or local); root and repo resolve as for every other
command (see [configuration](configuration.md)). An agent file linked into
several projects is asked about once. A `.md` file without `name` and
`description` frontmatter is not an agent and is skipped.

For each agent it shows `model:` and `effort:` from the frontmatter as written
(without quotes or a trailing `# comment`), or `(not set)`. If `$CLAUDE_CODE_SUBAGENT_MODEL` or `$CLAUDE_CODE_EFFORT_LEVEL`
is set, it notes that Claude Code uses it over the frontmatter at run time.
It also notes each agent asked about that has a project [override](override.md);
these notes are not in the JSON.

Each agent costs one `POST /api/alpha/decisions` request, with no retries. The
request holds one framing sentence, the first 24 KB of the agent file and two
questions:

- `model`: a choice among the Claude Code aliases `haiku`, `sonnet`, `opus` and
  `fable`, each with a one-line description. An alias is offered only if
  OpenRouter's public models list has an `anthropic/claude-<alias>-<version>`
  model. Claude Code resolves an alias to the newest model in its family.
- `effort`: a score over `low`, `medium`, `high`, `xhigh`, `max`, read as the
  nearest level. Its confidence is the probability the advisor gave that level,
  not the score's own `confidence` field.

Nothing else from the project is sent.

## Flags

| Flag | Meaning |
|---|---|
| `--advisor MODEL` | the decision model to ask; default `~typesafe/jev-latest`, and `cloudflare/clef` works too |
| `--max-cost USD` | the most the run may spend; default `0.50` |
| `--min-confidence P` | the probability a pick needs before it is suggested; default `0.6` |
| `--agent NAME` | ask only about this agent; repeatable |
| `--apply` | write each suggested change; without it the run is a dry run |
| `--json` | print the report as JSON on stdout (see below), with the Repo, Root and override notes on stderr |

## Reading the report

```
AGENT              NOW           SUGGESTED              CONFIDENCE
go-merge-reviewer  opus / xhigh  keep (low confidence)  sonnet 0.57 / xhigh 0.89
```

Model and effort are judged separately. A part is suggested only when the
advisor's pick differs from the current value and its probability is at least
`--min-confidence`. A differing pick under the threshold is dropped: a row that
still changes the other part names it, as in `opus / xhigh (effort: low
confidence)`, and a row with nothing left to change says `keep (low
confidence)`. `CONFIDENCE` shows both picks whether or not they were suggested.

A row is `unresolved` when the request failed (OpenRouter's error message is
shown, including a guardrail's reason and settings link), the response was not
valid JSON, an answer or its probability was missing, or the advisor picked
something it was not offered. An unresolved row suggests nothing.

## JSON

`--json` prints one object: `advisor`, `rows`, `spent_usd`, and `overrides`
(the `$CLAUDE_CODE_*` variables that are set; omitted when none). Each row has:

| Field | Meaning |
|---|---|
| `agent`, `path` | the agent's label and file |
| `model`, `effort` | the current values, or `(not set)` |
| `status` | `change`, `keep`, `unresolved` or `skipped` |
| `suggested_model`, `suggested_effort` | set only for a part that changes |
| `model_pick`, `effort_pick` | `{value, probability}` as the advisor answered; absent unless both were read |
| `model_low_confidence`, `effort_low_confidence` | `true` when that part's differing pick was dropped; omitted otherwise |
| `reason` | why a row is unresolved or skipped |
| `cost_usd` | the `usage.cost` OpenRouter reported for this row |
| `applied` | `true` when `--apply` wrote this row; omitted otherwise |
| `apply_error` | why `--apply` did not write this `change` row; omitted otherwise |

With `--json`, the lines `--apply` prints go to stderr with the other notes.

## Applying

Without `--apply` nothing is written, and the last line says how many changes
`--apply` would write. With it, each `change` row is written; `keep`,
`unresolved` and `skipped` rows, and any part dropped for low confidence, are
not. There is no prompt: `--apply` is the write boundary, as for `link` and
`reconcile`.

- Only the suggested parts change. An existing top-level `model:` or `effort:`
  line keeps its spacing, its quotes and any trailing `# comment`; a missing
  key is added just before the closing `---`, with the file's line ending.
  Every other byte, including the body, is unchanged.
- The file written is the one the agent resolves to. For a linked agent that is
  the canonical repo file, so every project linked to it gets the change; the
  link stays a link. The report says `wrote <path> (canonical; linked into N
  projects)`, counting the link records in `.agentsync-state.json`, or
  `(canonical file)` when there are none. A project-local agent file is edited
  in place.
- agentsync never commits. When a written file is in the canonical repo, the
  run ends by saying the changes there are not committed.
- A project [override](override.md) still wins over the written frontmatter in
  its project, and the run notes each one that sets a part it wrote.
- Each file is written through a temp file in its own directory and renamed
  over it, keeping its mode.

A `change` row is refused, and the run exits 1, when:

- the agent is a link to a file outside the canonical repo (a foreign link,
  which agentsync never touches);
- it is a regular file where `.agentsync-state.json` records an agentsync link,
  a dereferenced copy: run `agentsync link --apply` first;
- the file is read-only (its owner write bit is clear);
- the file changed since the advice was read: run optimize again;
- its frontmatter is not simple `key: value` lines: no closing `---`, `model:`
  or `effort:` set twice or spelled another way (`model :`, `"model":`), or a
  block, flow, anchor, alias, tag, multi-line or partly quoted value.

A file that cannot be read or written is reported and the run exits 2. So is
every regular (unlinked) agent file while `.agentsync-state.json` cannot be
read, since a dereferenced copy cannot be told apart then. Either way the
other rows are still written.

## Cost

The advisor's input price comes from OpenRouter's public models list; an
advisor that is not listed, or lists no fixed price, is refused before any
request. Before each request the cost is estimated as request bytes / 4 times
that price, or at the highest cost per request byte OpenRouter has reported so
far in the run if that is higher. The request is skipped if the estimate would
take the spend so far past `--max-cost`. Spend so far is the sum of the
`usage.cost` OpenRouter reports on each response.

The run stops, and the remaining agents are skipped, whenever a request may
have been billed without a usable cost: a response with no or a negative
`usage.cost`, a successful response that was cut short or was not valid JSON,
or a connection error or timeout after sending. The row it happened on is
unresolved. An error status from OpenRouter is not billed and does not stop
the run. The last line prints the total reported spend.

A model's billed price can differ from its listed one. The estimate learns
the billed rate from each response, so it is mostly the first request that
can be underestimated. The cap still holds to within one request, because
spend is what OpenRouter reports.

## Exit codes

- `0`: every agent got advice, and with `--apply` every change was written.
- `1`: a row is unresolved or skipped, or `--apply` refused a change.
- `2`: no `$OPENROUTER_API_KEY`, bad flags, no repo or root configured, a scan
  that could not read everything, an agent file that could not be read, or a
  models list that is unreachable, has no fixed price for the advisor, or has
  no Claude family; or, with `--apply`, an agent file that could not be read or
  written, or a regular agent file while the state file could not be read.

See [exit codes](exit-codes.md).
