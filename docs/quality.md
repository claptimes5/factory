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
| Diff guard | The change stays under `max_changed_lines` and touches no `protected_paths`: the factory's own controls and files that steer agents. Files a tool writes (lockfiles and RuboCop's `.rubocop_todo.yml`) don't count toward the size limit. |
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

The PR's Testing section lists every gate with its result and the end
of each passing command's output, redacted (see
[The PR description](using.md#the-pr-description)).

Then the reviewer reads the issue, the diff and the gate results. A
change to auth, input handling, commands, SQL or dependencies also gets a
[security pass](#security-pass) beside it. Before a finding blocks, a
second, cheaper model checks it (see [Review findings](#review-findings)). A block sends the findings back to
the builder, for at most `max_review_rounds` fix rounds (2); a third block
earns it one [rethink](#rethink-before-parking) before it parks. A malformed review retries once, then parks, so a broken
reviewer never consumes a fix round or passes code.

A fix round builds on the PR's branch, so first the factory merges the
base branch's latest into it: what merged since, such as a fix to a check
that fails on the base, reaches the build, and every retry of the round
gets it too. When the merge conflicts, the builder resolves it as part of
the round, and a build that leaves a conflict marker in those files fails.
A build that stops partway through resolving one (out of turns,
cancelled, or cut short by a shutdown) keeps the merge in progress, so
[Continue](using.md) carries on from its resolution so far.

## Rethink before parking

When the reviewer blocks past the round cap, the item gets one more try
before it parks: a fresh build from the base branch, on the implement
route one size up (`S` and `XS` go to `implement.M`, `M` and `L` to
`implement.L`; the item's own route when that one is missing). The old branch's approach is dropped; the builder gets
the reviewer's unresolved findings, fenced as the reviewer's words, and
is told the approach itself may be wrong and to reconsider it from
scratch. The rethink gets its own `max_review_rounds` fix rounds, on the
same route, and reviewers start over with it: its rounds count from 1,
and findings on the dropped attempt aren't carried as open findings. Its
first reviewer gets them, fenced, as context, to check whether the new
change repeats any of them. If it hits the cap
too, the item parks, and the notice says both attempts did.

A rethink happens once per triage, so a `factory:retry` allows another.
Its build job is the record (its input says `rethink`), so a restart
never starts a second; the issue gets a comment, the item a `rethink`
event, and the PR says it is the second approach.

Only the round cap rethinks. Questions, a failed build, a refused
estimate and every other park are as before, and so are the protections
on the way to a PR: a high-risk item still gets its critic and waits for
you. With `budgets.confirm_over` set, a rethink must fit the item's
estimate (its p75, or what you approved): what it has billed since
triage, plus the rethink route's build cap and one round of review (the
reviewer, the critic on a high-risk item, the security pass when the
first approach had one). Otherwise it parks at the cap, saying why. `review.rethink: false` turns it off.

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

Every item has a risk. Triage rates it `low` or `high` from the issue,
before any code exists. Once the change is built and its gates pass, a
[feature map](#feature-map) can raise it to `medium` or `high`, and so can
[its diff](#risk-from-the-diff). Nothing lowers it. Review gets stricter
with it, and the PR tells you how much of your attention it needs:

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

## One vendor only

A factory can run every step on Claude alone or on Codex alone (see
[Claude-only review](configuration.md#claude-only-review) and
[Codex-only](configuration.md#codex-only)). The reviewer and the verifier
still never run on a model a builder can reach, and the security pass never
on a reviewer's; what it gives up is the second vendor's blind spots.

**To revisit after real use.** Judgment calls made before any real runs:
- A Codex builder runs with `--sandbox danger-full-access` in its
  container, as Claude's runs with `bypassPermissions`, with the container
  and the egress allowlist as the boundary. `workspace-write` with
  `--add-dir` for the out directory and
  `sandbox_workspace_write.network_access` could be a second layer inside
  Docker, if Codex's sandbox runs there and lets installs and services
  through; the local runner already uses it.
- Codex models compare by exact name, so a `-codex` variant of a model
  counts as a different model from it. That may be too loose.
- With two Codex models, the security pass runs on the builder's model,
  and with Claude it runs on a builder tier that didn't build the item
  (Haiku for a Sonnet build). Neither has been measured against the
  cross-vendor default.
- The orchestrator reaches the daemon's MCP tools on Codex through
  `-c mcp_servers.<name>.*` overrides, with `default_tools_approval_mode`
  set to `approve`: without it Codex 0.163.0 fails every call ("requires
  approval, but approval policy is never") and still exits 0, so the
  adapter now fails a run with any tool call Codex couldn't make. Approving
  up front leaves each tool's own checks (reasons, caps) as the only
  guard, as with Claude's `--allowedTools mcp__factory`.
- A workspace's builder runs in its first repo's image. Rails first works
  for a Rails API and a Vite app, since the job image adds Node to a Ruby
  base; a workspace whose first repo uses the default image would need its
  image to carry every member's toolchain by hand.

## Risk from the diff

After the gates pass and before review starts, the daemon rates the change
again from what was actually built, and the item keeps the higher of that
and triage's rating. So a change that grew while it was built gets the
stronger reviewer (and the critic, at high), and can no longer merge
itself. It reads the diff in its own repository, never the builder's
checkout. What it looks at:

| Signal | Raises it to |
| --- | --- |
| Changed lines, lockfiles left out: `risk.medium_lines` (150), `risk.high_lines` (300) | medium, high |
| Changed files: `risk.medium_files` (10), `risk.high_files` (25) | medium, high |
| A path in `risk.medium_paths` (deploy and infrastructure files by default) or `risk.high_paths` (none by default) | medium, high |
| A feature the change touches, by its `risk` in the [feature map](#feature-map) | its risk |
| A migration: a change to a file in the [sensitive list](configuration.md#sensitive-code)'s `migrations` area (or `risk.migration_paths`), other than a test or a doc | high |
| Auth code: a change to a file in its `auth`, `crypto`, `secrets` or `payments` area (or `risk.auth_paths`), other than a test, a doc or an image, or vendored code (`vendor/`, `node_modules/`, `third_party/`) | high |
| A new dependency: a name a manifest or lockfile lists that it didn't before, or one whose source changed | medium |
| A deleted test: a test file deleted, or a test function or case taken out and not put back elsewhere | medium |

New dependencies are read from Go (`go.mod`), npm (`package.json`,
`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`), Python
(`requirements*.txt`, `pyproject.toml`, `Pipfile`, `Pipfile.lock`,
`poetry.lock`, `uv.lock`), Ruby (`Gemfile`, `*.gemspec`, `Gemfile.lock`)
and Rust (`Cargo.toml`, `Cargo.lock`). A version bump from the usual
registry doesn't count; a package a lockfile pulls in with it does, and so
does a change of where one comes from: an npm alias (`npm:other@1`), a
git, GitHub, file or URL spec, an override or resolution, a go.mod
`replace`, a Cargo `[patch]` or `[replace]`, a pip index, find-links,
direct URL or git line, another registry in a lockfile. Other ecosystems
get no dependency check of their own: a change to their manifests
(`composer.json`, `setup.py`, `setup.cfg`, `pom.xml`, `build.gradle*`,
`*.csproj`, `mix.exs`, `Package.swift`, `Podfile`, `deno.json`, `go.work`
and the like) counts as a new dependency whatever it changes.

Deleted tests are found in Go (testify suite methods too), Python, Ruby,
JavaScript and TypeScript test files by name, and Rust's test attributes
(`#[test]`, `#[tokio::test(...)]`, `#[rstest]`, `#[test_case(...)]`) by
count. A test counts as deleted when the change takes out more of that
name than it puts back in the same directory, so moving one within its
package doesn't. The diff guard already blocks a deleted test file; a test
taken out of a file that stays gets this far. A test that's skipped or
disabled rather than taken out isn't found here.

Paths, package and test names come from the builder's diff, so the PR
quotes each in a code span, clipped, with any issue reference or mention
broken.

When the rating raises the risk, the item's audit trail says so ("low ->
high from the diff: adds or changes a migration: `db/migrate/2_users.rb`"),
the PR's "How to review" says why, and every reason it found is listed
under it, with its level.

**Changes that always need you.** Some kinds of change wait for you
whatever their risk: by default migrations, new dependencies, auth code
and deleted tests (`risk.needs_person`). Such a PR never merges itself,
even when `auto_merge` covers its risk and size. Its "How to review" says
which kind and where, under **Needs a person**, the notification that it's
ready ends with "It needs you whatever its risk: it adds dependencies",
and the dashboard's item view shows both lists. A diff the factory
couldn't rate (a manifest it can't parse, say), or a build with no rating
on record, waits for you the same way, and the PR says so. If the raised
risk can't be saved, the build fails and is retried rather than reviewed
at the lower risk.

The rating is saved with the build job, so the PR, auto-merge and a review
resumed after a restart all read the same one, and every fix round rates
the whole change again.

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

That's judged once, when the finding is raised, against the decision of
the reviewer who raised it, and kept on the finding. So a critic's block
in a paired review doesn't make the reviewer's passing notes block, and
a round that blocks only because it upheld an earlier finding doesn't
turn its new notes into blocks. A finding carried from an earlier round
always blocks.

A reviewer that blocks without raising a finding blocks the round, with
nothing to verify. That holds when other findings join it: an earlier
round's carried into this one, or, in a paired review, the other side's
notes.

**Verified before it blocks.** Every finding that would block goes to the
verifier (`routing.verify`, a cheap model), with the issue, the diff and
a read-only checkout. It confirms each one or refutes it, with a reason;
it's told to refute only what it can show is wrong. Only confirmed
findings block. A refuted one is kept as a note: the builder sees it if
the round blocks anyway, and the PR lists it under "Reviewer notes". If
the verifier can't run (budget, an error, no ruling on a finding), the
finding blocks: the verifier can only take a block away, never pass code
the reviewer would have stopped.

**Only what threatens the merge blocks.** The verifier can also rule a
finding a `note`: true, but no threat to merging the change. A finding
threatens the merge when the change does the wrong thing or leaves part
of the issue undone, breaks something that worked, has a test that
doesn't prove the fix or would break the suite, or opens a security or
data-safety problem. Style, comments, how a test is organized and a design
the reviewer would prefer are notes. A critical finding, or one from the
security pass, blocks whatever the verifier rules: whether those threaten
the merge isn't its call.

**The rounds settle.** A re-review reads what changed since it last
looked, and only those lines can earn a new block. A new finding on code
it already read and passed is a note, never sent to the verifier. A fix's
regression is on lines the fix changed, so it still blocks; so do critical
and security findings, wherever they are.

**A note holds auto-merge.** Noted findings don't cost a round, but
nothing has tested a ruling that a true finding doesn't matter. So the PR
lists them, says it won't merge itself, and waits for you, even where
[`auto_merge`](configuration.md#auto-merge) would take it.

**Answered in the fix round.** The builder is shown the blocking findings
with their ids and the verifier's reasons, fenced as text to check rather
than instructions, and answers each in its verdict: `fixed`, `wrong` (with
why) or `out_of_scope` (with why). One it leaves out is recorded as
unanswered. The change already passed every check, so the builder
runs only the tests and lint on what it changes; the gates run them all
again on the result.

**Ruled on by the next review.** The next round's reviewer sees its open
findings with the builder's answers, also fenced, and rules on each:
`resolved`, `upheld` (it blocks again, with the same id, and is verified
again) or `withdrawn` (the builder was right). One it doesn't rule on
stands as upheld, with "the review didn't rule on it" as its evidence:
only a ruling lifts a block. Of two rulings on one finding (a paired
review's reviewer and critic, say), an upheld one wins. It's told not to raise
again what was already raised or refuted. It reviews the diff since the
commit it last reviewed, with the whole change as context when that's
small (see [How review checks itself](configuration.md#how-review-checks-itself)).
A workspace change is reviewed whole each round.

Every finding and how it ended is kept in the database, so the metrics
can say how often a reviewer's findings were worth a round.

**To revisit after real use.** These defaults were picked before any
real runs, and are to be looked at again once the metrics have data:
- A rethink goes one size up, not one model tier up: on routes where the
  next size uses the same model, it differs only in effort and skills.
  Whether rethinks pass more often than fix rounds, and whether one is
  enough, is for the metrics to say.
- A rethink's budget check counts one round of review; a rethink that
  needs its fix rounds can bill past the estimate.
- A noted finding always holds auto-merge. Once enough PRs with notes
  have merged and held up, the Trust screen could offer to let them merge
  themselves; it doesn't yet.
- A re-review settles on the lines changed since that reviewer last
  looked, hunks and their context. A critic that didn't run last round
  reads more than one round's change.
- The verifier confirms a finding when it can't tell, so a finding it's
  unsure of still blocks. That's safe, but a light filter; a stricter
  verifier would refute more.
- `review.paired_high_risk` is off: a critic beside the reviewer costs a
  run on every high-risk round.
- A finding the builder doesn't answer is recorded as unanswered and the
  build goes on; it could fail the round instead.
- A refuted finding goes on the list of what not to raise again, so one
  wrong refutation sticks for the rest of the item's rounds.
- A `withdrawn` or `resolved` ruling lifts a block the verifier confirmed,
  with no second check: the reviewer's word alone.
- The verifier is a cheaper model than the reviewer and reads untrusted
  text (the diff, the issue, the findings). It is fenced, and all it can
  do is refute, but refuting is removing a block: a verifier talked into
  refuting passes code the reviewer stopped.
- [Risk from the diff](#risk-from-the-diff): the thresholds (medium at 150
  lines or 10 files, high at 300 lines or 25 files), the default paths
  (deploy and infrastructure files medium, none high), what counts as auth
  code (the sensitive list's `auth`, `crypto`, `secrets` and `payments`
  areas: narrow names such as `auth.*`, `session_*`, `*jwt*` and
  `payment*`, which will still catch some code that isn't, and miss
  some that is, which only the wide `access` area's pass sees), the
  levels each kind raises to (migrations and auth high, new dependencies
  and deleted tests medium), and the four kinds that always need you.
  The metrics, and the dashboard's Spend screen, count how often the
  diff raised an item's risk.
- Crypto, secrets and payments code now needs a person, as auth code
  does, since they share the sensitive list with the security pass. That
  may hold more PRs (`*crypt*` catches any file with crypt in its name).
  The wide names (`*token*`, `*session*`, `*middleware*`, `*checkout*`)
  only start a pass, so a token service named `tokens.go` merges itself
  if `auto_merge` allows it.
- The [security pass](#security-pass)'s default globs (the sensitive
  list) and risky-code
  signals were picked by hand. Some will start passes that find nothing
  (`*session*`, `*proxy*`, `pyproject.toml`), and some holes live in files
  they don't name. `security_passes` against `security_blocks` says how
  often a pass is worth its run.
- A feature map that can't be read starts a security pass on every
  change, since its high-risk features can't be ruled out.
- A security finding the verifier refutes stops blocking. The PR says so
  and won't merge itself, but a person has to read it.
- Old-bug hunting is off, and its findings aren't put to the verifier
  before they're filed: a person approves each issue anyway. If too many
  are closed unbuilt, a verifier pass, or a stricter prompt, comes next.
- A hunt's finding counts as filed before when its file and title match
  one, or it's within 5 lines of one in the same file: two real bugs that
  close together are filed one at a time at best, and the same bug moved
  further by edits, and worded anew, can come back.

## A plan before big builds

A big change that goes the wrong way costs a whole build and a review
round to find out. So an item triage rates `L` (the sizes are
`plan.sizes`) is planned before it's built: the builder's own route
reads the repo, read-only, and writes a short plan (the approach, the
files it will touch, the risks, and how it will test it), which goes on
the issue. Nothing is built until someone approves it with `factory:go`.
Correcting a plan takes a comment; correcting a finished PR takes
another build.

When the item's cost estimate waits too, the plan and the estimate are
asked about in one comment, and one `factory:go` approves both. To change
the plan, comment what you want and add `factory:replan`: the builder
plans once more with your comment, and if you ask again the item parks
rather than loop. The approved plan goes to the builder, which is told to
follow it and say where it departed, and to the reviewer, which checks
the change against it. Plan and comment are fenced in both prompts, and
the issue in the planner's.

Medium items can have the critic check their plan instead
(`plan.medium: critic`): it approves the plan or raises concerns, which
send it back once, and the build goes on without anyone. It's off by
default, because it's two more runs on every M item. See
[Plans before big builds](configuration.md#plans-before-big-builds).

**To revisit after real use.**
- Only `L` waits for a person, and `M` gets no plan unless you turn the
  critic on. Whether plans on M catch enough to pay for two runs is for
  the metrics to say.
- The re-plan after the critic's concerns is built without another
  check, and a critic's check that fails twice lets the build go on
  without it: the gates and the review still hold the change.
- The plan job's cost isn't in the cost estimate's history yet (it counts
  implement, review and critic jobs), so an `L` estimate leaves out one
  read-only run.
- A change request is the labeler's latest comment on the issue; one
  written before the plan, with nothing newer, would be taken as one.
- A plan is approved, or sent back with what to change, from the item's
  page or with the labels; the page's buttons need the password or a
  browser on the factory's machine, as approving an estimate does.

## Security pass

A change that touches code where a mistake is an exploit gets a security
review of its own, beside the reviewer's. It runs on `routing.security`
(here Claude Opus, while Codex reviews), with its own prompt,
[`prompts/security.md`](../prompts/security.md), which asks only for
concrete, exploitable problems: injection, broken authentication or
authorization, path traversal, leaked secrets, server-side request
forgery, unsafe deserialization, weakened crypto or TLS, and risky
dependencies. Style, tests and bugs with no security consequence are left
to the reviewer.

A change gets one when any of these holds:

- **A changed file is in the sensitive list** (or `security.paths`, when
  set). Its default areas name authentication, sessions and permissions,
  cryptography, keys and secrets, payments, uploads, webhooks,
  serializers and redirects, running commands, SQL and migrations, CI
  workflows, and every common dependency manifest and lockfile (see
  [Sensitive code](configuration.md#sensitive-code)), the same list risk
  and REVIEW.md's `skip` read. Tests and docs don't count.
- **It touches a `high` feature** in the [feature map](#feature-map),
  through its code.
- **The code it adds does something risky:** runs a command
  (`exec.Command`, `subprocess`, `child_process`), builds SQL by joining
  or formatting strings rather than with placeholders, evaluates code,
  deserializes an unsafe format (`pickle`, `yaml.load` without a safe
  loader), writes raw HTML, or turns off TLS checks. Only added lines
  count, outside tests and docs.
- **An earlier pass's finding is still open,** which only a security pass
  rules on.

It reads the whole change at the commit the gates verified, with the
reviewer's checkout, and starts beside the reviewer (after it, when both
share a harness that runs one job at a time). Its findings join the
reviewer's in one verdict and go through the same steps as theirs: an id,
the verifier, the builder's answer, and the next round's ruling, which the
security pass makes on its own findings and the reviewer on the rest.

**It blocks whatever the reviewer says.** Its `critical` findings always
block, and its `major` ones when it decides `block`, even when the
reviewer passes the change or only asks questions (the questions are
still asked). A `critical` security finding never goes to the verifier.
A `major` one is checked on the security pass's own route, not the
cheaper verifier's, and one a later round upholds isn't checked again.
When a security finding is refuted, the PR body lists it and the item
won't auto-merge.

It runs on a model of its own: `routing.security`, or else the first of
the critic's route and the implement routes that is neither a reviewer's
model nor one that built the item. It fails closed. When its harness is
paused, signed out or busy with another job, the review goes back to the
queue and waits for it, without spending an attempt (up to five times).
When the pass can't start for another reason, fails, or ends without a
valid verdict, the review fails, is retried once, and parks the item. It
never passes without it.

Each pass is a job of its own, kind `security`, with its model, cost and
log on record. The metrics count the passes and the rounds that blocked on
one of their findings.

## REVIEW.md

A repo can tell the factory's reviewers what matters to it in a
`REVIEW.md`, at the root or in `.github/` (the root one wins). Keep it
short: what failure looks like in this repo, paths not worth a review,
how many minor findings are worth reading, and real bugs from its past
to look for again.

```markdown
---
skip: [docs/, "*.lock", internal/gen/]
max_minor: 3
---
A failure here is a wrong total on an invoice or a lost order.
Past bugs: rounding in tax.go (#88), a missing tenant filter (#120).
```

- **It's read from the base branch,** never from the change, so a builder
  can't change what reviews it. Builders can't edit it either: `REVIEW.md`
  and `.github/REVIEW.md` are always [protected](security.md#isolation).
- **The reviewer, the critic and the verifier read it,** fenced and marked
  as the repo's guidance. It's written by people with write access, so it
  steers what they look at, but it's guidance, not permission: the
  verifier is told to refute a finding only when the code shows it's
  wrong, never because the guidance says so.
- **HTML comments are taken out,** and the text is cut at 16 KB, with a
  note. A file over 1 MB isn't read, nor is a symlink (its target isn't
  protected).

The front matter, a YAML block between `---` lines at the very top, is
optional. The daemon applies it to the reviewer's verdict itself:

| Key | What it does |
| --- | --- |
| `skip` | Paths or globs, matched as in the [feature map](#feature-map). A major or minor finding that would block, on a path it covers, becomes a note. Never a critical one, never on a high-risk item, and never on a protected path, a path of a `high` feature, or one that is security-sensitive: in the [sensitive list](configuration.md#sensitive-code), `security.paths`, `security.extra_paths`, `risk.auth_paths` or `risk.migration_paths`, or with a name that looks it (auth, session, secret, token, crypto, payment, billing, migrations, `.github/`, Dockerfiles, `.env`), which config can't take away. Only a file the change modifies can be skipped, never one it adds, renames or copies, and the finding's path must be a plain path in the repo. When the feature map can't be read, nothing is skipped. If that leaves the review's block with nothing to block on, it passes; findings carried from earlier rounds still block. |
| `max_minor` | Minor notes past this many are left out, and the PR says how many. A minor finding that blocks is never left out, so the cap can't pass a change. In a workspace, the lowest cap of its repos holds. |

Neither ever touches a finding of the [security pass](#security-pass),
which also reads the guidance, as guidance only.

A key it doesn't know, or a value it can't use, is ignored; the PR's
reviewer notes and the readiness check say so. The readiness check reports
a missing `REVIEW.md` as a tip, which costs no points.

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
  reviewed as high risk (with the critic), whatever triage said, and a
  change to a `high` feature's code gets a [security pass](#security-pass).
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

The map also steers [old-bug hunting](using.md#hunting-for-old-bugs),
which reviews one feature at a time, riskiest and longest-unhunted first.

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

It first asks the repo's host what the factory's token may do there,
without changing anything: it sends empty requests to open a pull
request, an issue and a label (and on GitLab to update them and
comment), which the host refuses with a 403 when the token lacks the
permission and with a "field missing" error when it has it. A token that
can't open pull requests is a blocker, the first setup step, since every
change ends in one. Then it clones the base branch, runs the install step
and every required check (the tests twice, to catch flakiness, or once if
a check in the last day ran the same test command twice and both passed), and looks
for tests, end-to-end tests, a feature map, a lockfile and CI. It prints a score and what's
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

While a repo's latest report has blockers, only the work fixing them
runs there: the issues filed for the blockers, and the pieces any of them
was split into. Every other task on that repo waits in the queue,
untouched, and starts by itself once a check finds the repo ready. (A
repo never checked, or whose check couldn't finish, holds nothing.)

Each one comes with an issue written for the factory's builder, so the
first thing the factory does on a new repo can be making it verifiable.
While a repo has blockers, `-queue` labels only those for the factory,
since every other job would fail until they're fixed. The gaps are filed
for later. `-queue` also hands the rest to the daemon: while the repo
isn't ready, `factory serve` checks it again by itself whenever its base
branch moves (a fix merged, by the factory or a person), and once the
blockers are fixed that run labels the gaps too. The choice is kept as
`data/readiness/<owner>__<name>.follow`; delete that file to stop it.

Issues it files carry `factory:readiness`. Running it again skips a gap
whose issue is still open or was closed as not planned, and files it again
if it was closed as done but came back. (On GitLab, which doesn't record
why an issue was closed, it never files a gap again once its issue is
closed.)

If the check itself can't run (no Docker, a missing job image, a command
that isn't installed, a timeout), it stops there and files nothing. The
report is saved to `data/readiness/<owner>__<name>.json`, with the number of
the issue filed for each gap.

**Following it on the dashboard.** Setup is split in two. **Your part**
is what only you can fix, as the dashboard's **Setup** screen's steps: the
factory's token can do its job, and the factory can install the repo and
run its checks (the services, the files git doesn't track, a command in
`factory.yaml` that won't run). **The factory's part** is every blocker a
change can fix (lint failing, tests failing or flaky, no tests): each
check files an issue for each, labeled for the factory, so they run
through the factory like any work, on Home and the line, tagged **Blocks
setup**, and you review each as a pull request. New tasks on the repo
wait ("Waits for setup") until they're merged. Gaps follow as
**Recommended**: filed too, and labeled once the repo is ready; tips
under **Nice to have**.

One **Next** box says the one thing to do now, with its button. When it's
about one finding (a token, a missing file, the factory's fix waiting for
your review), it sits on that finding's row, and a line at the top
points to it; anything only a person can do is marked **Needs you**.
While your part is left, Home leads with it, and the decisions waiting on
you are listed under it, the factory's setup fixes first. Once your part
is done, Home is Home again, and a line above every screen says what new
tasks are waiting for. Steps that are done fold to a line each.
`factory status` prints the same summary.

A blocker the factory fixed stays on the report until the check runs
again, since only a fresh run proves it. While a repo isn't ready, the
daemon checks it again by itself within five minutes of its base branch
moving, or of a lint baseline's pull request being merged or closed. It
also looks when it starts, and whenever someone has a dashboard page
open (asking the host at most every 30 seconds), so a merge shows soon.
**Check again** queues it now, on the base branch as it is, so a fix made
outside the factory counts too. A check is a job like any other, one per
repo at a time: the daemon runs it, it counts as running work, it's still
there after a restart, and its steps and outcome stay with the job. While a check runs,
the step it's checking again lists each command as it goes, with how
long it took and whether it passed; **What the last run did** keeps the
list once it's over. Like Settings, Check again needs the dashboard
password or a browser on the factory's own machine.

When a check the factory filed an issue for passes on the base branch,
the next check closes that issue with a comment, and the issues it was
split into too; the factory then stops working on their items.

### Lint that fails on the base branch: a baseline

A repo with years of lint debt fails `lint` on its base branch. When the
linter counts its offenses (RuboCop's "N offenses detected"), a change
that fails lint is held to the base commit: the factory runs lint there
too, and the change passes if it adds no offenses to those already
there. When it adds some, the failure says how many, so the next attempt
knows which to fix. So the setup check counts lint failing on the base
branch as a gap in the repo's score, not a blocker, and work goes on. A
linter with no count fails every change instead, as any check that fails
on the base branch does.

The old offenses still mean lint can't hold the code to its rules, and a
fix too big for one build can't be split into pieces that each pass a
whole-repo lint. So when the linter is RuboCop
(`bundle exec rubocop` or `bin/rubocop`), lint finds more than 50
offenses and there's no baseline yet, the issue the check filed for lint
is fixed with a baseline: when the factory picks it up, instead of
triaging it, it, without a model:

1. clones the base branch and runs the repo's install;
2. runs `rubocop --auto-gen-config --no-exclude-limit --no-offense-counts
   --no-auto-gen-timestamp`, which lists every current offense in
   `.rubocop_todo.yml` file by file (no cop is turned off for the whole
   repo) and includes it from `.rubocop.yml`;
3. checks the lint command passes with it. If it doesn't, it adds
   `inherit_mode: merge: [Exclude]` to `.rubocop.yml` (the generated
   lists otherwise replace a cop's own excludes, so a cop like
   `Style/Documentation`, which skips `test/` by default, starts flagging
   it), then lists any file still flagged, such as a `rubocop:disable`
   comment the baseline made redundant. If lint still fails, it pushes
   nothing;
4. pushes only those two files to `factory/lint-baseline` and opens a
   pull request: the item's, for you to review and merge like any of
   the factory's work. If making it fails, the item waits for you with
   why.

It asks the host first whether the token may open a pull request, and
starts nothing if it may not.

Once it's merged, the daemon checks again by itself: lint passes, the issue filed for it
and any issues it was split into are closed, and only new offenses fail
a change. The check knows the baseline is in from the commit it checked
(its `.rubocop_todo.yml` says RuboCop generated it), not from timing. If
lint still finds offenses there, ones that landed after the baseline was
made, the issues lint was split into close anyway (the offenses they were
for are accepted), and the leftovers get one new issue, labeled for the
factory. A baseline pull request closed without merging stops being
asked for, and the offer to make one comes back. Pay the old ones down later by deleting a cop's entry from
`.rubocop_todo.yml` and fixing what it flags, a cop at a time. Running it
again before the pull request is merged updates the same pull request.

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

The dashboard's Spend screen (and `/metrics.json`) report:

- cost per merged PR by complexity and by the model that landed it;
- first-pass rate and escalation rate;
- reviewer blocks, and security passes with the rounds that blocked on
  one of their findings;
- review escapes: passed PRs you changed before merging, or closed;
- reviewer precision: the share of blocking findings that led to a fix
  (not refuted by the verifier, answered `fixed` and not upheld by a
  later review), overall and per reviewer route and model. A finding
  counts once it's settled: refuted, ruled on by a later review, or its
  item has left the build loop;
- the share of checked findings the verifier refuted;
- rounds to pass: the review rounds an item that passed review took, 1
  for a first-round pass;
- the revert rate: factory PRs merged in the last 30 days that a later
  commit or PR reverted on the base branch, overall and per repo, and how
  many turned main red (see
  [Auto-merge guard rails](using.md#auto-merge-guard-rails));
- items whose built diff raised their risk above triage's (see
  [Risk from the diff](#risk-from-the-diff));
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

## Reviewer evals

The replay set measures the builder. Reviewer cases, in the same
directory, measure the reviewer: each has a `kind` in its `case.yaml` and
the change to review in `change.patch`. `factory eval` ignores them, and
`factory eval review` reads only them.

| Kind | What it is | Scored |
| --- | --- | --- |
| `planted` | A merged PR with a known bug planted in a line it added | Caught when the review blocks with a finding in the bug's file within 6 lines of it (or on no line) |
| `clean` | A merged PR that stood | Any block is a false positive |
| `revert` | A factory PR the revert watch saw reverted | It should have been blocked |

The bugs are put in by the program, the same way each run: a flipped
comparison in an `if`, an off-by-one on a `< len(` bound, a dropped
`if err != nil` block, and a dropped `if x == nil` block. Go code only
for now (not tests). The case records the mutation, the file and the
line; a case whose bug would land elsewhere (the patch no longer applies
the same) fails as an eval error rather than scoring wrong.

Each case runs only the review stage, against a private copy of the repo
frozen at the case's base with the change on its branch, as if its gates
had passed: the review route for the case's `risk` (default low), the
verifier, and a high-risk change's critic. A block stops there; no fix
round runs. The report gives recall (planted bugs caught), the
false-positive rate (clean changes blocked), how many reverts it
blocked, and cost. A case the review gave no ruling on (an eval error, or
a review parked for no valid verdict) counts as an error and in no rate;
a review that asks questions counts as a block. With `-baseline`, the
candidate is rejected when the two runs reviewed different cases, when
more cases got no ruling, when recall falls or the false-positive rate
rises by more than `-tolerance` (default `tuning.review_eval_tolerance`,
5 points; 0 allows none), or when it blocks fewer reverts. The reasons
name each case that got worse.

`factory eval capture -review PR` writes, from a merged PR, a clean case
and one planted case per mutation that fits a line it added. When the
revert watch saw that PR reverted, it writes a revert case instead.
`factory eval capture -reverts` writes one for every reverted factory PR
that has none yet. Pick clean cases from PRs that merged unchanged and
stood a while: a PR fixed soon after isn't clean.

With `tuning.review_eval` on, the [tuning loop](using.md#tuning) runs
these before keeping any proposed change to how review works (see
[Tuning](configuration.md#tuning)).

**To revisit after real use.** Picked before any real runs:
- The tolerance, 5 points of recall or false-positive rate, is a guess;
  with 20 cases one case is 5 points, so it allows one case of noise.
- A finding counts as at the bug within 6 lines; a reviewer that names
  the right function from further away is scored as a miss.
- A reviewer that asks questions on a clean case counts as blocking it.
- Only four Go mutations; JS, TS and Python changes give clean cases only.
- The gate reviews at most `tuning.review_eval_cases` (20) cases, twice
  per proposal, each run capped at half of what's left of today's
  budget. The cap is checked before each case, so a run can pass it by
  one case's cost.

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
- **Findings you overruled.** When you merge a factory PR, each finding
  that blocked a round and was overruled is kept: the verifier refuted it,
  or the builder answered `wrong` and a later review withdrew it without
  the verifier having confirmed it. If you
  pushed to the PR first and your commits changed that finding's file, it
  doesn't count: you may have agreed with it after all.

Once a day per repo, a read-only learn job runs if there's something new: at
least one edit of yours, or `learning.min_findings` alike findings (in
files of the same folder and type, or with claims in the same words) from
two or more items. One runs at a time. It reads them with the repo's current
`AGENTS.md` and `CLAUDE.md`, and proposes up to three lines: general rules a
builder would follow next time, not one fix restated. It may propose none. A
lone finding waits, for up to 30 days, for one like it. It reads your five
newest edits and ten findings per group at most; older ones are set aside.

Findings you overruled teach the reviewers, not the builders. When
`learning.min_findings` alike ones (at least 2) from two or more items
repeat, the learn job may propose up to three lines for the repo's
[`REVIEW.md`](#reviewmd): a specific false alarm not to raise again. It
proposes REVIEW.md lines only from those, and lessons only from your edits
and repeated findings. The lines go under a `## Learned from findings you
overruled` section of the `REVIEW.md` there is (a new `REVIEW.md` at the
root when there's none), in the same lessons PR, with the same rules: you
merge it, declined lines are never proposed again, and a line with HTML,
an `@` or a link is refused. So is a REVIEW.md line about security-sensitive
code (auth, permissions, validation, injection, secrets and the like, or
a path in the [sensitive list](configuration.md#sensitive-code)) or a
blanket one ("any", "all", "skip", "ignore", "never flag", "approve"). A `REVIEW.md` the factory can't write (a
symlink out of the repo) only sets those findings aside.

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
