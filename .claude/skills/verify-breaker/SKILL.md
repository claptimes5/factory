---
name: verify-breaker
description: Use when a change touches auto-merge or its breaker (internal/pipeline/guard.go, autoMerge/autoMerged/cancelAutoMerge in stages.go, TripBreaker in store/guard.go), or when check.sh --sync fails. Model-checks, over every order of events, that a tripped breaker leaves no PR merging itself unnoticed.
---

# The auto-merge breaker, model-checked

Playbook Step 7b. A TLA+ model of auto-merge's breaker, checked by TLC
over every order of events within the bounds below.

**Property.** Once a repo's auto-merge breaker trips, the factory never
turns auto-merge on there again, and every PR it had set to merge itself
is turned off, is still being turned off, or the maintainer was told to
turn it off.

**Harm if broken.** An unreviewed change merges to main after the factory
decided, from its own reverts and red builds, to stop trusting itself.

**Why a model checker.** The breaker races PR starts on several
goroutines, holds a lock across forge calls, and the daemon can stop
between any two steps. Tests drive one order at a time; TLC tries them all.

## Run it

```sh
.claude/skills/verify-breaker/check.sh            # the property holds: "No error has been found"
.claude/skills/verify-breaker/check.sh --sync     # the Go below is what the model was checked against
```

About a second. TLC is `tla2tools.jar` v1.7.4, pinned by SHA-256 in
`check.sh` and downloaded once to `~/.cache/tla`; it needs Java 11 or later.
`scripts/verify.sh` (and so CI) runs `--sync`, not TLC.

| Config | Bounds | Result |
|---|---|---|
| `Breaker` | 2 PRs, the daemon may stop once | holds: 3,116 states, depth 19 (3 PRs and 2 stops checked once: 85,724 states, holds) |
| `BreakerBroken` | `recordMergeOn`'s breaker check deleted (guard.go:902-904), no stops | fails: the breaker trips, then a PR records and turns auto-merge on after it. The negative control |

## Finding (2026-11-05, fixed)

A daemon stop after `checkBreaker` recorded the trip (`TripBreaker`,
guard.go:562) and before its hand-backs reached a PR left that PR set to
merge itself: after the restart, `checkBreaker` saw the breaker already
tripped and returned (guard.go:536-540), so nothing turned it off and its
alert never went out. TLC's trace, with one stop allowed: p2 set to merge
itself; breaker trips; daemon stops; from then on only `BAlready`.

Fixed: `Recover` calls `finishTrips`, which runs the hand-backs again for
each tripped repo and tells you when it found one
(`TestRestartFinishesABreakerTripCutShort`). The model's `Crash` does the
same, and `Breaker` now allows a stop. With the old `Crash` it fails again.

## Coverage record

- **Sync commit:** the fix's commit. `covered.sha256` holds a hash of
  each Go function the model stands for, as of then: `checkBreaker`,
  `finishTrips`, `handBackTripped`, `guardLock`, `breakerTripped`,
  `handBack`, `recordMergeOn`, `startMerge`, `enableAt`, `resync`
  (guard.go); `autoMerge`, `autoMerged`, `cancelAutoMerge` (stages.go);
  `TripBreaker` (store/guard.go); `Recover` (pipeline.go).
- **When `--sync` fails:** read the changed function against the actions
  in `Breaker.tla` (each names its Go lines), update the model and its
  line numbers if the behavior changed, run `check.sh`, then
  `check.sh --record`, and say in the PR that you did. Don't record
  without reading: the hashes only say the code moved, not that the model
  still fits.

## What it doesn't cover

A pass covers the model within its bounds, not the system:

- Two PRs and one daemon stop (three and two checked once), one repo. A
  workspace's repos share the breaker the same way and aren't modelled
  separately.
- The forge is trusted: a successful `CancelAutoMerge` stops the merge,
  including for a PR in a merge queue (`QueueMerge`), which isn't modelled
  apart. A merge the forge already started can still land; the property
  is about what's left on, not about racing the forge.
- `autoMerged` reads the event log; the model keeps one record per PR, and
  doesn't model the store failing (`autoMerged` then says on, which only
  makes cancelling more likely).
- The restart's hand-backs run before any start can (the model holds the
  lock for them): `Recover` runs before the first `Tick`.
- Starts cut short by a stop aren't retried in the model. Their retries
  (`followMerge`, `enableAt`) go through the same lock and check.
- It assumes `openPR` moves an item to awaiting_human before `startMerge`
  (stages.go:1155, 1259): the breaker hands back only items there. `openPR`
  isn't in `covered.sha256` (it changes often), so check this by hand.
- The breaker's cool-off and reset, strike counting, `no_guard` and a
  threshold of 0 aren't modelled.
