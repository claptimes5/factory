# Factory

A single Go binary that turns labeled GitHub issues into reviewed draft PRs.
Claude Code builds, Codex reviews adversarially, and a human merges. The full
design is in [SPEC.md](SPEC.md).

```
issue labeled factory:ready
  -> triage   (Claude, read-only): complexity S/M/L, risk, or park with a question
  -> implement (Claude, in a fresh container): failing test first, then a commit
  -> gates    (daemon, deterministic): diff guard, vet, lint, test -race, build,
              and test-proves-fix (the new tests must fail on the old code)
  -> review   (Codex, read-only, sees only issue + diff + gate results)
  -> critic   (Claude, high-risk items only)
  -> draft PR with the evidence, labeled factory:awaiting-you
```

Each model tier gets `max_attempts_per_tier` attempts (2 by default), then
the item moves one tier up, then parks. A reviewer block sends the findings
back to the builder (at most two fix rounds; a third block parks). Missing or
malformed verdicts never pass. Merge is always human.

A parked item gets a comment saying why. Add the `factory:retry` label to send
it back through triage.

## Setup

You need a Linux machine (or WSL2 on Windows) with Docker and Go 1.26+, and
admin rights on the GitHub repo you want the factory to work on.

```sh
git clone https://github.com/claptimes5/factory && cd factory
CGO_ENABLED=0 go build -o factory ./cmd/factory
./factory setup
./factory serve
```

`factory setup` walks you through everything else and checks each answer
before moving on:

1. **Repositories:** which repos the factory works on, one or several. It
   updates `factory.yaml`.
2. **Docker:** checks that Docker works for you, and offers to build the job
   image the first time (a few minutes).
3. **GitHub:** creates a GitHub App for the factory for you. It gives you a
   link that opens GitHub with the app already filled in (only the
   permissions the factory needs), then asks you to install it on the repo.
   It saves the app's key and finds the IDs itself, so you copy nothing by
   hand. It then confirms it can reach the repo and offers to create the
   factory's labels. On a server you reach over SSH, setup prints the
   `ssh -L` command that lets your own browser open the link.
