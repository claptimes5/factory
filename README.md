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

You need a Linux host with Docker and Go 1.23+.

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

## Commands

| Command | What it does |
| --- | --- |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage, queue depth, today's spend, recent jobs |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |
| `factory mcp-bridge SOCKET` | Internal: the orchestrator's stdio MCP server, run inside its container |

## Orchestrator

With `orchestrator.enabled: true`, a short read-only Claude run plans and
steers through the daemon's MCP tools (`prompts/orchestrate.md`). It runs on
a new `factory:epic` issue, when an item's last two jobs failed, when the
queue drains with work stuck (ready with no job, or blocked on dependencies
that are not moving), every `interval`, and once a day after
`summary_hour` for the summary it sends to your notifier.

Its tools are `list_items`, `get_item`, `queue_status`, `get_job_log`,
`create_issue`, `set_dependencies`, `set_priority`, `enqueue`, `retry`,
`park` and `notify_human`. The daemon checks each against policy (daily
issue cap, no dependency cycles, stage allowed for the item, review-queue
cap, budget, notification rate) and writes every call, reads included, to
the audit trail with the reason it gave; the control room's Orchestrator
log shows them. There is no tool to merge, push, close a PR or edit config.
One run at a time; extra triggers, and any that piled up while the daemon
was down, coalesce into one run. Items whose dependencies are not done wait
in `blocked` and start when the last one merges.

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
- **The orchestrator is off by default** in code (on in this repo's
  `factory.yaml`), since it spends on its own schedule.
- **A drained queue only triggers the orchestrator for stuck work**, once per
  distinct set of stuck items, so items waiting on your PR review don't cost
  a run every half hour.

## Status

M1 (store, claim, config), M2 (runner and adapters), M3 (the pipeline),
M4 (control room and notifications) and M5 (the orchestrator) are in. Next:
hidden scenarios and `factory eval` (M6), side-by-side skill comparison, and
Dashboard v2.
