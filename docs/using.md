# Using the factory

This guide covers the screens you watch, the notifications you get, every
command, and the orchestrator that plans and steers work.

## The dashboard

`factory serve` serves the dashboard at `/app/` on `web.listen` (default
`127.0.0.1:8080`), so open http://127.0.0.1:8080/app/. It is a React app
built from `web/` and embedded in the binary, fonts included, so running
the factory needs no Node and loads nothing from outside. It follows the
system's dark or light setting, and the sun/moon button switches it.

To reach it from another machine, or to put a password on it, see
[Control room login](security.md#control-room-login).

| Screen | What it shows |
| --- | --- |
| **Floor** | What needs you first: PRs ready for review, with their gates and a button to the PR, and parked items with the question to answer. Below, a column per stage. A running job's card says what it is doing, read from its log ("editing webhook/retry.go"), with its model, attempt and how long it has run. A live feed of the day and the week's numbers sit beside it. |
| **Jump to** (⌘K or Ctrl+K) | Any item by number or title, or any screen. |
| **Live** | The feed on its own, said plainly ("PR #88 is ready for you", "started building with opus, attempt 2"), with the orchestrator's decisions marked. |
| **Item** | The [cost estimate](#cost-estimates) and what it's based on, with Approve estimate when it waits for you, the issue, triage's spec, the latest build's [screenshots](#screenshots-on-the-pr), jobs, audit trail, and the PR panel: CI checks from GitHub, gate results, the reviewer's verdict with each finding linked to its diff line, and Approve (on GitHub), Retry, Retry a tier up and Park. |
| **Session** | The job's transcript live beside the issue and spec, and a live terminal in the job's container (xterm.js over a WebSocket; Docker runner only). |
| **Diff** | Side by side or unified, the exact patch the reviewer saw, with its findings inline. |
| **Orchestrator** | Every decision with its reason, filterable by item and type. |
| **Spend** | Cost per merged PR, first-pass and escalation rates per week, daily spend against the cap, cost by size and model, and how the cost estimates compared with what finished builds cost. |
| **Inbox** | PRs ready for review, parked items, items waiting for your OK on their cost, and budget alerts. "Notify me in this browser" turns on browser notifications while a dashboard tab is open. |
| **Settings** | Budgets, models, escalation, auto-merge, the orchestrator, tuning and intake, saved to `factory.yaml`, and the latest [tuning proposal](#tuning). See below. |

When a repo's base branch is red or flaky (see
[Keeping main green](#keeping-main-green)), a line under the header says so
on every screen, with a link to the fix issue.

With several repos, the chips in the header narrow every screen to one. On
a phone, what's building and in review comes first and the other stages
are a tap away. A tablet gets two columns.

### Settings

The Settings screen changes part of `factory.yaml` for you:

- **Budgets**: the daily cap, jobs at once, PRs waiting for you, the cap
  per job and per triage or review, `pause_at_usage` and
  `fast_multiplier`.
- **Models**: the harness, model, effort and fast mode of each routing
  entry in the file.
- **Escalation**: the model tiers, attempts per tier and review rounds.
- **Auto-merge**: the top-level `up_to`, `max_size` and `method`.
- **Orchestrator**, **Tuning** and **Intake**: whether the orchestrator
  runs, its budgets and how often it runs; how often [tuning](#tuning)
  runs and the fewest builds it needs; and how often intake polls.

An empty box means the key is left out of the file, so its default
applies; the box shows that default. Gate commands, repos, the runner,
egress, env, services and pricing aren't on the screen, since they can
run commands on the host or open the network. Edit those in the file.

**Saving** usually writes only the lines that change, so your comments
and layout stay. A few YAML shapes (anchors, tags, multi-line values)
make it rewrite the whole file instead, which keeps comments but may
change spacing. The whole file must still load, or nothing is written and
the screen shows why, as the factory would say it at start. A save is
refused if the file changed since the page loaded it; reload to
see the change.

**Changes apply when the factory restarts**, since the daemon reads
`factory.yaml` only at start. Press **Restart when jobs finish**: no new
job starts, and once the running ones finish the daemon starts again with
the same command, reading the new file. You get a notification when the
restart is asked for. While it waits, **Cancel restart** calls it off.
The restart is refused if `factory.yaml` no longer loads (say, after a
hand edit). Restarting it yourself (systemd, `docker restart`) works too.

**Who can save.** Anyone who can open the dashboard can see the settings.
Saving and restarting need a password on the control room
(`FACTORY_WEB_PASSWORD`). Without one, they work only from the factory's
own machine: `web.listen` on a loopback address such as `127.0.0.1`, and
the page opened as `localhost` or `127.0.0.1`, not through a proxy such
as `tailscale serve`. Anywhere else the screen is read-only. Values that
would quietly stop the factory, such as no jobs at once or a poll every
second, are refused. See [Control room login](security.md#control-room-login).

## The classic control room

The older control room is still there on the same address (search the
dashboard for "Classic control room" to go back to it):

- **Needs you:** PRs waiting for review, parked items with the reason, budget alerts.
- **Board:** items by stage, with model and cost.
- **Job view:** live log over SSE, gate results, verdict, cost; buttons to
  retry, retry one tier up, cancel, and attach a terminal (with `ttyd`
  installed; otherwise it shows the `docker exec` command).
- **Orchestrator log** and **Spend** (today, 7 days, cost per merged PR).
- **Compare:** the reports from [`factory compare`](#comparing-skill-sets).

The numbers behind these screens are also at `/metrics` (see
[Metrics](quality.md#metrics)).

## Notifications

Set `FACTORY_NOTIFY_URL` in `factory.env` to one of:

- an ntfy topic URL, for phone push;
- a Slack incoming webhook;
- a Discord webhook.

You get a notification when an item parks, a PR is ready, an item waits
for your OK on its [cost estimate](#cost-estimates), the daily budget is
hit, or the job image is missing. With the orchestrator on, it sends a
daily summary there too.

The daemon also tells you when builds stop for
[low disk space](configuration.md#disk-space) and start again, and when you
merge a factory PR only after changing it (see
[Failures become cases](quality.md#failures-become-cases)). With
[main health](#keeping-main-green) on, it tells you once each time a check
goes red or flaky on a base branch and once when it's green again. With
[sources](#issues-from-sentry-dependabot-and-todos) on, it tells you about
each issue they file, once a day when the cap holds some back, and once
when a source can't be read.
With [grooming](#backlog-grooming) on, it tells you what each run
flagged, and when a split you confirmed files its sub-issues.

The dashboard's Inbox can also show browser notifications, but only while
a dashboard tab is open.

## Commands

Every command takes `-config factory.yaml` and `-env factory.env` before
the command name to use other files.

| Command | What it does |
| --- | --- |
| `factory setup` | Guided first-run setup: repo, Docker, GitHub App, labels, keys; saves `factory.env` |
| `factory codex-login` | Sign Codex in to a ChatGPT plan, or again once its sign-in stops working (see [Codex on a ChatGPT plan](setup.md#codex-on-a-chatgpt-plan)) |
| `factory images [--rebuild]` | Build the missing job images; `--rebuild` rebuilds them all |
| `factory readiness [-file\|-queue] [REPO]` | Measure how ready each repo is (see [How ready a repo is](quality.md#how-ready-a-repo-is)) |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage with their cost estimates, queue depth, today's spend, each repo's base branch CI (with main health on), sources, grooming's last run and open proposals, recent jobs |
| `factory tune` | Queue a [tuning](#tuning) run now, however few items were built; the daemon runs it |
| `factory groom [REPO]` | Queue a [backlog grooming](#backlog-grooming) run of one repo, or of each groomed repo, now, whatever `grooming.every` says; the daemon runs it |
| `factory limits clear [harness]` | Forgets the usage-limit readings, so a paused harness's jobs start before the window resets |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |
| `factory eval [-config candidate.yaml] [-set evals] [-baseline base.json] [-out report.json]` | Run the replay set and report pass rate and cost per pass; with a baseline, fail if the candidate is worse |
| `factory eval capture [-set evals] [-name NAME] [-accept CMD] PR` | Turn a merged PR into a replay case: the issue it closed, frozen where its branch started, with its tests as the hidden acceptance files |
| `factory compare ISSUE -skills a,b [-complexity M] [-no-push]` | Build one issue once per skill set and report the arms side by side |
| `factory pull ISSUE` | Fetch an item's branch into the current checkout and copy its job transcripts to `.factory/issue-N/` |
| `factory last-job`, `factory wait-job AFTER TIMEOUT`, `factory db-path` | Used by `deploy/upgrade.sh` |
| `factory mcp-bridge SOCKET` | Internal: the orchestrator's stdio MCP server, run inside its container |

With several repos, name an issue as `owner/name#12`. With one, `12` or
`#12` is enough.

## Taking over an item: `factory pull`

To look at or finish an item yourself, run `factory pull 12` in a checkout
of the repo. It fetches the item's branch into that checkout and copies its
job transcripts to `.factory/issue-12/`, so you can see what the builder
did and why.

## Measuring a change: `eval` and `compare`

Before you change a prompt, model or skill set, you can measure it.

- **`factory eval`** runs the replay set, a collection of past issues with
  hidden acceptance tests, against a candidate config. See
  [The replay set](quality.md#the-replay-set) and `evals/README.md`.
- **`factory compare`** builds one real issue once per skill set and puts
  the results side by side.

### Comparing skill sets

`factory compare 42 -skills tdd-strict,none` builds issue #42 once per
[skill set](configuration.md#skill-sets) (`none` means no skills):

- The arms run in parallel, from one frozen copy of the base, at the same
  complexity tier (the item's triage, or `-complexity`).
- Every arm goes through the same gates and review, but no PR is opened.
- Arms only see the base, never another arm's work or an earlier branch
  for the issue.
- Each arm's branch is pushed as `factory/compare-42-<set>`. Pass
  `-no-push` to keep them local.
- The arms split what is left of today's budget, and their spend counts
  against it.

You get a side-by-side report covering the outcome, review verdict and
findings, failed gates, implement attempts, cost, time and diff. It's saved
under `data/compare/` and shown in the control room under Compare.

## The orchestrator

The orchestrator is a short, read-only Claude run that plans and steers
work through the daemon's MCP tools. Its prompt is `prompts/orchestrate.md`.
Turn it on with `orchestrator.enabled: true`. It is off by default in code,
and on in this repo's `factory.yaml`, since it spends on its own schedule.

### When it runs

- On a new `factory:epic` issue.
- When an item's last two jobs failed.
- When the queue drains with work stuck: ready with no job, or blocked on
  dependencies that are not moving. It runs once per distinct set of stuck
  items, so items waiting on your PR review don't cost a run every half
  hour.
- Every `interval`.
- Once a day after `summary_hour`, for the summary it sends to your
  notifier.

One run at a time. Extra triggers, and any that piled up while the daemon
was down, coalesce into one run.

### What it can do

Its tools are `list_items`, `get_item`, `get_issue`, `queue_status`,
`get_job_log`, `create_issue`, `set_dependencies`, `set_priority`,
`enqueue`, `retry`, `park` and `notify_human`. There is no tool to merge,
push, close a PR or edit config.

The daemon checks each call against policy: daily issue cap, no dependency
cycles, stage allowed for the item, review-queue cap, budget and
notification rate. It writes every call, reads included, to the audit
trail with the reason the orchestrator gave. The Orchestrator screen shows
them.

The orchestrator also:

- cannot restart parked items (that stays your `factory:retry` label);
- may retry each item at most twice;
- may only add labels listed in `orchestrator.labels`;
- reads only approved issue text (see [Who can start work](security.md#who-can-start-work));
- has its own daily spending cap, which does not count against the build
  budget.

It sees every repo and names one when it opens an issue.

### Epics and dependencies

An epic becomes an item in stage `epic` that is never built, even if it
also carries `factory:ready`. It is done when all its children are. Items
whose dependencies are not done wait in `blocked` and start when the last
one merges.

## Tuning

Every `tuning.every` (720h by default, about a month), the factory reads
its own metrics and proposes changes to its settings. You decide whether
any of them happen.

**What it reads.** The metrics for the period since the last tuning run,
and for the period before it, so it sees trends: first-pass and
escalation rates, review escapes and cost per merged PR, each by size and
by the model a build started on, plus the most common park reasons. It
also gets the current value of every setting it may change.

**What it may change.** Most of what the Settings screen can: budgets,
models, escalation, the orchestrator's budgets and schedule, and intake.
Some things stay your call, so it never proposes them: auto-merge,
whether the orchestrator runs, the `orchestrate` and `tune` routes, and
its own `tuning` settings. Gate commands, repos, the runner and the
network aren't on the screen at all. The daemon applies each proposed
change to a copy of `factory.yaml` and keeps it only if the whole file
still loads. It drops the rest and says why. It never writes
`factory.yaml` itself.

**What else it proposes.** Ideas for prompts and skills, in plain prose,
for you to make by hand.

**Where proposals show up.**

- A notification with the summary.
- A **Tuning proposal** card at the top of the Settings screen: the
  summary, each change with the numbers behind it, the ideas, what was
  dropped, and the diff against the file as it is now. Untick a change
  to leave it out. **Apply** saves the ticked ones like the form does, to
  the file as it is now, so edits you made since stay. A change the file
  already makes is marked as applied. Then restart to apply them.
  **Dismiss** drops it. A new proposal replaces one you haven't acted on.
- With `tuning.repo` set, a draft PR on that repo with the change to
  `tuning.path`, labeled `factory:awaiting-you`. The factory never merges
  it. Applying the proposal on the Settings screen is fine too; close the
  PR afterwards. If the PR can't be opened, the proposal says why.

**Budgets.** A run that's due waits while today's budget or the
orchestrator's daily budget is spent, and you're told once a day. It runs
once there's budget again.

**Thin data.** A run is skipped, and you're told, when fewer than
`tuning.min_items` (10) items were built since the last run. The next one
comes a cadence later, over the longer period. The tuner is also told to
propose nothing when the numbers are thin. `factory tune` runs one now
regardless.

**Check model changes first.** Before you apply a change to a model, run
[`factory eval`](#measuring-a-change-eval-and-compare) with the candidate
config. A change that lowers the pass rate or raises cost per pass is the
wrong one.

Its prompt is `prompts/tune.md`, and it runs on `routing.tune` (see
[Configuration](configuration.md#tuning)). It runs whether or not the
orchestrator is on.

## Cost estimates

After triage, every item gets a cost estimate: a range from the median to
the p75 of what past items of its size cost to build, said with what it's
based on ("based on 14 past M items in you/api: median $1.80, p75 $3.10").
With too little history it's worked out from your budgets, and says so.
[Asking before costly builds](configuration.md#asking-before-costly-builds)
has how it's worked out, and what happens on a subscription plan.

It shows on the item's page, on its card while it waits to build, and in
`factory status`. Once the build is over, the item's page shows what it
actually cost beside the estimate, and the Spend screen shows how many
finished builds came in at or under their p75. Both count only the jobs
since the item's latest triage, so a build retried after an earlier one
parked is compared with its own estimate.

**Approving one.** With `budgets.confirm_over` set, an item whose p75 is
over it stops before implement. It goes to Blocked and to **Needs you**
on the Floor, and the factory:

1. Labels the issue `factory:awaiting-you`.
2. Comments with the estimate, what it's based on, and how to approve it.
3. Notifies you.

If the forge or the notifier fails partway, the factory tries again on
its next poll until all three went out.

To build it, either:

- add the `factory:go` label to the issue, after the factory's comment.
  As with `factory:ready`, it counts only from someone with write access
  (Developer or above on GitLab); from anyone else it always comes off,
  with a note saying who can add it. A `factory:go` added before the
  comment (say, while the item was in triage) comes off with that
  comment, and one on an item that isn't waiting comes off with a note:
  it approves only the estimate it was added for; or
- press **Approve estimate** on the item's page. Like Settings, this needs
  the control room's password, or a browser on the machine the factory
  runs on (see [Control room login](security.md#control-room-login)).
  Without either, the page says so instead of showing the button.

Then it builds as usual, or waits on its dependencies if it has any.
Nothing else builds it meanwhile: not the orchestrator's `enqueue`, not
**Retry** on an old build job, and the orchestrator doesn't count it as
stuck work. To drop it instead, park it. A parked item still needs the
OK: its estimate shows "parked", and the way back is `factory:retry`,
which triages and estimates it again.

It's asked once per item. If the item parks and you send it back with
`factory:retry`, it's triaged and estimated again, but it only waits
again if the issue's text changed or the new estimate is higher than the
one you approved. If the new estimate needn't wait, `factory:awaiting-you`
comes off.

The only items never held are main health fixes: ones the factory filed
for a red or flaky main, marked so when they came in (they stay exempt
after main is green again). A high priority alone exempts nothing, and
the orchestrator can't set priorities from 100 up.

## Keeping main green

With `main_health.enabled` (see [Configuration](configuration.md#main-health)),
the daemon reads CI on each repo's base branch every `main_health.every`
(10 minutes). When a check goes red or flakes there, it files an issue for
the fix and builds it before anything else.

**What it reads.** The checks on the branch's latest commit: GitHub's
check runs (GitHub Actions and apps that use the Checks API, not commit
statuses), or the jobs of GitLab's pipelines. A check is the same check on
every commit when it has the same name in the same GitHub workflow (or
app), or the same name in a GitLab pipeline of the same source, so two
workflows' `build` jobs, or a scheduled pipeline's `test` and a push
pipeline's, are separate checks. Workflow names need the app's
`actions: read` permission, which setup asks for; without it, Actions jobs
are told apart by name only. On a commit, only a check's latest run of CI
(its check suite, or its pipeline) counts, and in it the latest run with a
result:

- **Red:** it failed or timed out.
- **Flaky:** it failed, then passed when it was retried in the same check
  suite or pipeline. That's the only flake signal it uses: a check that
  fails on one commit and passes on the next may just have been fixed.
- A check still running counts as nothing, whatever it said before, and so
  does one that was cancelled or skipped, or a GitLab job whose runner
  broke (`runner_system_failure` and the like). A failure GitLab allows
  (`allow_failure`) counts as passing.

**What it files.** One open issue per repo and check, titled
`Main is red: <check>` or `Flaky on main: <check>` and labeled
`factory:main-health`. Check names are cut to 100 characters, on one
line, with invisible characters taken out. Before filing, it looks for an
open issue the factory itself opened with that title, so a lost database
doesn't file twice.

**First in the queue.** The fix's item gets priority 100. Items start at
0 and the orchestrator's and tuning runs at 10, so its triage, build and
review jobs are claimed first. The orchestrator's `set_priority` can still
move it down, and the factory doesn't put it back, but it can't set a
priority of 100 or more on anything.

**Approval, and what it can't do on its own.** CI output is text that
anyone who can shape what CI prints can write: a test that echoes its
input, a dependency, an app with `checks: write`. So:

- By default (`require_approval: false`) the issue is filed with
  `factory:ready` and starts at once, approved as the factory. Its text,
  which is the builder's task, holds only the check's name, the commit and
  a link to the failing run. The end of the run's output goes in a comment
  on the issue, fenced and marked untrusted, and no prompt reads comments.
- A fix started that way never merges itself, whatever `auto_merge` says:
  no human read its issue, so its PR waits for you.
- With `require_approval: true`, the issue is filed without
  `factory:ready`, with the output quoted in it for you to read, and starts
  once someone with write access adds the label. It's approved by them
  then, and merges under the usual rules.
- On GitLab, the output is only quoted when everyone who can read the
  project's issues can read its job logs: a private project, or public
  pipelines. Otherwise the issue links to the job.

Either way, if the issue's text changes after approval, the item parks as
any would.

**When it's green again.** Only an actual pass counts. Once the check
passes on the branch's latest commit, the factory comments on its issue
and closes it, as long as it's still open and still labeled
`factory:main-health`; an issue you took the label off is left alone. A
build for it that hasn't opened a PR yet is parked, since there's nothing
left to fix. If its PR is open, the issue stays open, with a comment, for
you to merge or close the PR.

A check that's missing from the branch's CI (a path filter, a renamed
job, an app that reports late) isn't a pass: it's left alone, and after a
day missing it's forgotten without a word on its issue.

A red check that passes on a retry is flaky, not fixed: its issue stays
open for the flake, with a comment saying so. A flaky check's issue isn't
closed when the check passes, since passing proves nothing about a flake;
it's done when its fix merges, or you close it. A flaky check that fails
outright on a later commit is red, and its issue says so.

**Muting.** Add `factory:mute` to a check's issue, open or closed, and
the factory files nothing more for that check and leaves the issue as it
is. On GitHub, closing the issue as not planned mutes it for 30 days, or
until you reopen it.

**Not looping.**

- One issue per check at a time.
- If the issue closes as done (its fix merged, say) and a later commit
  still fails, that's a new failure, and it's filed again. The first time,
  it starts on its own as before. After that it waits for you to add
  `factory:ready`: a fix that passes on its PR and leaves main red twice is
  likely for a check that only runs on main, such as a deploy.
- At most `main_health.daily_issue_cap` (3) issues per repo per UTC day.
  Past that you're told, and the issue is filed the next day if the check
  is still failing.
- A repo whose CI the factory can't read (no permission, no branch) or
  that has no checks at all gets no issues, and you're told once. A rate
  limit isn't "can't read": it's tried again next time.

`factory status` shows each repo's state and the checks failing now; so
does the dashboard while a repo is red or flaky.

## Issues from Sentry, Dependabot and TODOs

The factory can find work on its own in three places, each turned on in
[`sources`](configuration.md#sources):

- **Sentry:** unresolved errors with enough events (and users).
- **Dependabot:** open security alerts at or above a severity (GitHub only).
- **TODOs:** comments marked `TODO(factory)` or `FIXME(factory)` on each
  repo's base branch.

For each thing it finds, it files one issue, labeled `factory:from-sentry`,
`factory:from-dependabot` or `factory:from-todo`, and tells you (the
notice names the issue, not its title, which is someone else's text). Unlike
[main health](#keeping-main-green)'s, these issues are **never built on
their own.** They're filed without `factory:ready` and wait for someone
with write access to add it, through the usual
[approval check](security.md#who-can-start-work). Each issue says so in a
line of its own. Read it, edit it if you like, then label it; or close it.

**Sentry.** Every `sources.sentry.every` (15 minutes) the factory lists the
unresolved issues of each repo's Sentry project, most events first (or most
users, with `sort: user`), and files those with at least `min_events` (10)
events and `min_users` (0) users. The issue links the Sentry issue, gives
its counts and when it was first and last seen, and quotes its title,
culprit and the latest event's stack trace: the in-app frames only, at most
20 per exception. All of that is fenced and marked untrusted, since whoever
sets off an error controls its message.

**Dependabot.** Every `sources.dependabot.every` (1 hour) it reads the
open security alerts of each GitHub repo at `min_severity` (high) or worse,
and files one issue per alert, worst first: the advisory (GHSA and CVE),
severity, package, vulnerable versions, first patched version and manifest.
Dependabot often opens its own PR for an alert. When an open Dependabot PR
bumps the same package in the same folder (by its title or its
`dependabot/` branch, or a grouped update that lists the package), the
factory files nothing for it; if that PR closes unmerged, the alert is
filed on the next read. On a
GitHub App, reading alerts needs the *Dependabot alerts: read* permission
(see [Configuration](configuration.md#sources)). GitLab's dependency
scanning isn't read.

**TODOs.** Every `sources.todos.every` (6 hours) it makes a shallow clone of
each repo's base branch and searches the files git tracks there for the
markers, where one starts a comment (`// TODO(factory): ...`, `# ...`,
`/* ... */`, `<!-- ... -->`, and the like). The default markers are only
the factory's, so ordinary TODOs aren't filed; add `TODO` and `FIXME` to
`markers` to file those too. It leaves out vendored and generated code
(`vendor/`, `node_modules/`, `third_party/`, `dist/`, `testdata/`, minified
and lock files, files that say `Code generated ... DO NOT EDIT`), prose
(`.md`, `.txt` and the like), and anything in `skip_paths`. At most
`max_per_run` (5) are filed per scan; the rest follow on later scans. A
TODO is known by its file and its text, not its line, so lines added above
it don't file it again. The issue links the line at the commit and quotes
the lines around it, fenced and marked untrusted.

**When it goes away.** Once the Sentry issue is resolved (or archived),
the alert is fixed or dismissed, or the comment is gone from the base
branch, the factory closes the issue (as not planned, on GitHub) and says
why, as long as the issue is still open, still has its source label, was
opened by the factory, still reads as the factory filed it, and nobody
approved it (`factory:ready`, `factory:epic`, or a build for it). An
approved issue is left to its build; the factory only stops tracking it
once it closes. One someone edited is left open for you.

Titles are cut to one line with no invisible characters, and `#12` and
`@name` in them are spaced out, so a title can't mention anyone or, once
built, close another issue. The factory's commit messages and PR titles
also space out a closing keyword's issue number (`fixes # 12`) for every
issue, not only these.

**Not looping.**

- One issue per Sentry issue, alert, or TODO, kept in the factory's
  database. Before filing it also looks for an open issue it filed itself
  (opened by its own app or account) for the same thing, so a lost
  database doesn't file twice. An issue you close
  isn't filed again while what it was filed for is still there.
- At most `sources.daily_issue_cap` (3) issues per repo per UTC day, across
  all three sources. Past that you're told once that day, and the rest are
  filed on later days if they're still there.
- A source the factory can't read for a repo (a missing or bad Sentry
  token, no permission for the alerts, no such project) files nothing, and
  you're told once, and again when it can read it. A server error or a
  rate limit is retried quietly, then read again on the next cadence.

Reads run beside the daemon's loop, so a slow API or a large clone doesn't
hold up intake or builds, and a restart doesn't read again before the
cadence is due. The TODO scan stops reading matches at 2000 (and then
closes nothing that scan), and doesn't clone while the disk is low.

`factory status` shows each source's last read of each repo, what it found
and the issues it has open.

## Backlog grooming

With [`grooming.every`](configuration.md#backlog-grooming) set (weekly is
a good start), the factory looks over each repo's open issues and flags
three things for you:

- **Likely duplicates:** issues that ask for the same change, with the one
  to keep.
- **Likely stale issues:** no activity for `grooming.stale_after` (90
  days), and something in them suggests they're obsolete, such as naming
  code or a version that's gone. Age alone isn't enough.
- **Issues too big for one change**, with a proposed split into 2 to 5
  sub-issues, each with a title and a short body.

**It never closes, edits or splits anything on its own.** For each issue it
flags, it leaves one comment saying why and what you can do, and adds a
label: `factory:duplicate?`, `factory:stale?` or `factory:split?`. Then:

| You | The factory |
| --- | --- |
| Add `factory:close` to a duplicate or stale issue | Closes it (on GitHub, as a duplicate or as not planned) with a note: for a duplicate, which issue to follow instead |
| Add `factory:split` to an issue flagged too big | Opens the proposed sub-issues, each waiting for `factory:ready` like any other, and makes the issue their [epic](#epics-and-dependencies) |
| Take the `?` label off | Drops the proposal, and doesn't make it again for that issue unless the issue's title or text changes |
| Close the issue yourself | Forgets the proposal |
| Approve the issue, or let the factory take it up some other way | Takes the `?` label off and forgets the proposal |

`factory:close` and `factory:split` count only from someone with write
access (Developer or above on GitLab), checked as `factory:ready` is (see
[Who can start work](security.md#who-can-start-work)). From anyone else,
or if the issue changed after the label went on, the label comes off and
a comment says why. The factory also refuses, takes both labels off and
says why, when the issue's title or text changed since it was flagged, an
open PR would now close it, or, for a duplicate, the issue to keep has
closed or is itself proposed closed: of two issues, only one is ever
closed as the other's duplicate. Nor is an issue closed in favor of one a
source filed that nobody has approved, since the source may close that
one itself. A confirming label on an issue with no
matching proposal does nothing.

**Splits.** The sub-issues say they're part of the original and carry no
factory label, so nothing is built until someone approves each one. The
original becomes an epic item, labeled `factory:epic`, that's never built
itself; an approved sub-issue becomes its child. The epic is done once
every sub-issue is closed and every approved one is built, including when
all were closed unbuilt. The orchestrator can't split it again. Should
filing fail partway, the next poll carries on: each sub-issue's body ends
with a hidden marker, so one filed before the failure is found, not filed
twice.

**What it reads.** Each run reads up to `grooming.max_issues` (50) open
issues: their title, labels, age, last activity and the first 1500
characters of their text, all fenced as untrusted (see
[Grooming: untrusted issue text](security.md#grooming-untrusted-issue-text)).
It leaves alone, and only lists by title so it can name them as the issue
to keep, issues that:

- the factory has an item for (being built, parked, or done);
- an open PR would close;
- carry any `factory:` label or the intake label: in progress, an epic,
  filed by [main health](#keeping-main-green) or a
  [source](#issues-from-sentry-dependabot-and-todos) (whose own loop
  closes them when what they were filed for goes away), or flagged already;
- a split filed and nobody has approved yet.

**What it posts.** Just before flagging an issue, the daemon reads it
again and leaves it alone if it closed, changed, or was taken up while
the run went on. It checks every proposal against what the run was shown: issue numbers it wasn't given, or may not flag, are dropped,
as are stale calls on recently active issues and splits into fewer than
2 or more than 5. Anything it would post must be plain prose: no links,
@mentions, HTML, GitLab references or quick actions, and `#N` only for an
issue the run was shown. A proposal that breaks a rule is dropped and the
job's verdict says why; a reply that isn't the schema fails the run. At
most 10 issues are flagged per run.

**Not twice.** Each comment ends with a hidden marker naming the proposal
and the issue text it was made for. The factory records each proposal
before commenting, and drops the record if the comment or label fails.
Before commenting it checks the record and the issue's comments for the
marker, counting only the factory's own comments: when it finds its
marker with no record (a lost database, or a label that failed), it puts
the label and record back without commenting again. One caveat: after a
lost database, that can put back a `?` label you had taken off, if the
issue hasn't changed since. An issue has at most one open proposal.

**Budget.** A groom job runs on `routing.groom` (Sonnet by default) and
counts toward `budgets.daily_total`. A run that's due waits while today's
budget is spent, and you're told once a day.

**Checking for your labels.** While proposals are open, the daemon looks
at most every 10 minutes: per repo with open proposals, it lists the
issues carrying `factory:close`, `factory:split` and each `?` label in
use (one list call per label, more for long lists), and reads each
flagged issue whose `?` label is gone or that's confirmed. Confirming
reads the issue, and for a duplicate the issue to keep, once more. Every
6 hours it also reads each split's sub-issues nobody has approved yet,
one call each, to see which closed. With no proposals open and no
sub-issues waiting it makes no calls.

`factory status` shows each repo's last run, what it read and flagged, and
the proposals still open. Its prompt is `prompts/groom.md`.

## Screenshots on the PR

With a repo's [`preview`](configuration.md#pr-previews) set, each build of
a web app that passes its gates is run and photographed before review.

**How.** In the export the gates just passed on, with the dependencies
the install step fetched, the factory starts the app with `preview.start`
and `$PORT`, in the repo's job image like any gate: no secrets, the same
egress allowlist, the repo's services. It polls `preview.ready` until the
app answers, then the image's own headless Chromium (Playwright) opens
each page at each viewport, waits for it to load and settle, and takes a
full-page screenshot, cut at 4000 pixels. With `preview.before`, the base
branch gets the same on its own export, so each page shows before and
after.

**Which pages.** The `pages` you list, then the ones the builder names:
when the repo has a preview, the builder's verdict lists the paths its
change shows up on. A builder's entry that isn't a plain path (`/orders/1`)
is dropped. All together, pages × viewports × sides stay within
`previews.max_screenshots` (12): past it the builder's pages are dropped
first, then yours, then viewports. Each page has 45 seconds to load and be
taken, so one that hangs costs its own time and not the rest's; when
`previews.timeout` runs short, the pages left are skipped.

**The reviewer** gets them as image files beside its prompt, with each
page, size and side named, and is asked to treat a visibly broken page as
a finding. Codex gets them attached to its prompt; Claude reads them with
its Read tool. At most 12 go to the reviewer.

**The PR** gets one comment, a table per page with a column per viewport
and a row per side; each image links to its full size. Each push edits
the same comment, and the last build's images are removed once the new
ones are up. A live preview link from `preview.url` goes at the top. On
GitHub the images are committed to the `factory/previews` branch (see
below); on GitLab they're the project's uploads, as a comment's images
would be.

**When something goes wrong** (the image has no Chromium, the app never
answers, a page errors, the capture runs out of time), the comment says
so in a folded note, in the factory's own words ("the app didn't answer
/ within 1m", "2 of 4 screenshots couldn't be taken"), and the build goes
on as if previews were off. The app's log and the browser's errors are on
the item's page in the dashboard only, never on the PR: the log can quote
secrets from the repo's `files`. A preview never fails or retries a build.

**Cleanup.** A PR's images are removed when it merges or closes, and
after `previews.keep_for` (two weeks) on a PR left open, and its comment
says so. The `factory/previews` branch is one commit with no history:
each change makes it anew, so a removed image is gone from the branch,
and the branch itself goes once it's empty. It has no workflow files, so
updating it runs no CI. If a branch protection rule matches every branch
(`**`; `*` stops at a `/`) or a ruleset targets all branches, exempt
`factory/previews`, or the uploads fail and the comment says so. A branch
named `factory` blocks it too (git can't have both `factory` and
`factory/previews`): the comment says so, and renaming that branch fixes
it. The factory only ever rewrites a `factory/previews` whose one commit
is its own ("factory: PR screenshots", no parents); any other branch of
that name is left alone and the uploads are refused.

A removed image is gone from the branch, but GitHub keeps the commit it
was in until its own garbage collection, so someone who saved an image's
link (it names the commit) can still open it for a while. Don't rely on
removal to take back a screenshot that showed something it shouldn't.

On GitLab, removing uploads takes the Maintainer role and GitLab 17.2 or
later. Without them the factory can't remove a PR's images: it says so in
the comment once, stops trying, and leaves them to you (Settings >
General > Uploads).

**On the dashboard.** The item's page shows the latest build's
screenshots, served from the job's folder on the factory's machine, so
they're there even before the PR opens or after the PR's copies are
removed.

**Not yet.** A short video of each page, and deploying a live preview
with a command of the factory's own. `preview.url` links one that your
own hosting deploys; screenshots don't depend on it.

## Working on the dashboard

The JSON API is at `/api/v1/` and described in `internal/web/openapi.yaml`.
The app's types are generated from it, and a Go test fails if the spec and
the Go types drift. To work on the app:

```sh
go run ./internal/web/devserver &   # sample data on 127.0.0.1:8090
cd web && npm ci && npm run dev     # http://localhost:5173/app/
npm run build                       # regenerate types, typecheck, build into internal/web/app
npm run e2e                         # Playwright on desktop and phone sizes
```

Commit the rebuilt `internal/web/app` with your change; CI checks it is
current.
