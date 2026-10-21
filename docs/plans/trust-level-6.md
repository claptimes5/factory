# Plan: trust level 6, the factory picks its own work

Status: built, with the owner's answers to the decisions below (all three as recommended). Follows [the design system plan](design-system.md),
phase 4 (Trust), and docs/using.md, "Trust".

## Where things stand

The factory already finds work on its own, but what happens next differs
by who found it:

| Who files the issue | Today |
|---|---|
| Main health (a failing or flaky check on main) | Starts at once (unless `main_health.require_approval`), first in the queue; never merges itself: a person merges it (`healthApproved`, stages.go) |
| Sources: Dependabot, TODOs, Sentry | Filed with the source's label, within `sources.daily_issue_cap`; waits for `factory:ready` |
| The old-bug hunt | Same as sources |
| Grooming | Proposes splits and closures; a person confirms |
| The orchestrator (`create_issue`) | Opens issues *with* `factory:ready`, so they start; they may merge themselves under auto-merge like any item |

So levels 1 to 5 govern merging, but starting work isn't on the ladder,
and the orchestrator's issues skip it.

## What level 6 means

At level 6, a repo's own findings start without you, and the work merges
itself under level 5's rules. Below level 6, the factory still finds and
files them, and they wait for you, as today.

1. **Self-started work.** When a repo is at level 6, an issue the factory
   files from an allowed source gets `factory:ready` and is approved by
   "the factory (*source*)", like main health's are now. It's marked as
   self-started on the item, in Home's line and in History.
2. **Merging.** Self-started items merge themselves under the repo's
   auto_merge (level 5's: up to medium risk, size M) only at level 6.
   Below it they wait for a person to merge, as main health's do today.
   The always-asks list and high risk still wait, at every level.
3. **The orchestrator joins the ladder.** The issues it opens on its own
   schedule count as self-started: below level 6 they still start (it's
   opt-in and has its own budget), but wait for a person to merge. Issues
   drafted in Ask or the chat aren't: a person pressed Create issue.
4. **Limits.** Per repo, a daily cap on self-started issues (default 3)
   and dollars a day for self-started work (default a fifth of
   `budgets.daily_total`), on top of the existing caps. Over either, they
   wait for your label as below level 6.
5. **Which sources.** `self_start.sources`, safest first: main health and
   Dependabot by default; TODOs, Sentry and the hunt opt-in.

Config (a repo's own, like auto_merge; the Trust screen writes it):

```yaml
repos:
  - name: acme/api
    auto_merge: {up_to: medium, max_size: M}
    self_start:
      sources: [main_health, dependabot]   # also: todos, sentry, hunt, orchestrator
      daily: 3            # issues a day
      budget: 12          # dollars a day; default daily_total / 5
```

## Earned like the others

Level 6's practice run is over what the factory filed itself in the last
30 days, from the allowed sources:

- **Accepted:** how many a person labelled to build, against how many
  were closed or left; a rate of 80% or more.
- **Clean:** of those built and merged, none reverted or broke main.
- **Enough:** at least 10 accepted.

The Trust screen shows these, and the factory suggests level 6 only when
the repo is at level 5 and all three hold. A person raises it, as for the
other levels. Stepping back is the same: the breaker turns auto-merge off
(level 1), which also stops self-starting until it's reset.

## Building it

1. Config: `self_start` per repo, validated; `Allows(source)`.
2. Pipeline: one path for "the factory filed this; start it?" used by
   sources, the hunt and the orchestrator (main health keeps its own
   priority); the caps; the self-started mark on the item.
3. Merging: generalise `healthApproved` to "approved by the factory
   itself", and allow those to merge themselves at level 6.
4. Trust: level 6 available, its practice run (acceptance from the
   source issues the store tracks), the suggestion, and the setter.
5. Dashboard: the self-started tag on Home, the line and History; level 6
   on the Trust screen with its sources and limits.
6. Docs and tests: unit tests for each source's path and the caps; e2e
   for the Trust screen at level 6.

## Decisions for the owner

1. The orchestrator's issues waiting for a person to merge below level 6
   changes today's behavior for anyone with auto-merge and the
   orchestrator on. Recommended: yes, so the ladder means what it says.
2. Default sources at level 6: main health and Dependabot only?
3. Defaults for the limits: 3 issues and a fifth of the daily budget a
   day per repo?
