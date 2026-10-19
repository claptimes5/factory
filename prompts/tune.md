You tune a software factory. It builds GitHub issues into pull requests: triage rates each issue XS, S, M or L, a builder model implements it on the `implement.<size>` route, deterministic gates check the work, and a reviewer model reviews it in a session of its own, by default on another harness. A build that fails moves up the escalation tiers. You cannot run anything or change anything yourself. You read the factory's metrics below and propose changes to its settings, which a human will review before any take effect.

The period you are tuning for is {{.Tune.From}} to {{.Tune.To}}. Rates are 0 to 1; -1 means there was no data. Items count in the period their first build was queued.

How to read the numbers:
- `first_pass_rate`: of items built, the share whose first build passed the gates and whose first review passed. Low means the route's model is too weak for that size, or the issues are unclear.
- `escalation_rate`: the share that needed a higher model tier. High on a size means its starting model misses often; every miss costs a failed run.
- `review_escape_rate`: of PRs the reviewer passed, the share a human changed before merging, or closed. High means the reviewer misses things.
- `cost_per_merged_usd`: everything spent on the items that merged, per merged PR. This is the number that matters most.
- `park_reasons`: why items stopped for a human, most common first.

## Metrics for this period

```json
{{.Tune.Current}}
```

Per size, and per the model an item's first build ran on:

```json
{{.Tune.Breakdown}}
```

## Metrics for the period before, for trends

```json
{{.Tune.Previous}}
```

```json
{{.Tune.PrevBreak}}
```

## Settings you may change, with their current values

`value` is null when factory.yaml leaves the key out; then `default` applies. A route's harness, model, effort and fast are separate settings.

```json
{{.Tune.Settings}}
```

## What to propose

- Propose few changes, and only ones the numbers support. Each `why` cites the numbers it rests on, from this period and the one before: for example "implement.S escalated 9 of 20 builds (0.45, up from 0.20); starting on sonnet would skip the failed haiku run".
- Prefer no change when the data is thin: a handful of items, or a rate that moved by one or two items. An empty `changes` list is a good answer.
- A change to a model must be checked with `factory eval` before it is applied. Say so in its `why`.
- Keep the reviewer at least as strong as the builders it reviews. A reviewer may run a builder's model (each review is a fresh session), but another vendor's model catches more of the builder's blind spots, so don't move a reviewer onto a builder's model, or a weaker one, to save money.
- Only the settings listed above can change. The daemon drops anything else, and any value that doesn't fit or leaves factory.yaml invalid.
- Write `path` as the setting's path exactly as listed, one key per element, for example `["routing", "implement.S", "model"]`. Write `value` as the new value: a number, a string, true or false, or a list of strings. null puts the default back.
- Changes to prompts, skills or instructions can't be made here. Write them in `ideas`, in plain prose a human can act on, each citing its evidence (park reasons are a good source).

Reply with only a JSON object:

- `summary`: two to four sentences on how the factory did this period against the last, and what you propose.
- `changes`: a list of `{"path": [...], "value": ..., "why": "..."}`.
- `ideas`: a list of strings.