4. **Claude:** an API key, or a subscription token from `claude setup-token`.
5. **OpenAI:** an API key for the Codex reviewer, only if `factory.yaml` uses
   Codex (see [Claude-only review](#claude-only-review) to skip it).
6. **Phone notifications** (optional): an ntfy, Slack or Discord URL.

Your answers go in `factory.env` next to `factory.yaml`, readable only by
you, and every `factory` command loads it. Run `./factory setup` again any
time to change something; it shows the saved answers as defaults.

Then open the dashboard at http://127.0.0.1:8080/app/ and label an issue
`factory:ready`.

One thing setup can't do for you: **protect `main`**. In the repo's settings,
require a pull request with your review and the `test` and `web` checks.
`.github/CODEOWNERS` makes you the owner of `.github/`, `factory.yaml`,
`prompts/` and the lint config.

### Running it as a service

To keep the factory running across reboots, install it from a checkout in
`/opt/factory`, run as a `factory` user that can use Docker, and run setup
as that user. It gets its own settings and database there:

```sh
sudo useradd --system --create-home --groups docker factory
sudo git clone https://github.com/claptimes5/factory /opt/factory
sudo chown -R factory /opt/factory
cd /opt/factory
sudo -u factory env CGO_ENABLED=0 "$(command -v go)" build -o factory ./cmd/factory
sudo -u factory ./factory setup
sudo cp deploy/factory.service /etc/systemd/system/
sudo systemctl enable --now factory
journalctl -u factory -f     # follow its log
```

The service reads `/opt/factory/factory.env`. If you set up an older
version with `/etc/factory/env`, delete that file: values in it override
the ones setup saves.

To upgrade later, build the new binary somewhere else and hand it to
`deploy/upgrade.sh`, which keeps the old one and rolls back on failure
(see [Quality (M6)](#quality-m6)):

```sh
cd /opt/factory && sudo -u factory git pull
sudo -u factory env CGO_ENABLED=0 "$(command -v go)" build -o /tmp/factory.new ./cmd/factory
sudo deploy/upgrade.sh /tmp/factory.new
```

### Manual setup

`factory setup` is the easy way. If you'd rather do each step yourself:

1. **Build the job images:** `./factory images` (the default one is
   `docker build -t factory-job:latest deploy/job-image`).
2. **Create a GitHub App.** The factory acts on GitHub as this app, so its
   access is limited to one repo and a few permissions you choose.
   ([GitHub's guide](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app))

   1. Open [New GitHub App](https://github.com/settings/apps/new). For a repo
      owned by an organization, use
      `https://github.com/organizations/<org>/settings/apps/new` instead.
   2. **GitHub App name:** anything unique, such as `factory-<your-login>`.
      **Homepage URL:** this repo's URL.
   3. **Webhook:** untick **Active**. The factory polls GitHub, so it needs no
      public URL.
   4. **Repository permissions:** set these four and leave everything else
      at *No access*. Metadata switches to *Read-only* by itself.

      | Permission | Access | Used for |
      | --- | --- | --- |
      | Contents | Read and write | pushing `factory/*` branches |
      | Issues | Read and write | reading issues, labels and comments |
      | Pull requests | Read and write | opening and updating draft PRs |
      | Checks | Read-only | the CI panel in the dashboard |

      Leave **Workflows** at *No access*, so the factory can never edit its
      own CI. An app made without *Checks* still works, but the dashboard's
      CI panel stays empty until you add it (then accept the new permission
      on the installation page).
   5. **Where can this GitHub App be installed?** *Only on this account.*
      Click **Create GitHub App**.
   6. On the app's page, copy the **App ID** shown under *About*.
   7. Scroll to **Private keys**, click **Generate a private key**, and keep
      the downloaded `.pem` where only the daemon can read it
      ([about keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)):

      ```sh
      sudo install -D -m 600 -o "$USER" ~/Downloads/<the-downloaded-file>.pem /etc/factory/app.pem
      ```
   8. In the app's left sidebar, click **Install App**, then **Install** next
      to your account. Choose **Only select repositories**, pick the repo the
      factory works on, and click **Install**
      ([about installing](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)).
   9. You land on `https://github.com/settings/installations/<number>` (for
      an organization, `https://github.com/organizations/<org>/settings/installations/<number>`).
      That number is the **installation ID**. You can find it again later under
      [Settings → Applications](https://github.com/settings/installations) →
      *Configure*.

3. **Save the settings** in `factory.env` next to `factory.yaml`, as plain
   `NAME=value` lines with no comments after a value, and `chmod 600` it.
   Every `factory` command loads it; a variable already set in the
   environment wins. The model keys reach each job container as env vars and
   are never written into a job's files.

   ```sh
   FACTORY_GITHUB_APP_ID=123456
   FACTORY_GITHUB_INSTALLATION_ID=7890123
   FACTORY_GITHUB_APP_KEY=/etc/factory/app.pem
   ANTHROPIC_API_KEY=sk-ant-...
   OPENAI_API_KEY=sk-...
   ```

   `CLAUDE_CODE_OAUTH_TOKEN` can replace `ANTHROPIC_API_KEY`, `CODEX_API_KEY`
   can replace `OPENAI_API_KEY`, and `FACTORY_NOTIFY_URL` turns on phone
   notifications. Use `-env PATH` or `FACTORY_ENV_FILE` to keep the file
   elsewhere.

   For a quick local try you can use `GITHUB_TOKEN=<a PAT>` instead of the app.

   Codex runs on an [OpenAI API key](https://platform.openai.com/api-keys),
   billed per use; a ChatGPT subscription is not needed. Without one, review
   jobs fail and their items park. To run on Claude alone, see
   [Claude-only review](#claude-only-review).

4. **Create the labels** `factory:ready`, `factory:running`, `factory:parked`,
   `factory:retry` and `factory:awaiting-you` (`scripts/setup-labels.sh` does
   it with `gh`).

4. **Create the labels:** `scripts/setup-labels.sh <owner>/<repo>` does it
   with `gh`.

Set a spend alert on both API accounts. The daemon enforces its own caps
(`budgets` in `factory.yaml`): a per-job cap, a daily total that stops new
jobs until midnight UTC, three concurrent jobs, and no new implement jobs
while five PRs wait for you.

## Control room

`factory serve` also serves the control room on `web.listen`
(default `127.0.0.1:8080`). There is no login, so keep it on localhost or a
Tailscale address.

- **Needs you:** PRs waiting for review, parked items with the reason, budget alerts.
- **Board:** items by stage, with model and cost.
- **Job view:** live log over SSE, gate results, verdict, cost; buttons to
  retry, retry one tier up, cancel, and attach a terminal (with `ttyd`
  installed; otherwise it shows the `docker exec` command).
- **Orchestrator log** and **Spend** (today, 7 days, cost per merged PR).

Notifications go to `FACTORY_NOTIFY_URL`: an ntfy topic URL for phone push,
a Slack incoming webhook, or a Discord webhook. You get one when an item
parks, a PR is ready, the daily budget is hit, or the job image is missing.

The control room only answers to localhost names and the host in
`web.listen`. To reach it under another name (a Tailscale name, say), list it
in `web.hosts`.

## Dashboard (M7)

The dashboard is at `/app/` on the same address as the control room
(search for "Classic control room" to go back). It is a React app built
from `web/` and embedded in the binary, fonts included, so running the
factory needs no Node and loads nothing from outside. It follows the
system's dark or light setting, and the sun/moon button switches it.

- **Floor:** what needs you first: PRs ready for review, with their gates
  and a button to the PR, and parked items with the question to answer.
  Below, a column per stage. A running job's card says what it is doing,
  read from its log ("editing webhook/retry.go"), with its model, attempt
  and how long it has run. A live feed of the day and the week's numbers
  sit beside it. With several repos, the chips in the header narrow every
  screen to one. On a phone, what's building and in review comes first and
  the other stages are a tap away; a tablet gets two columns.
- **Jump to (⌘K or Ctrl+K):** any item by number or title, or any screen.
- **Live:** the feed on its own, said plainly ("PR #88 is ready for you",
  "started building with opus, attempt 2"), with the orchestrator's
  decisions marked.
- **Item:** the issue, triage's spec, jobs, audit trail, and the PR panel:
  CI checks from GitHub, gate results, the reviewer's verdict with each
  finding linked to its diff line, and Approve (on GitHub), Retry, Retry a
  tier up and Park.
- **Session:** the job's transcript live beside the issue and spec, and a
  live terminal in the job's container (xterm.js over a WebSocket; Docker
  runner only).
- **Diff:** side by side or unified, the exact patch the reviewer saw, with
  its findings inline.
- **Orchestrator:** every decision with its reason, filterable by item and type.
- **Spend:** cost per merged PR, first-pass and escalation rates per week,
  daily spend against the cap, and cost by size and model.
- **Inbox:** PRs ready for review, parked items and budget alerts. "Notify
  me in this browser" turns on browser notifications while a dashboard tab
  is open.

The JSON API is at `/api/v1/` and described in
`internal/web/openapi.yaml`; the app's types are generated from it, and a
Go test fails if the spec and the Go types drift. To work on the app:

```sh
go run ./internal/web/devserver &   # sample data on 127.0.0.1:8090
cd web && npm ci && npm run dev     # http://localhost:5173/app/
npm run build                       # regenerate types, typecheck, build into internal/web/app
npm run e2e                         # Playwright on desktop and phone sizes
```

Commit the rebuilt `internal/web/app` with your change; CI checks it is
current.

### Windows (WSL2)

Run the daemon inside a WSL2 distro with Docker available there (Docker
Desktop's WSL integration, or Docker Engine in WSL), with the repo and
`data/` under the Linux home rather than `/mnt/c`. Open
`http://localhost:8080/app/` in a Windows browser: WSL2 forwards localhost.
If that forwarding is unreliable, set `networkingMode=mirrored` in
`.wslconfig`. The `factory` CLI runs inside WSL.

## Commands

| Command | What it does |
| --- | --- |
| `factory setup` | Guided first-run setup: repo, Docker, GitHub App, labels, keys; saves `factory.env` |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage, queue depth, today's spend, recent jobs |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |
| `factory eval [-config candidate.yaml] [-set evals] [-baseline base.json] [-out report.json]` | Run the replay set and report pass rate and cost per pass; with a baseline, fail if the candidate is worse |
| `factory eval capture [-set evals] [-name NAME] [-accept CMD] PR` | Turn a merged PR into a replay case: the issue it closed, frozen where its branch started, with its tests as the hidden acceptance files |
| `factory compare ISSUE -skills a,b [-complexity M] [-no-push]` | Build one issue once per skill set and report the arms side by side |
| `factory pull ISSUE` | Fetch an item's branch into the current checkout and copy its job transcripts to `.factory/issue-N/` |

With several repos, name an issue as `owner/name#12`; with one, `12` or
`#12` is enough.
| `factory last-job`, `factory wait-job AFTER TIMEOUT`, `factory db-path` | Used by `deploy/upgrade.sh` |
| `factory mcp-bridge SOCKET` | Internal: the orchestrator's stdio MCP server, run inside its container |

## Orchestrator

With `orchestrator.enabled: true`, a short read-only Claude run plans and
steers through the daemon's MCP tools (`prompts/orchestrate.md`). It runs on
a new `factory:epic` issue, when an item's last two jobs failed, when the
queue drains with work stuck (ready with no job, or blocked on dependencies
that are not moving), every `interval`, and once a day after
`summary_hour` for the summary it sends to your notifier.

Its tools are `list_items`, `get_item`, `get_issue`, `queue_status`,
`get_job_log`, `create_issue`, `set_dependencies`, `set_priority`, `enqueue`, `retry`,
`park` and `notify_human`. The daemon checks each against policy (daily
issue cap, no dependency cycles, stage allowed for the item, review-queue
cap, budget, notification rate) and writes every call, reads included, to
the audit trail with the reason it gave; the control room's Orchestrator
log shows them. There is no tool to merge, push, close a PR or edit config.
One run at a time; extra triggers, and any that piled up while the daemon
was down, coalesce into one run. Items whose dependencies are not done wait
in `blocked` and start when the last one merges.

An epic becomes an item in stage `epic` that is never built, even if it also
carries `factory:ready`, and is done when all its children are. The
orchestrator cannot restart parked items (that stays your `factory:retry`
label), may retry each item at most twice, and may only add labels listed in
`orchestrator.labels`. Its spend has its own daily cap and does not count
against the build budget.

## Quality (M6)

- **Hidden scenarios.** Set `gates.scenarios_dir` to a directory outside the
  repository. After every other gate passes, each `name.sh` (run with sh in
  the checkout) and each Playwright `name.spec.js|mjs|ts` (with a
  `playwright.config.*` in the same directory) runs against the commit
  under test. Builders and reviewers only learn `scenario:name pass|fail`; the
  full output stays in `data/jobs/<id>/scenarios/`. Build the job image with
  `--build-arg PLAYWRIGHT=1` for browser scenarios.
- **Metrics** at `/metrics` (and `/metrics.json`): cost per merged PR by
  complexity and by the model that landed it, first-pass rate, escalation
  rate, reviewer blocks, review escapes (passed PRs you changed before merging
  or closed), and hours spent awaiting you.
- **Replay set.** `evals/<name>/case.yaml` freezes a past issue at
  `base_sha`, and `evals/<name>/accept/` holds the hidden acceptance files.
  `factory eval` runs every case through the real pipeline against a private
  copy of the repo and an in-memory GitHub (nothing is pushed or posted),
  then runs `accept` on the result. See `evals/README.md`.
- **Failures become cases.** When you merge a factory PR only after changing
  it, the daemon tells you, because what you changed is what the factory
  missed. `factory eval capture PR` turns that PR, or any merged fix, into a
  case: frozen at the commit its branch started from, with the issue it
  closed as the task and its own tests and test data as the acceptance files. Every
  later config is then measured against the miss.
- **Session resume.** With the Docker runner, an implement retry on the same
  model continues the previous attempt's Claude session (best effort; a
  missing session starts fresh).
- **Safe upgrade.** `sudo deploy/upgrade.sh NEW_BINARY` stops the daemon,
  backs up the database, swaps the binary (keeping `factory.old`), migrates,
  starts it and watches the next job. If the migration fails, the daemon
  stops, or that job fails before doing any work, it restores the old binary
  (and the database backup when the migration failed).

## Skill sets

A skill set is a folder under `skills/` holding Claude Code skills
(`<skill>/SKILL.md`). Name it on any routing entry and the daemon mounts it
read-only as the worker's `~/.claude/skills` (and `~/.codex/skills`):

```yaml
routing:
  implement.M: {harness: claude, model: sonnet, skills: tdd-strict}
```

Every job records the skill set it ran with (`factory status` shows it, and
so does the PR's job table).

**Comparing skill sets.** `factory compare 42 -skills tdd-strict,none` builds
issue #42 once per skill set (`none` means no skills), in parallel, from one
frozen copy of the base, at the same complexity tier (the item's triage, or
`-complexity`). Every arm goes through the same gates and review, but no PR
is opened. You get a side-by-side report covering the outcome, review
verdict and findings, failed gates, implement attempts, cost, time and diff.
It's saved under `data/compare/` and shown in the control room under
Compare. Each arm's branch is pushed as `factory/compare-42-<set>` (pass
`-no-push` to keep them local). Arms only see the base, never another
arm's work or an earlier branch for the issue. They split what is left of
today's budget, and their spend counts against it.

## Configuration

Everything lives in [`factory.yaml`](factory.yaml): routing per stage,
escalation tiers, timeouts, budgets, Codex token pricing, gate commands and
the diff guard. The daemon refuses a config where a reviewer could run the
same harness and model as the builder it reviews. Stage prompts are in
[`prompts/`](prompts).

### Several repositories

One factory can work on several repos. Each is worked on separately: its
own issues, branches, PRs and checks. They share the queue, the budgets,
the models and the dashboard, which can filter by repo.

```yaml
repos:
  - you/api                     # takes the top-level base_branch and gates
  - name: you/web
    base_branch: develop
    gates:                      # overrides the top-level gates for this repo
      required: [types, test]
      commands: {types: npm run typecheck, test: npm test}
```

A repo's `gates` override the top-level ones field by field. Its
`commands` replace the top-level commands; anything it leaves out, such as
the diff guard or `proves_fix`, is inherited. Set `proves_fix: none` or
`scenarios_dir: none` to turn one off for that repo.

### Workspaces: one change across repos

Some repos change together, like a Rails API and the React app that calls
it. Put them in a workspace and an issue in either one becomes a single
change across both:

```yaml
repos: [you/api, you/web]
workspaces:
  shop: [you/api, you/web]      # the order their PRs merge in
```

- The builder gets every repo of the workspace checked out side by side,
  `api/` and `web/`, on one branch named for the issue
  (`factory/web-12` for `you/web#12`), and is told each repo's checks.
- The gates run in each repo the change touches, in that repo's own image
  and with its own commands, feature map and scenarios. Their results are
  named for the repo: `api: test`, `web: build`.
- The reviewer reads one diff across the repos, every path under its
  repo's folder, and is asked to check the repos still agree.
- Each repo the change touches gets its own draft PR. The PRs link to each
  other and list the order to merge them in. Only the one that merges last
  closes the issue, so it stays open until the whole change is in.
- The item waits on you once every repo passes, and is done when every PR
  is merged. A PR closed without merging parks it.

The builder and reviewer run in the first repo's image with its `env`, so
that image needs every member's toolchain (Ruby and Node, say). Services
of all the members start together; where two name a service the same, the
first repo's wins. Triage reads only the issue's own repo.

### GitLab repos

Repos can be on GitLab, gitlab.com or self-hosted, alongside GitHub ones.
Name them by host and path:

```yaml
repos:
  - you/api                          # GitHub
  - gitlab.com/team/web              # gitlab.com
  - git.example.com/platform/infra/deploy   # self-hosted, nested groups
```

The factory works them the same way: it picks up issues labeled
`factory:ready` and opens a draft merge request for each. It reaches GitLab
with `GITLAB_TOKEN`, which `factory setup` asks for and checks against each
project. A project access token limits it to one project; for several, use
a group access token or a personal token for a bot account. The token
needs the `api` and `write_repository` scopes and the Developer role, or
Maintainer if Developers can't push to the base branch.

On GitLab a pushed branch runs its own `.gitlab-ci.yml` with the
project's runners and CI variables, so builders may never change
`.gitlab-ci.yml` or `.gitlab/`; the diff guard adds both for every GitLab
repo. A group with a dot in its name needs `host:` set, as in
`{name: my.team/web, host: gitlab.com}`.

Two differences from GitHub: GitLab doesn't record why an issue was closed,
so the readiness check never files a gap again once its issue is closed.
The dashboard's CI panel shows the commit's pipeline jobs.

### Review scaled by risk

Every item has a risk: triage rates it `low` or `high`, and a feature map
can raise it to `medium` or `high` (see below). Review gets stricter with
it, and the PR tells you how much of your attention it needs:

| Risk | Reviewers | The PR says |
| --- | --- | --- |
| low | `review` | a skim of the diff is enough |
| medium | `review.medium_risk` if set, else `review` | read the changes to the features it touches |
| high | `review.high_risk`, then a second critic (`routing.critic`, else `implement.L`) | read the whole diff, and check the features it touches by hand |

The reviewer is told the risk and holds a high-risk change to a higher
bar. Each PR carries a `factory:risk-low|medium|high` label (filter on it
to find the quick ones) and a "How to review" section that says why it got
its risk. Merging is yours at every level. To make GitHub enforce it, give
the paths behind your high-risk features a CODEOWNERS entry and require
code owner review on the base branch.

### Feature map

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
matched against the whole path (`*` stops at `/`; there is no `**`, so
use a directory for a tree).
When a change touches a feature, through its code or its tests:

- its `check` runs as a gate, `feature:<name>`, after the required checks
  (once per distinct command). A check that fails on the base branch too
  doesn't block the change if it fails the same way (same exit code,
  after the same install); the PR marks it as failing on the base branch;
- the reviewer and the PR get the **blast radius**: each feature touched,
  its risk, the files that touch it and what proves it, plus any changed
  files the map doesn't cover;
- a feature's `risk` raises the item's: a change to a `high` feature is
  reviewed as high risk (with the critic), whatever triage said;
- the builder is told the `high` features and where they live before it
  starts, and asked for a property-based test of any rule it changes in
  them (as it is for an issue triage rated high).

The map is read from the base branch, never from the change, so a builder
can't shrink its own blast radius. Builders are asked to keep it up to
date when they add or move a feature; those edits count once merged. The
readiness check reports how much of the code the map covers, entries that
point at nothing, features with no tests, and `high` features whose tests
include no property-based or fuzz test, and files the work to fix each.

### How ready a repo is

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

A **blocker** means every job would fail, or pass for the wrong reason
(tests that fail on the base branch, flaky tests, no tests at all). A
**gap** means jobs work but less of what they change is verified. Each one
comes with an issue written for the factory's builder, so the first thing
the factory does on a new repo can be making it verifiable. While a repo
has blockers, `-queue` labels only those for the factory, since every other
job would fail until they're fixed; the gaps are filed for later, and the
next `-queue` run after the blockers are fixed labels them too.

Issues it files carry `factory:readiness`. Running it again skips a gap
whose issue is still open or was closed as not planned, and files it again
if it was closed as done but came back. If the check itself can't run (no
Docker, a missing job image, a command that isn't installed, a timeout), it
stops there and files nothing. The report is saved to
`data/readiness/<owner>__<name>.json`.

### Each repo's toolchain and checks

`factory setup` reads each repo's files and suggests checks and a job image
for its stack: Rails, Ruby, Node (Vite, Next.js, React), Python, Rust or Go.
If the repo has a dev container with an `image`, the job image is built on
that. You confirm each suggestion, and setup writes it to `factory.yaml`.
For a Rails API and a Vite frontend it writes something like:

```yaml
repos:
  - name: you/api
    image_from: ruby:3.3.6-bookworm # ./factory images builds factory-job-you__api on it
    gates:
      install: bundle install
      required: [lint, test]
      commands: {lint: bundle exec rubocop, test: bin/rails db:prepare test}
      proves_fix: none              # the proves-fix check is Go-only for now
  - name: you/web                   # Node is in the default image
    env: {VITE_API_URL: http://localhost:3000}
    gates:
      install: npm ci
      required: [lint, test, build]
      commands: {lint: npm run lint, test: CI=true npm run test, build: npm run build}
      proves_fix: none
```

- **`install`** runs first on each commit's export, with the network on.
  If it fails, nothing else runs. The builder is told to run it too, along
  with the repo's own check commands.
- **Dependency cache.** A repo with an install step gets a cache at
  `/cache` (in `data/cache/<owner>__<name>`) for npm and pnpm, whose stores
  check every package against the lockfile's hashes, so one job can't plant
  a package a later job trusts. Only install may write to it. Gems
  (`vendor/bundle`), Python virtualenvs and Cargo's home are built inside
  each commit's export instead, so they start fresh every time.
- **`image_from`** is the base for the repo's job image: any Debian or
  Ubuntu image with the toolchain. `./factory images` builds the missing
  images (`--rebuild` rebuilds them all). An `image` with no `image_from`
  is one you build yourself; it needs the agent CLIs, so start it from
  `deploy/job-image/Dockerfile` with `--build-arg BASE=...`.
- **`env`** is set in the repo's agent and gate containers (top-level `env`
  applies to every repo). Agents can read it, so it is not for secrets.

`repo: you/api` is the short form for one repo. With a GitHub App, install
it on every repo (setup walks you through this). Repos under different
accounts need the app to be public, which setup sets up for you; anyone
could then install it, but only on their own repos, and its private key
stays with you. The orchestrator sees every repo and names one when it
opens an issue.

### Services: databases and the like

When a repo's tests need a database, cache or queue, list it under the
repo (or at the top level, for every repo) and the factory starts it for
each build and check:

```yaml
repos:
  - name: you/api
    services:
      postgres:
        image: postgres:16
        env: {POSTGRES_PASSWORD: factory}
        ready: pg_isready -U postgres     # run until it succeeds, before anything else
      redis: {image: redis:7, ready: redis-cli ping}
    env:
      DATABASE_URL: postgres://postgres:factory@postgres:5432/api_test
      REDIS_URL: redis://redis:6379
```

Each service is reachable by its name as the host. Every implement job,
and the readiness check, gets its own fresh set on a private network that
has no way out. The builder and every check join it, and reviewers don't.
Hidden scenarios join it as well, but they still reach nothing else. The
services are removed when the job ends, and the daemon clears any that a
crash left behind. Create the test database in the install step (for
example `bin/rails db:prepare`), because each job starts empty. Within a
job, though, the builder and every check share one set, so data one check
writes is there for the next; tests that clean up after themselves (a
transaction per test) keep that from mattering. `factory eval` gets a set
too. Services need the docker runner on Docker 28 or newer, and can't be
used with `network: host`. Don't put secrets in their `env`, since it goes
on the docker command line.

If a check fails because it can't connect to a database and the repo has
no services, the readiness check says so. It suggests the services to add,
read from the repo's `docker-compose.yml` or `config/database.yml`,
instead of filing an issue for a builder, who can't edit factory.yaml.

### Disk space

Each job checks out the repo, installs its dependencies and runs in a
container, so a full disk fails jobs halfway, usually during install,
where it reads like the builder's fault. Before starting a job the daemon
checks the free space, at most once a minute, and starts nothing while any
of these has less than `budgets.min_free_disk_gb` (default 10; negative
turns the check off):

- the data directory;
- where Docker stores containers: its data root on this machine, or, with
  Docker Desktop, whose disk lives in its own VM, the free space a
  container sees;
- under WSL, the Windows drive holding WSL's and Docker Desktop's virtual
  disks (`runner.wsl_drive`, default `/mnt/c`). Inside WSL, `df` shows the
  virtual disk's full size, not what's left on the drive under it.

You're told once when jobs stop for lack of space and again when they
start, and the control room shows a banner meanwhile.

### Claude-only review

By default Codex reviews what Claude builds, so the reviewer is never the
model that wrote the code. If you only have Claude, a different Claude model
can review instead. The reviewer's model must differ from every model the
builder can reach, including the tiers a retry escalates to, so a reviewer
on Opus means Opus cannot be a build tier. Claude models compare by family,
so `claude-sonnet-4-5` counts as `sonnet`. Change these entries in
`factory.yaml` and leave the rest:

```yaml
routing:
  implement.S:       {harness: claude, model: sonnet}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: sonnet}
  review:            {harness: claude, model: opus}
  # drop review.high_risk, or point it at opus too
escalation:
  implement: [sonnet]
```

This is weaker than cross-vendor review: two models from one family tend to
share blind spots. The rule covers the reviewer, whose approval is needed
before any PR opens. The critic that also reads high-risk diffs runs only
after the reviewer approves and can only add a block, so it uses
`routing.critic` (default: `implement.L`) even when that is the builder's
model. No OpenAI key is needed once no route names `codex`.

## Isolation

The builder runs arbitrary code, so nothing it writes is trusted:

- **It works in its own clone.** Afterwards the daemon reads that clone's
  files (never its `.git`) into a daemon-owned repository and commits them
  there. Hooks, fsmonitor commands, remotes or refs a builder plants have no
  effect on the daemon's git.
- **The daemon pushes, not the builder,** and pushes exactly the commit the
  gates verified. The token reaches git through `GIT_ASKPASS` for that one
  command; it is never on a command line, on disk, or in a container.
- **Gates run on an export** of that commit, a copy apart from the daemon's
  repository, so code under test cannot change what gets pushed. Checks
  may add files there (dependencies, build output), but a check that edits
  or deletes the commit's own files fails, so it can't get around the diff
  guard by rewriting a lint config or removing a test at run time.
- **Reviewers only count their final message.** A verdict file anywhere is
  ignored, and Claude's read-only runs ignore the checkout's settings, hooks
  and MCP servers. The diff guard also protects files that steer agents
  (`.claude/`, `CLAUDE.md`, `AGENTS.md`, `AGENTS.override.md`, `.codex/`,
  `.mcp.json`).
- **The review is pinned to the verified commit.** If the branch moved after
  the gates ran, the item parks.

## Where this differs from SPEC.md

- **The daemon pushes, not the builder** (see Isolation).
- **Checkouts are fresh per job on the host** (under `data/jobs/<id>`),
  mounted into the container and deleted when the job ends.
- **Unparking is explicit:** a `factory:retry` label, not a removed label.
- **A malformed review retries once, then parks**, so a broken reviewer
  never consumes a fix round or passes code.
- **On restart, live job containers are killed and requeued** without using
  a retry. Reattaching to them comes with container reattach (M2 recovery).
  Spend from a job killed mid-run is not recorded (the CLIs report cost only
  at the end), so the daily cap can undercount after a crash.
- **Test-proves-fix counts a compile failure as a failing test.** A new test
  that calls a function the change adds fails to build on the old code,
  which is the expected TDD shape.
- **Stuck items are reconciled:** an item in an active stage with no job for
  two minutes resumes from its last job's outcome.
- **No separate spec stage yet.** Triage parks unclear issues with a question.
- **Reviewer value is measured as review escapes** (passed PRs you changed or
  closed), since blocked versions never reach you and so can't be judged.
- **The orchestrator is off by default** in code (on in this repo's
  `factory.yaml`), since it spends on its own schedule.
- **Approve opens the PR on GitHub.** The dashboard has no GitHub login of
  yours, and the factory never approves its own PRs.
- **Browser notifications need an open tab.** There is no Web Push service;
  your notify channel (ntfy, Slack, Discord) covers a closed browser.
- **The terminal is a plain `sh` in the container**, through `docker exec`
  on the host. It connects only when you press Connect, and a shell left
  behind ends with the job's container.
- **A drained queue only triggers the orchestrator for stuck work**, once per
  distinct set of stuck items, so items waiting on your PR review don't cost
  a run every half hour.

## Status

M1 (store, claim, config), M2 (runner and adapters), M3 (the pipeline),
M4 (control room and notifications), M5 (the orchestrator) and M6 (hidden
scenarios, metrics, `factory eval`, session resume, safe upgrade) are in.
Side-by-side skill comparison, M7 (Dashboard v2), services and workspaces
across repos are in too.
