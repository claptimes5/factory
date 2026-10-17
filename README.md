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

You need a Linux host with Docker and Go 1.26+ (Go fetches the pinned 1.27 toolchain itself).

1. **Build the job image.** It has the Go toolchain, golangci-lint and the
   pinned `claude` and `codex` CLIs.

   ```sh
   docker build -t factory-job:latest deploy/job-image
   ```

2. **Create a GitHub App** on your account with repository permissions
   *Contents: read & write*, *Issues: read & write*, *Pull requests: read &
   write*, *Metadata: read*. Do **not** grant *Workflows*, so the factory
   cannot edit its own CI. Install it on this repo and download a private key.

3. **Set the daemon's environment.** These are read by the daemon only and
   injected into each job container as env vars, never written to disk.

   ```sh
   export FACTORY_GITHUB_APP_ID=...            # app id
   export FACTORY_GITHUB_INSTALLATION_ID=...   # installation id
   export FACTORY_GITHUB_APP_KEY=/etc/factory/app.pem
   export ANTHROPIC_API_KEY=...                # or CLAUDE_CODE_OAUTH_TOKEN
   export OPENAI_API_KEY=...                   # for the Codex reviewer
   ```

   For a quick local try you can use `GITHUB_TOKEN=<a PAT>` instead of the app.

4. **Create the labels** `factory:ready`, `factory:running`, `factory:parked`,
   `factory:retry` and `factory:awaiting-you` (`scripts/setup-labels.sh` does
   it with `gh`).

5. **Protect `main`:** require a pull request with your review, and require
   the `ci` check. `.github/CODEOWNERS` makes you the owner of `.github/`,
   `factory.yaml`, `prompts/` and the lint config.

6. **Run it.**

   ```sh
   CGO_ENABLED=0 go build -o factory ./cmd/factory   # static: the orchestrator's container runs it too
   ./factory status        # creates data/factory.db
   ./factory serve         # or install deploy/factory.service
   ```

   Then label an issue `factory:ready`.

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

The dashboard is at `/app/` on the same address as the control room (the
"Classic view" link goes back). It is a React app built from `web/` and
embedded in the binary, so running the factory needs no Node.

- **Board:** items in columns by stage, with a live dot when a job runs,
  model, cost, PR, and a needs-you or blocked badge. On a phone the columns
  stack, with what needs you first.
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
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage, queue depth, today's spend, recent jobs |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |
| `factory eval [-config candidate.yaml] [-set evals] [-baseline base.json] [-out report.json]` | Run the replay set and report pass rate and cost per pass; with a baseline, fail if the candidate is worse |
| `factory compare ISSUE -skills a,b [-complexity M] [-no-push]` | Build one issue once per skill set and report the arms side by side |
| `factory pull ISSUE` | Fetch an item's branch into the current checkout and copy its job transcripts to `.factory/issue-N/` |
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
the diff guard. The daemon refuses a config where the builder and the
reviewer use the same harness. Stage prompts are in [`prompts/`](prompts).

## Isolation

The builder runs arbitrary code, so nothing it writes is trusted:

- **It works in its own clone.** Afterwards the daemon reads that clone's
  files (never its `.git`) into a daemon-owned repository and commits them
  there. Hooks, fsmonitor commands, remotes or refs a builder plants have no
  effect on the daemon's git.
- **The daemon pushes, not the builder,** and pushes exactly the commit the
  gates verified. The token reaches git through `GIT_ASKPASS` for that one
  command; it is never on a command line, on disk, or in a container.
- **Gates run on a read-only export** of that commit, so code under test
  cannot change what gets pushed.
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
Side-by-side skill comparison and M7 (Dashboard v2) are in too.
