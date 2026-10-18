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
| **Item** | The issue, triage's spec, jobs, audit trail, and the PR panel: CI checks from GitHub, gate results, the reviewer's verdict with each finding linked to its diff line, and Approve (on GitHub), Retry, Retry a tier up and Park. |
| **Session** | The job's transcript live beside the issue and spec, and a live terminal in the job's container (xterm.js over a WebSocket; Docker runner only). |
| **Diff** | Side by side or unified, the exact patch the reviewer saw, with its findings inline. |
| **Orchestrator** | Every decision with its reason, filterable by item and type. |
| **Spend** | Cost per merged PR, first-pass and escalation rates per week, daily spend against the cap, and cost by size and model. |
| **Inbox** | PRs ready for review, parked items and budget alerts. "Notify me in this browser" turns on browser notifications while a dashboard tab is open. |
| **Settings** | Budgets, models, escalation, auto-merge, the orchestrator and intake, saved to `factory.yaml`. See below. |

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
- **Orchestrator** and **Intake**: whether the orchestrator runs, its
  budgets, how often it runs, and how often intake polls.

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

You get a notification when an item parks, a PR is ready, the daily budget
is hit, or the job image is missing. With the orchestrator on, it sends a
daily summary there too.

The daemon also tells you when builds stop for
[low disk space](configuration.md#disk-space) and start again, and when you
merge a factory PR only after changing it (see
[Failures become cases](quality.md#failures-become-cases)).

The dashboard's Inbox can also show browser notifications, but only while
a dashboard tab is open.

## Commands

Every command takes `-config factory.yaml` and `-env factory.env` before
the command name to use other files.

| Command | What it does |
| --- | --- |
| `factory setup` | Guided first-run setup: repo, Docker, GitHub App, labels, keys; saves `factory.env` |
| `factory images [--rebuild]` | Build the missing job images; `--rebuild` rebuilds them all |
| `factory readiness [-file\|-queue] [REPO]` | Measure how ready each repo is (see [How ready a repo is](quality.md#how-ready-a-repo-is)) |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage, queue depth, today's spend, recent jobs |
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
