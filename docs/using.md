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

On a desk, a sidebar holds the screens, search, the repo filter, what's
running, the day's spend and plan usage. On a phone, tabs at the bottom do
(Home, Line, Ask, History, Settings), and Settings leads to the rest. The
dashboard installs as a web app: on a phone, add it to the home screen
(on an iPhone, Share, then Add to Home Screen) and it opens like an app,
with push notifications (see [Notifications](#notifications)).

| Screen | What it shows |
| --- | --- |
| **Home** | While a repo isn't set up, its one next step leads (see [How ready a repo is](quality.md#how-ready-a-repo-is)), with what needs you listed under it in the order to do it, numbered: the work that fixes setup first. Otherwise, what needs you, one decision at a time: a PR to review, a plan or a cost to OK, a question or your call on a parked item, each with its buttons. Reviews come first, then whatever other work waits for ("Do first: #20 waits for it"), then the rest. Last comes what can't pass its checks until setup or the base branch is fixed ("Waits for the base branch": its build failed a check that fails on the base branch too, so Retry it once that's fixed). **Later** sends one to the back, and the next two are listed. Below, the line at a glance (Queue, Build, Review, and You as the last station), a line when a repo has [earned more trust](#trust), "since you looked": what went live since Home was last marked seen (kept by the factory, so a phone and a desk agree), and a box that opens Ask. |
| **Ask the factory** | [Chat](#chat-opening-issues-with-the-orchestrator) with the orchestrator's model from any screen, with suggestions to start from: beside the screen on a wide desk, a sheet over it on a laptop or phone. |
| **The line** | The old Floor: what needs you, with each item's buttons, then a column per stage. A running job's card says what it is doing, read from its log ("editing webhook/retry.go"), with its model, attempt and how long it has run. A live feed of the day and the week's numbers sit beside it. |
| **Jump to** (⌘K or Ctrl+K) | Any item by number or title, or any screen. |
| **History** | The activity, said plainly ("PR #88 is ready for you", "started building with opus, attempt 2"); what's done; and the orchestrator's own decisions with their reasons, filterable by item and type. |
| **Trust** | Each repo's [trust level](#trust), its record, its practice runs, and the level its record has earned. |
| **Item** | Where it is along the line, as stamps (Plan, Build, Review, You, Live); the [plan](#plans-before-big-builds) of a big item, with Build it and Ask for another plan when it waits for you, the [cost estimate](#cost-estimates) and what it's based on, with Approve estimate when it waits for you, the issue, triage's spec, the risk the latest build's diff raised and why, and whether it [needs you whatever its risk](quality.md#risk-from-the-diff), the latest build's [screenshots](#screenshots-on-the-pr), jobs, audit trail, and the PR panel: CI checks from GitHub, gate results, the reviewer's verdict with each finding linked to its diff line, and Approve (on GitHub), Retry, Tier up and Park. A parked item says why and offers its choices at the top. |
| **Session** | The job's transcript beside the issue and spec, the way Claude Code shows a session: the agent's words as the thread, each run of tool calls folded into a line ("Ran 3 commands, read 2 files") that opens to each call's input and output, thinking tucked away, failed calls in sight, a "Now:" line while it runs and how it ended at the bottom. Raw log downloads the whole log. While a Claude build runs, a box sends it a message it reads at its next step. A build that ran out of turns or budget shows its note and the work it kept. Then a live terminal in the job's container (xterm.js over a WebSocket; Docker runner only). |
| **Diff** | Side by side or unified, the exact patch the reviewer saw, with its findings inline. |
| **Orchestrator** | The full chat, with every conversation (Ask opens it), and the orchestrator's decisions. |
| **Spend** | Plan usage, cost per merged PR, first-pass and escalation rates per week, review escapes, [security passes](quality.md#security-pass) and the rounds they blocked, the [revert rate](#auto-merge-guard-rails), how many items their diff raised the risk of, reverts by repo, daily spend against the cap, cost by size and model, and how the cost estimates compared with what finished builds cost. |
| **Inbox** | Every item waiting on you in one list, and budget alerts. "Notify me in this browser" shows them while a dashboard tab is open. |
| **When to tell you** | Push notifications on this device and the list of devices with them on, a test, and [quiet hours](#quiet-hours). |
| **Settings** | Budgets, models, escalation, auto-merge, the orchestrator, quiet hours, tuning and intake, saved to `factory.yaml`, and the latest [tuning proposal](#tuning). See below. |
| **Setup** | How ready each repo is for the factory, from the [readiness check](quality.md#how-ready-a-repo-is): your part as steps (the token, and the factory being able to install the repo and run its checks), and the factory's part, each blocker it's fixing as an issue with its item (waiting its turn, being built, needs you, done). A **Next** box says the one thing to do now, on the row it's about. **Check again** runs the check on the repo as it is now; each check files the factory's work by itself. |
| **Design** | `/app/design`, not in the menus: the dashboard's colors, type and components, for anyone changing it. |

Warnings that apply to every screen sit in one notice area above it; the
most serious shows, and the rest wait behind "more notices". When a repo's
base branch is red or flaky (see [Keeping main green](#keeping-main-green)),
a notice says so, with a link to the fix issue. When a repo's
[auto-merge breaker](#auto-merge-guard-rails) has tripped, a notice says
that too, with a **Reset** button behind the same password check as
Settings.

While a repo the readiness check looked at has blockers, or its check
couldn't finish, a notice says how far its setup is, with a link to
**Setup**. Once every checked repo is ready, it says to add the
`factory:ready` label to issues to start, until the factory has worked on
anything but setup.

When Claude or Codex runs on a subscription plan, the sidebar and Spend
show each plan's usage windows ("5-hour: 42% used, resets 6:06pm",
"Weekly: 18% used"), since on a plan those limit the work, not dollars.
Until a harness reports a reading it says so: Claude Code only includes a
percentage once a window gets close to its limit, and Codex reports after
each job on the Docker runner. A job on a plan costs no dollars: it
records none, so it never counts toward `budgets.daily_total` (the plan's
windows and `budgets.pause_at_usage` are its limit), and job and item
pages say so. When every harness in use is on a plan, the dashboard shows
no dollars at all: the sidebar shows plan usage only, Spend becomes
**Usage**, and PR descriptions mark each job "plan". With API keys,
dollars are what count.

With several repos, the chips in the header narrow every screen to one. On
a phone, what's building and in review comes first and the other stages
are a tap away. A tablet gets two columns.

### Settings

The Settings screen changes part of `factory.yaml` for you. It starts
with **How hard it works**, a ladder of presets:

| Preset | What it sets |
| --- | --- |
| **Lean** | Small models for most builds, one review round, findings not double-checked |
| **Balanced** | A small model for small builds, the big one for large builds and review, findings double-checked, two review rounds |
| **Thorough** | Bigger models for most builds, the reviewer at high effort, high-risk changes reviewed in pairs, three review rounds |
| **Maximum** | The biggest model at high effort for every build and review |

A preset sets the model of each step (`routing`), `review.verify`,
`review.paired_high_risk`, `escalation.implement` and
`escalation.max_review_rounds`, for the harness that builds and the one
that reviews, and leaves everything else as it is. Claude's models are
haiku, sonnet and opus; Codex's are the ones your config prices or routes
to, cheapest first, since their names differ by account. With one harness
the reviewer gets its biggest model, and the security pass never runs on
a reviewer's model. The card says which preset the file is at, or "set by
hand" once you've changed one of those settings yourself, and
**Swap** changes which harness builds and which reviews.

The rest is in five sections, each folded to a line that says where it
stands ("$25 a day · 3 jobs at once"); one you've edited opens:

- **Spending and limits**: the daily cap, jobs at once, PRs waiting for
  you and `pause_at_usage` first; the per-job, triage and review caps and
  `fast_multiplier` under **More limits**. When every harness is on a
  plan, the dollar caps fold away, since no job costs dollars.
- **Models**: the harness, model, effort and fast mode of each routing
  entry in the file.
- **Review and escalation**: whether findings are verified and high-risk
  reviews paired; the model tiers, attempts per tier and review rounds.
- **Automation**: auto-merge's top-level `up_to`, `max_size` and
  `method`; whether the orchestrator runs, its budgets and how often; how
  often [tuning](#tuning) runs and the fewest builds it needs; and how
  often intake polls.
- **Notifications and asks**: quiet hours and concise asks.

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

**Remove the factory**, at the bottom, lists the factory's labels on your
repos (with how many open issues and PRs carry each), the job images it
built and its job data, then removes them once you confirm. The same
access as saving applies. See [Removing the factory](setup.md#removing-the-factory).

## What to do with a parked item

A parked item waits for you and nothing runs on it. Its card, its page
and the inbox say why in plain words, and offer what you can do, each
with the model it runs on (hover a button, or open "What do these do?"):

- **Continue**, for a build that ran out of turns or budget: picks up
  where it stopped. See below.
- **Retry**: starts the step over from scratch, on a fresh checkout, with
  the same model and a fresh budget. Anything the stopped job did is
  dropped.
- **Tier up**: the same, one model tier up (`escalation.implement`).
- **Park**: stops work on an item and takes it off the queue. Nothing is
  lost; bring it back with Retry or Continue, or add the `factory:retry`
  label on the issue to send it back through triage.

The issue gets a comment saying the same, and where to find the item on
the dashboard (Items, then its issue number). The comment is public, so
it names no address.
The `factory:retry` label still works, but you don't have to go to the
issue unless the factory asked a question there (a park from triage).

To drop an item altogether, close its issue on GitHub or GitLab. On its
next poll the factory cancels any job queued or running for it and marks
it done ("its issue was closed"). An item with a pull request is left to
its pull request: close or merge that instead.

### What happened

The header shows only where an item is now, so a build that failed and
then had its retry cancelled reads as just "parked". A parked item, or
one whose latest jobs failed, ran out of turns, timed out or were
cancelled, has a **What happened** box at the top of its page. It tells
the last few jobs in order from what the factory recorded, each linked
to its job: "The build ran out of turns (job #12), then its retry was
cancelled by you (job #14)." An item parked with no job to blame (a
question from triage) shows why it was parked.

### Split it

When an item's build ran out of turns or budget, timed out, or went over
the diff size limit, or the item was parked with no build to blame, it may
be too big for one build. A build that failed a check isn't offered a
split: smaller pieces fail it too. When that check fails on the base branch
as well, as the setup check found, the build parks after that one try
instead of moving to a stronger model, and What happened says so: make it
pass there first, or take it out of the required gates until it does. **Split it** in the What
happened box starts a new [chat](#chat-opening-issues-with-the-orchestrator)
with the orchestrator's model and sends it a message naming the item,
its issue and what happened, with the failed run's job to read. The
model opens smaller issues, each with this item as its parent, then
closes this item's issue in favor of them (as not planned on GitHub),
with a comment naming them. The item becomes their epic, done once they
are. The button is off,
with the reason, while chat can't reply (its daily budget is spent, for
one).

### When a build runs out of turns or budget

A build has its size's `budgets.max_turns` turns (40, 80, 150 and 300
for XS to L by default; other steps such as triage and review keep their
own 80) and its size's
`budgets.per_job` dollars. Ten turns before the end, the factory tells
Claude to stop starting new work, commit what it has and write what's
done and what's left to a note. If it still runs out, it doesn't restart
on its own: the same task would most likely run out again. Instead:

- Its checkout is kept as it left it, committed or not, under the data
  directory (`data/jobs/<id>/kept`), and survives a restart. Nothing is
  pushed: the forge sees nothing until a build's gates pass, as always.
- The item parks, and its job page shows the note and the work so far
  against the base branch.
- **Continue** picks up in that checkout and, on the Docker runner with
  the same model, the same Claude session, with a fresh `max_turns` and
  budget. You can add 50%, 100% or 200% more of both (adding needs the
  dashboard password, or the dashboard opened on the factory's machine)
  and a note it reads first, for example to narrow the scope. On the
  local runner, or if the session is gone, it starts a new session that's
  told where the last one stopped. If a continue is cancelled, or the
  factory restarts before it's done, the work goes back to the stopped
  build, so you can continue again. If the kept work is gone, the
  continue says so and builds nothing.
- Retry or Tier up start over instead, and the kept checkout is removed.
  It's also removed when the item is done, and after 14 days.

Codex has no turn cap and only reports its cost at the end, so a Codex
build never stops this way; its overruns are recorded, as before.

### Messaging a running build

While a Claude build runs, the box under its transcript sends it a
message, which it reads at its next step (Claude runs with streaming
input for this). Your message shows in the transcript as "You: ...".
Codex reads its whole prompt when it starts, so its box is off.

## The classic control room

The older control room is still there on the same address (search the
dashboard for "Classic control room" to go back to it):

- **Needs you:** PRs waiting for review, parked items with the reason, budget alerts.
- **Board:** items by stage, with model and cost.
- **Job view:** live log over SSE, gate results, verdict, cost; buttons to
  retry, retry one tier up, cancel, and attach a terminal (through `ttyd`;
  without it, or on a dashboard served beyond this machine, it shows the
  `docker exec` command instead).
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
hit, or the job image is missing. A ready PR of a kind that always needs
you ([a migration, a new dependency, auth code or a deleted
test](quality.md#risk-from-the-diff)) says so at the end: "It needs you
whatever its risk: it adds dependencies." With the orchestrator on, it sends a
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
It tells you once when a factory PR is reverted, once when main health
sees a check go red on a factory PR's merge commit, and once when a repo's
[auto-merge breaker](#auto-merge-guard-rails) trips (and when a cool-off
turns it back on). A PR set to merge itself that it hands back to you
(CI failed once it was brought up to date with main, say) is one
notification too.

The same notifications go to every device that turned on push from the
dashboard's **When to tell you** screen: the dashboard installed on a
phone's home screen (iPhones need iOS 16.4 or later, and the web app
opened from the home screen; a tap opens the dashboard, as iPhones don't
show buttons on web notifications), or a desktop browser. The factory
makes its own push key the first time and keeps it in its database, so
there's nothing to set up. The dashboard's Inbox can also show browser
notifications while a dashboard tab is open.

### Quiet hours

With quiet hours set, only something wrong goes out during them: main
broken or flaky, a factory PR reverted or turning main red, the
auto-merge breaker tripping or auto-merge that couldn't be turned off, the
daily budget reached, or the disk full. Everything else is held, and sent
as one catch-up ("While it was quiet: 4 updates") once they end:

```yaml
notify:
  quiet_hours:
    from: "19:00"        # 24-hour; until may be earlier, spanning midnight
    until: "08:00"
    weekends: true       # Saturday and Sunday quiet all day
    timezone: Europe/Madrid  # IANA name; left out, the factory machine's
```

Set them on the **When to tell you** screen or in Settings; they apply
once the factory restarts.

## Commands

Every command takes `-config factory.yaml` and `-env factory.env` before
the command name to use other files.

| Command | What it does |
| --- | --- |
| `factory setup` | Guided first-run setup: repo, Docker, GitHub App, labels, keys; saves `factory.env` |
| `factory codex-login` | Sign Codex in to a ChatGPT plan, or again once its sign-in stops working (see [Codex on a ChatGPT plan](setup.md#codex-on-a-chatgpt-plan)) |
| `factory images [--rebuild]` | Build the missing job images; `--rebuild` rebuilds them all |
| `factory uninstall [--data] [--dry-run]` | List, then remove, the factory's labels and job images, and with `--data` its data folder (see [Removing the factory](setup.md#removing-the-factory)) |
| `factory readiness [-file\|-queue] [REPO]` | Measure how ready each repo is (see [How ready a repo is](quality.md#how-ready-a-repo-is)) |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage with their cost estimates, each repo's setup progress from its last readiness check, queue depth, today's spend, a plan with no usage reading yet, review quality (escape rate, precision, rounds to pass), each repo's base branch CI (with main health on), sources, grooming's last run and open proposals, each repo's auto-merge breaker, the revert rate, recent jobs |
| `factory automerge [reset REPO]` | Show each repo's [auto-merge breaker](#auto-merge-guard-rails); `reset` turns a tripped one off so the repo's PRs may merge themselves again. The running daemon picks it up at once |
| `factory tune` | Queue a [tuning](#tuning) run now, however few items were built; the daemon runs it |
| `factory groom [REPO]` | Queue a [backlog grooming](#backlog-grooming) run of one repo, or of each groomed repo, now, whatever `grooming.every` says; the daemon runs it |
| `factory limits clear [harness]` | Forgets the usage-limit readings, so a paused harness's jobs start before the window resets |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |
| `factory eval [-config candidate.yaml] [-set evals] [-baseline base.json] [-out report.json]` | Run the replay set and report pass rate and cost per pass; with a baseline, fail if the candidate is worse |
| `factory eval capture [-set evals] [-name NAME] [-accept CMD] PR` | Turn a merged PR into a replay case: the issue it closed, frozen where its branch started, with its tests as the hidden acceptance files |
| `factory eval review [-config candidate.yaml] [-set evals] [-baseline base.json] [-out report.json] [-tolerance 0.05]` | Run the [reviewer evals](quality.md#reviewer-evals) and report recall, false positives and reverts blocked; with a baseline, fail if the candidate's reviewer is worse |
| `factory eval capture -review [-set evals] [-name NAME] PR` / `factory eval capture -reverts` | Turn a merged PR into reviewer cases (clean, and bugs planted in it; a revert case if it was reverted), or every reverted factory PR into a revert case |
| `factory compare ISSUE -skills a,b [-complexity M] [-no-push]` | Build one issue once per skill set and report the arms side by side |
| `factory pull ISSUE` | Fetch an item's branch into the current checkout and copy its job transcripts to `.factory/issue-N/` |
| `factory last-job`, `factory wait-job AFTER TIMEOUT`, `factory db-path` | Used by `deploy/upgrade.sh` |
| `factory mcp-bridge SOCKET` | Internal: the orchestrator's stdio MCP server, run inside its container |

With several repos, name an issue as `owner/name#12`. With one, `12` or
`#12` is enough.

### Stopping the factory

Ctrl-C (or SIGTERM) on `factory serve` stops it starting new jobs and
says how many are running. They get up to 10 minutes to finish.

- **Ctrl-C again** stops them now. A build cut short keeps its checkout,
  as one that [runs out of turns](#when-a-build-runs-out-of-turns-or-budget)
  does, and is queued again to continue from it when the factory starts:
  its job says "Cut short when the factory shut down. Its work is kept".
  Any other job (triage, a review) runs again from the start. A build
  the Ctrl-C cut short before its model ran (while it cloned, say) runs
  again at the same attempt: a shutdown doesn't count as a failed try.
- **Ctrl-C a third time** quits at once. A build cut off then starts over
  on the next start, as after a crash; its item's history says "the
  factory stopped while it ran, so it runs again from the start".

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

The orchestrator is a short, read-only Claude (or Codex) run that plans and steers
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

### Chat: opening issues with the orchestrator

The Orchestrator screen opens on a chat with the orchestrator's model (the
`orchestrate` route, Opus by default). Say what you want built, as you would
to a teammate. Each message starts one run (prompt `prompts/chat.md`) that
can read your base branches and the factory's work with the orchestrator's
read tools, open issues with `create_issue`, and close a split item's issue
with `close_split`. It can't change code or run commands.
It works whether or not the orchestrator runs on its own schedule.

An issue chat opens gets the intake label and any of `orchestrator.labels`
it asked for, is approved as the chat's, and goes to triage like an issue
you labeled. Chat and the orchestrator share `orchestrator.daily_issue_cap`.

Chats from before it could open issues may hold drafts: cards under the
reply that reach GitHub or GitLab only when you press **Create issue** (or
**Create all**), after editing them if you like, or **Dismiss** them. A
create files only the text you saw: a card that changed since is refused
until you look again.

A reply doesn't take or wait for a job slot, since it builds nothing, but one is
written at a time, and a restart waits for it. Each costs up to `orchestrator.run_budget`, and chat
shares `orchestrator.daily_budget` with the orchestrator and tuning. A reply
that fails says why, with **Try again**; **How it got there** on a reply opens
its run. Issue text no one approved is withheld from it, as from the
orchestrator: paste it into the chat instead.

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
wrong one. For a change to a review prompt, route or model, run
`factory eval review` the same way. With `tuning.review_eval` on, the
tuning loop does that itself for the review changes it proposes, and
drops them when the reviewer gets worse (see
[Configuration](configuration.md#tuning)).

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

## Plans before big builds

An item triage rates `L` isn't built straight away. The builder first
reads the repo and writes a short plan (approach, files, risks, tests),
and the factory comments it on the issue, labels it
`factory:awaiting-you` and notifies you with the issue's link. The item
goes to **Needs you** on the Floor, as a **Plan check** card, and shows the
plan on its page. It's Blocked until you do one of these.

**To build it**, either:

1. press **Build it** on the item's page; or
2. add `factory:go` to the issue, after the factory's comment. It counts
   only from someone with write access, as for an estimate.

**To get another plan**, either:

1. press **Ask for another plan** on the item's page and say what to
   change. What you write goes on the issue too, fenced; or
2. comment on the issue what to change, then add `factory:replan`.

The builder plans once more with what you asked, and the new plan is
asked about the same way. Asking a second time parks the item; edit the
issue and use `factory:retry` to start over.

The buttons need what **Approve estimate** does: the control room's
password, or a browser on the machine the factory runs on (see
[Control room login](security.md#control-room-login)). Without either,
the item's page lists the label steps instead, with the issue's link.
`factory status` lists each item waiting for your OK, with the same
steps and links.

If the item's [cost estimate](#cost-estimates) waits too, one comment asks
about both, and one `factory:go` (or **Build it** on the item's page)
approves both. Nothing builds the item meanwhile, not the
orchestrator's `enqueue` either. The approved plan goes to the builder and
the reviewer.

With `plan.medium: critic`, an M item's plan is checked by the critic
instead: it's posted for you to read, the critic approves it or sends it
back once with its concerns, and the build goes on by itself. See
[Plans before big builds](configuration.md#plans-before-big-builds).

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

## Trust

How much the factory may do on its own is a level for each repo, on a
ladder toward full automation, on the dashboard's **Trust** screen:

| Level | The factory may, on its own |
| --- | --- |
| 1 | Nothing: every PR waits for you. |
| 2 | Merge tiny, low-risk changes (size XS). |
| 3 | Merge small, low-risk changes (up to S). |
| 4 | Merge low-risk changes up to size M. |
| 5 | Merge medium-risk changes too. |
| 6 | Pick its own work: what it finds itself starts without your label, and merges itself under level 5's rules. |
| 7 | Ship: release and deploy, with a canary and an automatic rollback. *Not built yet.* |

Levels 1 to 5 are names for a repo's [`auto_merge`](configuration.md#auto-merge)
(`up_to` and `max_size`); level 6 adds its
[`self_start`](configuration.md#picking-its-own-work). At every level, high-risk changes and the kinds
in `risk.needs_person` (a migration, a new dependency, auth code, a
deleted test) wait for you, and the [breaker](#auto-merge-guard-rails)
steps a repo back to level 1 when its merges are reverted or break main,
until you reset it.

**Earned, never taken.** For each repo the screen shows its record over
the last 30 days (merged, merged by itself, reverted, broke main) and a
practice run of each level: how many of those merged PRs that level would
have merged on its own, and how many of them went wrong. When the next
level's practice run is long enough (5 changes for level 2, then 10, 20
and 30) and none went wrong, the factory suggests it, on the screen and on
Home. Level 6's practice run is different: of the issues the factory filed
from its own findings in the window, how many you took on (it needs 10,
and 4 in 5), and whether any of what merged from them went wrong. Only a person raises a level: **Move here** writes the repo's own
`auto_merge` to `factory.yaml`, and it applies once the factory restarts.

The ladder follows research on agent autonomy: oversight moving from
approving each action to watching and stepping in (Anthropic, [Measuring
AI agent autonomy in practice](https://www.anthropic.com/research/measuring-agent-autonomy)),
autonomy as a level a person grants on evidence and takes back
([Levels of Autonomy for AI Agents](https://knightcolumbia.org/content/levels-of-autonomy-for-ai-agents-1)),
and a change freeze when a budget for failure is spent ([error budget
policy](https://sre.google/workbook/error-budget-policy/)).

### Picking its own work (level 6)

The factory already finds work: main health files a fix when a check
fails on main, and Dependabot, TODOs, Sentry and the old-bug hunt file
issues for what they find. The orchestrator opens issues on its own
schedule. Below level 6, what it finds waits for you: issues from the
sources wait for your label, and main health's fixes and the
orchestrator's issues are built but wait for you to merge them.

At level 6, the findings from the repo's `self_start.sources` (main
health and Dependabot when the Trust screen raises it) start without a
label, approved as "the factory (dependabot)" and so on, and merge
themselves under the repo's auto_merge. Up to 3 a day, and a fifth of
the daily budget for that work, start this way; past either, they wait
for your label. A tripped breaker stops it too. Home, The line and
History mark such work "Found by the factory".

## Auto-merge guard rails

With [auto-merge](configuration.md#auto-merge) on, three things keep a
bad run of PRs from merging themselves one after another.

**The revert rate.** The factory notices when a PR of its own is reverted
on the base branch: by a commit (`git revert`'s "This reverts commit
<sha>", GitLab's "This reverts merge request !N") or by a merged PR that
reverts it (GitHub's revert button makes one titled `Revert "<title>"`
with a `revert-N-` branch and "Reverts owner/name#N"; a squashed one
whose message was rewritten is still found by its PR). A commit or PR
naming a commit or PR number counts only against that one. A bare
`Revert "<title>"`, naming nothing else, counts only when exactly one
factory PR still standing has that title: each rebuild of an issue opens
a PR with the same title. A `revert-N-` branch counts only when its PR
also says it reverts something, since anyone can name a branch that way.
On GitLab, only `!N` names a merge request (`#N` is an issue). A revert
of a revert puts the change back and doesn't count. Each revert is recorded on
the item, you're told once, and the revert rate, reverted PRs over the
factory PRs merged in the last 30 days, shows in `factory status`, the
metrics API and the dashboard's Spend screen, beside review escapes.

It reads the branch on main health's cadence (`main_health.every`, 10
minutes), as part of main health's reading when that's on and on its own
when it isn't, but only for a repo with a factory PR merged in the last
30 days and not reverted yet, and only when the branch's head moved since
the last reading. Then it lists the commits since (at most 1,000) and the
PRs merged since (at most 50), from a day before its last reading so a
commit made earlier and pushed later is still seen. If there were more
commits than that, the watch's place (the head and time it last read
from) stays where it was, so the next reading starts from there again. A quiet repo costs one call per reading, and one with nothing to
watch for, none. It reads reverts even when main health can't read the
repo's CI.

**The breaker.** A strike is a factory PR that was reverted, or whose
merge commit main health saw a check go red on: the commit the check was
first red on is the PR's merge, and the check passed on the commit
before it, the merge's first parent (main health reads the branch's head,
so a second commit landing before its next reading hides the first). A
check that was red before the merge, missing there, still running there
or with no result there, one main health files again after its issue was
closed, or one whose commit before can't be read, is no strike. A PR that did both is one strike. Once a repo has `auto_merge.breaker.threshold`
strikes (2) within `window` (7 days), the factory:

- stops auto-merging in that repo: new PRs wait for you, as any would;
- turns off auto-merge on the PRs that were waiting to merge themselves,
  the same way parking does, and labels them `factory:awaiting-you`;
- tells you once, naming the strikes.

It stays tripped until you reset it with `factory automerge reset
owner/name` or the dashboard's **Reset** button, which needs the control
room's password or a request from the factory's own machine, as Settings
does. Strikes before a reset don't count again. The PRs it handed back
still wait for you, their descriptions no longer saying they merge
themselves. With `cool_off` set it also resets itself that long after it
tripped, and tells you; by default it doesn't, since a breaker that trips
is worth a look. A cool-off reset counts from when the cool-off ended, so
a strike after that counts (and can trip it again) even if nothing looked
at the breaker in between, and `factory status` shows it back on as soon
as the cool-off ends. A PR being opened as the breaker trips doesn't slip
through: the factory checks the breaker again right before it turns
auto-merge on.

**Tested against current main.** A PR only merges itself once it was
tested against the base branch as it is now:

- **With a merge queue** (GitHub's, or GitLab's merge trains), the PR goes
  into it: the queue tests it merged with the base and the PRs ahead of it
  before it merges. A GitHub merge queue merges with its own method, so
  `auto_merge.method` doesn't apply there. If nothing is left to wait for,
  the PR joins the queue only once every check on it has passed.
- **On GitLab with merged results pipelines** (with or without merge
  trains), each pipeline already runs on the merge request merged with
  its target branch, and a train retests it just before it merges, so the
  factory turns on auto-merge directly and doesn't bring the branch up to
  date itself. It accepts that pipeline for the verified commit when the
  pipeline ran at the request's merge or train ref on a commit whose
  parents include the verified one. This is built from GitLab's
  documented API and tested with recorded responses, not against a live
  project with trains. Merge trains without merged results pipelines
  don't run, so the factory syncs the branch as below.
- **Without either**, the factory checks, in a clone of its own, whether the
  branch has the base's latest commit. If it does, auto-merge is turned on
  for the verified commit as before. If it's behind, the factory merges
  the base into the branch (a merge commit it makes and pushes, like
  GitHub's "Update branch"), and waits for CI on that new head: once every
  check passes there, auto-merge is turned on for exactly that commit. CI
  failing there, or not finishing within 2 hours, leaves the PR for you,
  with a notification. If the base can't be merged in cleanly, the PR
  waits for you at once.
- If the base moves again before the PR merges, the factory turns its
  auto-merge off, brings it up to date again, and waits for CI again, up
  to 3 times; after that the PR waits for you. If anyone else pushes to
  the branch while it waits, it waits for you too. The count starts over
  for a new build of the PR, or once auto-merge was turned off. Merged by
  you while it waits for CI, it counts as your merge.

On GitLab this does what "rebase before merge" would, without rewriting
the commits the reviewer passed. The factory checks the base on each poll,
so without GitHub's "Require branches to be up to date before merging"
(or a merge queue) GitHub can merge a PR whose base moved after the last
poll and before the next resync. Turning that setting on makes the forge
enforce it.

**To revisit after real use.** Choices made before anyone ran these on
real repos:

- Factory PRs a human merged count toward the revert rate and the
  breaker, as auto-merged ones do.
- A revert that names only a title still counts, when exactly one
  standing factory PR has that title.
- Bringing a branch up to date makes a merge commit, which can clash with
  `method: rebase` (and a repo that forbids merge commits on branches).
- The breaker's settings are in the YAML only, not the Settings screen.
- A manual reset doesn't send a notification (a cool-off reset does).

`factory eval` and `factory compare` turn the revert watch and the breaker
off, as they do main health and sources: the base branch is the real
repo's, not the run's.

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

## Hunting for old bugs

With [`hunt.enabled`](configuration.md#old-bug-hunting) on, the factory
spends its idle time reading code it has already shipped. Once a day (by
default), when no other job is queued or running and the budget allows, a
hunt job checks out one repo's base branch read-only and reviews **one
area** of its [feature map](quality.md#feature-map). It goes round the
areas: never-hunted ones first, riskiest first, then the one hunted
longest ago, with a `high` area coming round three times as often as a
`low` one.

The hunt reports only concrete bugs it's confident of, each with the file
and line, what fails and how to make it fail, and a severity; style,
refactors and missing tests are out. The factory drops any finding whose
file isn't in the area or on the base branch, or whose line isn't in the
file, keeps the most severe `hunt.max_findings` (3), and files each as an
issue labeled `factory:from-hunt`, like the
[sources'](#issues-from-sentry-dependabot-and-todos): **never built on its
own**, it waits for someone with write access to add `factory:ready`. The
model's words are fenced in the issue as an unchecked report, beside the
lines of code it points at, and the builder is asked to prove the bug
with a failing test before fixing it.

A finding is filed once. Every finding filed is remembered, whatever
became of its issue: a later hunt's finding with the same file and title
(ignoring case and spacing), or in the same file within 5 lines of one, is
dropped, and each hunt is shown the titles of the area's earlier findings,
open or closed, as already reported or rejected. Closing a hunt's issue is how you say
no. `factory status` shows how many hunts ran per repo and how many
issues they filed.

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

## The PR description

Each draft PR the factory opens says what it closes and then, in this
order:

- **Summary**: the builder's own summary of the change, in a code block.
- **How to review**: the change's risk, why it has it, and how much of
  your attention it needs (see
  [Review scaled by risk](quality.md#review-scaled-by-risk)).
- **Testing**: what ran on the verified commit. The gates' verdict line,
  every check by name with pass or fail, what the
  [test-proves-fix](quality.md#test-proves-fix) check and the
  [coverage floor](quality.md#coverage-floor) found, the tests the builder
  says it added, and, folded away in a `<details>` block, the last 20
  lines of each passing command's output. Hidden scenarios show only
  pass or fail.
- **Blast radius**, when the repo has a [feature map](quality.md#feature-map).
- **Reviewer notes**: findings that didn't block (see
  [Review findings](quality.md#review-findings)).
- **How to revert**: the forge's Revert button, or the commands to revert
  the merge by hand, plus a step for each kind of change a revert doesn't
  undo on its own. Database migrations (a `migrations/` or `migration/`
  folder, Rails' `db/migrate/`, Alembic's `alembic/versions/`,
  Liquibase's `db/changelog/`, Flyway's `V1__name.sql`): the revert takes
  the files out but not the schema, so roll the migration back first.
  Configuration (`.env*`, YAML, TOML, INI and the like, or anything under
  `config/`, `deploy/`, `k8s/`, `helm/` or `terraform/`): redeploy or
  restart whatever picked up the new values. Dependencies (`go.mod`,
  `package.json`, lock files and the like): do a clean install afterwards.
- **Jobs**, with their models and cost, and the reviewer's verdict.

**The repo's PR template sets the layout.** When the base branch has
`.github/pull_request_template.md` (or `.github/PULL_REQUEST_TEMPLATE.md`,
`PULL_REQUEST_TEMPLATE.md`, `pull_request_template.md`,
`docs/pull_request_template.md`, `docs/PULL_REQUEST_TEMPLATE.md`), or on
GitLab `.gitlab/merge_request_templates/Default.md`, the description uses
its headings, in its order, and fills each one it has a section for:
Summary, Description, What or Why get the summary; Test plan, Testing or
How has this been tested get Testing; Risk, Impact or Reviewers get How to
review and the blast radius; Revert, Rollback or Roll back get How to
revert. A heading it has nothing for (Type of change, Checklist,
Screenshots) says _n/a_, and its sections no heading asked for follow the
template's. Only the template's headings are kept, not its text or
checkboxes, and its HTML comments are taken out first. A template over
64 KB, or over 8 KB without its comments, or one with no `#` headings, is
ignored and the factory's own layout is used.

**What's quoted can't act.** The builder's summary and the checks' output
are in code blocks they can't close, so they can't add headings, close
the `<details>`, or hide text. `#12`, issue links and `@name` in them are
spaced out, so merging closes no other issue and nobody is mentioned.

**Output is redacted whole, then cut.** The gates keep the last 256 KB of
a passing command's output, in whole lines; the factory redacts all of it
before it keeps the last lines, so a secret is never left in part. Taken
out, as `[REDACTED]`: the model keys, the values of the repo's `env`,
its services' `env` and its
[`files`](configuration.md#files-a-repo-doesnt-commit) (read through links
as they're copied), each line of a multi-line value such as a PEM key, and
the long words in those files. A value counts from six characters when
it's a model key or set under a name like `*_TOKEN`, `*_SECRET`, `*_KEY`,
`*_PASSWORD` or `*_AUTH`; any other from twelve, or eight if it mixes
cases, digits and symbols, so `HOST=localhost` doesn't blank every line
that names localhost. So are common token shapes (GitHub, GitLab, OpenAI,
Stripe, Slack and Google API keys, AWS keys, JWTs). Any line that sets an
environment variable or names a token, secret, password, key, credential
or authorization is left out whole. If one of the repo's `files` can't be
read in full (over 8 MB, or more than 2000 of them), no output is quoted
for that repo at all.

**Only where it's private, by default.** [`pr.check_output`](configuration.md#pr-descriptions)
quotes the output only on a repo the forge reports as private; on any
other the Testing section still lists each check's pass or fail. In a
workspace, each PR quotes only its own repo's checks.

**It fits the forge.** GitHub takes at most 65,536 characters; the factory
keeps to 60,000. A description over that drops the checks' output first,
then the reviewer's verdict and notes, then the blast radius and job tables (the
dashboard has them all), and only then is cut.

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
