# Quality

This guide covers how the factory decides a change is good enough to show
you: the gates, how review scales with risk, the feature map, how ready a
repo is, and how the factory measures itself.

## The gates

After the builder stops, the daemon runs deterministic gates in the job
container. They run on an export of the builder's commit, never the
builder's own checkout (see [Isolation](security.md#isolation)). In this
repo's `factory.yaml`:

| Gate | What it checks |
| --- | --- |
| `install` (optional) | Runs first, with the network on. If it fails, nothing else runs. |
| Diff guard | The change stays under `max_changed_lines` and touches no `protected_paths`: the factory's own controls and files that steer agents. |
| Required checks | Each name in `gates.required` runs its command from `gates.commands`: here `types` (`go vet`), `lint` (`golangci-lint`), `test` (`go test -race`) and `build`. A required gate with no command is misconfigured and fails. |
| [Proves-fix](#test-proves-fix) | The new tests must fail on the old code. |
| [Coverage floor](#coverage-floor) | Touched packages can't end up less covered. |
| [Feature checks](#feature-map) | The `check` of each feature the change touches. |
| [Hidden scenarios](#hidden-scenarios) | After every other gate passes, if `gates.scenarios_dir` is set. |

Each repo can override the gates (see
[Several repositories](configuration.md#several-repositories) and
[Each repo's toolchain and checks](configuration.md#each-repos-toolchain-and-checks)).
Set `proves_fix: none`, `coverage: none` or `scenarios_dir: none` to turn
one off for a repo.

Then the reviewer reads the issue, the diff and the gate results. Before
a finding blocks, a second, cheaper model checks it (see
[Review findings](#review-findings)). A block sends the findings back to
the builder, for at most `max_review_rounds` fix rounds (2); a third block
parks the item. A malformed review retries once, then parks, so a broken
reviewer never consumes a fix round or passes code.

## Test proves fix

`gates.proves_fix` reverts the code, keeps the changed tests, and runs
them. They must fail. `{pkgs}` becomes the packages holding changed tests,
and an empty command turns the check off:

```yaml
gates:
  proves_fix: go test -count=1 {pkgs}
```

A compile failure counts as a failing test. A new test that calls a
function the change adds fails to build on the old code, which is the
expected test-first shape. The proves-fix check is Go-only for now.

## Coverage floor

`gates.coverage` runs, with coverage, the tests of every Go package whose
code the change touches, on the base and on the change. A package can't
end up less covered than on the base.

```yaml
gates:
  coverage: go test -count=1 -cover {pkgs}
  coverage_slack: 0.5
```

- **`coverage_slack`** allows a drop of that many points
  (`coverage_slack: 1` allows one point).
- **Packages compare by directory,** so renaming the module doesn't reset
  them.
- **A new package has no floor,** but it must have some tests, unless it's
  a command (`package main`).
- **A package the base can't measure,** because its tests fail, has no
  floor either.
- **Some code doesn't count:** vendored code, generated files
  (`// Code generated … DO NOT EDIT.`), files build tags leave out, and
  nested modules.
- **The output it reads** is Go's `coverage: N% of statements` lines. A
  script of your own can print `dir N%` lines instead.

The builder is told the floor exists. The coverage check is Go-only for
now.

## Text-only changes

Triage rates a change `XS` when it touches only text: copy, docs,
comments, translations or config values, with no change to how the code
behaves.

- An `XS` item builds on `routing.implement.XS` (Haiku by default, else
  `implement.S`), with its own budget (`budgets.per_job.XS`, else S's).
- The builder is told it needs no new test.
- The reviewer is told to block anything that turns out to change behavior
  without one.
- Every gate still runs. On Go code the proves-fix check still asks for a
  test, and the coverage floor skips text.
- An XS change that triage rates high risk keeps the usual test-first rule.

In a repo without those checks, the reviewer is what catches a "text"
issue that is really a logic change.

## Review scaled by risk

Every item has a risk. Triage rates it `low` or `high`, and a
[feature map](#feature-map) can raise it to `medium` or `high`. Review gets
stricter with it, and the PR tells you how much of your attention it
needs:

| Risk | Reviewers | The PR says |
| --- | --- | --- |
| low | `review` | a skim of the diff is enough |
| medium | `review.medium_risk` if set, else `review` | read the changes to the features it touches |
| high | `review.high_risk`, then a second critic (`routing.critic`, else `implement.L`) | read the whole diff, and check the features it touches by hand |

The reviewer is told the risk and holds a high-risk change to a higher
bar. Each PR carries a `factory:risk-low|medium|high` label (filter on it
to find the quick ones) and a "How to review" section that says why it got
its risk.

Merging is yours at every level. To make GitHub enforce it, give the paths
behind your high-risk features a CODEOWNERS entry and require code owner
review on the base branch.

## Review findings

Each finding the reviewer (or critic) raises gets an id, `F1`, `F2` and so
on, that it keeps for the rest of the item's rounds. Its severity says
whether it can block:

| Severity | Blocks |
| --- | --- |
| `critical` | Always, whatever the reviewer decided. |
| `major` | When the reviewer decides `block`. |
| `minor` | Only when the reviewer blocks on minor findings alone. |
| `pre-existing` | Never. It's a real problem the change didn't introduce, kept as a note. |

**Verified before it blocks.** Every finding that would block goes to the
verifier (`routing.verify`, a cheap model), with the issue, the diff and
a read-only checkout. It confirms each one or refutes it, with a reason;
it's told to refute only what it can show is wrong. Only confirmed
findings block. A refuted one is kept as a note: the builder sees it if
the round blocks anyway, and the PR lists it under "Reviewer notes". If
the verifier can't run (budget, an error, no ruling on a finding), the
finding blocks: the verifier can only take a block away, never pass code
the reviewer would have stopped.

**Answered in the fix round.** The builder is shown the blocking findings
with their ids and the verifier's reasons, fenced as text to check rather
than instructions, and answers each in its verdict: `fixed`, `wrong` (with
why) or `out_of_scope` (with why). One it leaves out is recorded as
unanswered.

**Ruled on by the next review.** The next round's reviewer sees its open
findings with the builder's answers, also fenced, and rules on each:
`resolved`, `upheld` (it blocks again, with the same id, and is verified
again) or `withdrawn` (the builder was right). It's told not to raise
again what was already raised or refuted. It reviews the diff since the
commit it last reviewed, with the whole change as context when that's
small (see [How review checks itself](configuration.md#how-review-checks-itself)).
A workspace change is reviewed whole each round.

Every finding and how it ended is kept in the database, so the metrics
can say how often a reviewer's findings were worth a round.

## Feature map

A feature map, `features.yaml` at the repo root, lists what the product
does, where each feature lives and what proves it works:

```yaml
features:
  - name: Login
    description: Sign in with email and password
    paths: [app/controllers/sessions_controller.rb, app/views/sessions/, "app/models/user*.rb"]
    tests: [spec/system/login_spec.rb, spec/models/user_spec.rb]
    check: bundle exec rspec spec/system/login_spec.rb   # optional: runs just its tests
    risk: high                                            # optional: low, medium or high
  - name: Billing
    paths: [app/services/billing/]
    tests: []                                             # none yet; say so rather than leave it out
```

A path is a file, a directory (with or without a trailing `/`), or a glob
matched against the whole path. `*` stops at `/`, and there is no `**`, so
use a directory for a tree.

When a change touches a feature, through its code or its tests:

- **Its `check` runs as a gate,** `feature:<name>`, after the required
  checks (once per distinct command). A check that fails on the base
  branch too doesn't block the change if it fails the same way (same exit
  code, after the same install). The PR marks it as failing on the base
  branch.
- **The reviewer and the PR get the blast radius:** each feature touched,
  its risk, the files that touch it and what proves it, plus any changed
  files the map doesn't cover.
- **A feature's `risk` raises the item's.** A change to a `high` feature is
  reviewed as high risk (with the critic), whatever triage said.
- **The builder is told the `high` features** and where they live before
  it starts. It's asked for a property-based test of any rule it changes
  in them, as it is for an issue triage rated high.

The map is read from the base branch, never from the change, so a builder
can't shrink its own blast radius. Builders are asked to keep it up to
date when they add or move a feature; those edits count once merged.

The readiness check reports how much of the code the map covers, entries
that point at nothing, features with no tests, and `high` features whose
tests include no property-based or fuzz test. It files the work to fix
each.

This repo's own map is [`features.yaml`](../features.yaml).

## How ready a repo is

The factory can only prove its changes work as far as the repo's checks
reach. `factory setup` ends by measuring that for each repo, and you can
run it again any time:

```sh
./factory readiness              # every repo; or name one: ./factory readiness you/api
                                 # (flags go before the name)
./factory readiness -file        # also open an issue for each gap
./factory readiness -queue       # open them labeled for the factory to work
```

It clones the base branch, runs the install step and every required check
(the tests twice, to catch flakiness), and looks for tests, end-to-end
tests, a feature map, a lockfile and CI. It prints a score and what's
missing:

```
you/api at main (4f2c1a9): 59/100, not ready yet: fix the blockers first
  ✗ `test` fails on main
  ! No lint check
  ! No feature map
  ✓ Installs from a clean checkout
  ✓ Has end-to-end tests
```

- A **blocker** means every job would fail, or pass for the wrong reason:
  tests that fail on the base branch, flaky tests, no tests at all.
- A **gap** means jobs work but less of what they change is verified.

Each one comes with an issue written for the factory's builder, so the
first thing the factory does on a new repo can be making it verifiable.
While a repo has blockers, `-queue` labels only those for the factory,
since every other job would fail until they're fixed. The gaps are filed
for later, and the next `-queue` run after the blockers are fixed labels
them too.

Issues it files carry `factory:readiness`. Running it again skips a gap
whose issue is still open or was closed as not planned, and files it again
if it was closed as done but came back. (On GitLab, which doesn't record
why an issue was closed, it never files a gap again once its issue is
closed.)

If the check itself can't run (no Docker, a missing job image, a command
that isn't installed, a timeout), it stops there and files nothing. The
report is saved to `data/readiness/<owner>__<name>.json`.

For what a new repo needs to pass, see [new-project.md](new-project.md).

## Hidden scenarios

Set `gates.scenarios_dir` to a directory outside the repository. After
every other gate passes, each of these runs against the commit under test:

- each `name.sh`, run with sh in the checkout;
- each Playwright `name.spec.js|mjs|ts`, with a `playwright.config.*` in
  the same directory.

Builders and reviewers only learn `scenario:name pass|fail`. The full
output stays in `data/jobs/<id>/scenarios/`. Build the job image with
`--build-arg PLAYWRIGHT=1` for browser scenarios.

Hidden scenarios join the job's [services](configuration.md#services-databases-and-the-like)
network, but still reach nothing else. Scenarios that run with no network
still get none.

## Metrics

`/metrics` (and `/metrics.json`) report:

- cost per merged PR by complexity and by the model that landed it;
- first-pass rate and escalation rate;
- reviewer blocks;
- review escapes: passed PRs you changed before merging, or closed;
- reviewer precision: the share of blocking findings that led to a fix
  (not refuted by the verifier, answered `fixed` and not upheld by a
  later review), overall and per reviewer route and model. A finding
  counts once it's settled: refuted, ruled on by a later review, or its
  item has left the build loop;
- the share of checked findings the verifier refuted;
- rounds to pass: the review rounds an item that passed review took, 1
  for a first-round pass;
- hours spent awaiting you.

Escapes say what the reviewer missed; precision says how much of what it
raised was worth a round. The dashboard's Spend screen shows them side by
side, with precision per reviewer, and `factory status` prints them on
its `review` line.

## The replay set

`evals/<name>/case.yaml` freezes a past issue at `base_sha`, and
`evals/<name>/accept/` holds the hidden acceptance files. `factory eval`
runs every case through the real pipeline against a private copy of the
repo and an in-memory GitHub, so nothing is pushed or posted. Then it runs
`accept` on the result. Each case also reports its review rounds and its
blocking findings (and how many the verifier refuted). With `-baseline`, it fails if the candidate config
is worse. See [`evals/README.md`](../evals/README.md) and the
[commands](using.md#commands).

## Failures become cases

When you merge a factory PR only after changing it, the daemon tells you,
because what you changed is what the factory missed.
`factory eval capture PR` turns that PR, or any merged fix, into a case:

- frozen at the commit its branch started from;
- with the issue it closed as the task;
- with its own tests and test data as the acceptance files.

Every later config is then measured against the miss.

## Learning from your edits

The factory learns from the work you correct. It watches two things:

- **Your edits.** When you push to a factory PR and then merge it, the
  factory keeps what your own commits changed after its last push (up to
  20 KB, in whole hunks). What an "Update branch" merge or a rebase brought
  in from the base branch isn't counted, nor the factory's own commits
  rebased.
- **Repeat findings.** Each critical or major finding in a review or critic
  verdict that blocked a build is kept, with its file and claim, once per
  item however often the review is retried.

Once a day per repo, a read-only learn job runs if there's something new: at
least one edit of yours, or `learning.min_findings` alike findings (in
files of the same folder and type, or with claims in the same words) from
two or more items. One runs at a time. It reads them with the repo's current
`AGENTS.md` and `CLAUDE.md`, and proposes up to three lines: general rules a
builder would follow next time, not one fix restated. It may propose none. A
lone finding waits, for up to 30 days, for one like it. It reads your five
newest edits and ten findings per group at most; older ones are set aside.

The lines go under a `## Lessons from the factory` section of `AGENTS.md`,
which Codex reads. If it's a symlink to another file tracked in the repo,
the lines go in that file. A symlink out of the repo, or to anything but a
plain tracked file, is refused. With no `AGENTS.md`, the PR makes one.

Claude Code reads `CLAUDE.md`, not `AGENTS.md`. So when Claude builds and
your `CLAUDE.md` doesn't load `AGENTS.md`, the same PR adds an `@AGENTS.md`
line at its end (or makes a `CLAUDE.md` of just that line), so one file
steers both. An import inside a code block or comment doesn't count, as it
doesn't for Claude. Take the line out and the next lessons PR adds it
again; to keep Claude off `AGENTS.md`, route builds to Codex. Set this up when you add a repo, and the readiness check
reports it as a gap until you do (see
[Starting a new project](new-project.md)).

The daemon, not the agent, makes the change: on the `factory/lessons`
branch, in a draft PR labeled `factory:awaiting-you`. If that branch has
commits of someone else's and no open PR, the factory doesn't push over
them: it uses `factory/lessons-2`, and so on. The PR lists each line with
why it was proposed (as quoted text, so nothing in it links, closes or
mentions) and links to the issues and PRs it came from. While it's open,
new lines are added to it. A line already in the file or in the PR, or one
you declined, is skipped, and a line with HTML, an `@`, a link or an image
is refused. You get a notification each time it opens or grows.

If the factory can't write the file (a symlink out of the repo, say), it
finds out before the agent runs, tells you once, and sets aside what it had
to learn from rather than trying again each day.

Read the lines before you merge: they steer every later build. Drop any you
don't want on the branch. If you reword one, the factory counts the
original as declined and doesn't track your version. The PR never merges
itself, whatever `auto_merge` says, and no build ever runs on its branch.

To turn a line down, take it out of the PR's branch, or close the PR without
merging it to turn them all down, or delete it from the file after you
merge. The factory remembers declined lines and never proposes them again;
the next lessons start a fresh PR.

To turn it off, set `learning: {enabled: false}` (see
[Configuration](configuration.md#learning-from-your-edits)).
