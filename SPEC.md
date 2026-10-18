# Software Factory Spec

Oct 17, 2026 · @Someone

## Overview

The factory is a single Go binary that turns labeled GitHub issues into verified draft PRs. Claude Code builds, Codex reviews adversarially, and a human merges.

**Goals**

- Run unattended on one Linux host: issues in, reviewed draft PRs out.
- Use stock harnesses only — Claude Code and Codex CLI, driven headless. No custom agent loop.
- Pick the model per task by complexity, and escalate on failure.
- Make every decision inspectable: who ran, which model, what it cost, why it stopped.

**Non-goals (v1)**

- Multi-host workers, Kubernetes, or a hosted SaaS control plane.
- Automatic merge to the default branch. Merge is always human.
- Integrations beyond GitHub and one chat notifier.

**Design principles**

1. **Few dependencies.** One Go binary (SQLite, web UI, MCP server embedded), Docker, `gh`, and the two CLIs inside the job image.
2. **The daemon is deterministic; agents decide.** Agents propose. The daemon enforces budgets, concurrency, and permissions.
3. **Every run starts fresh.** State lives in SQLite and GitHub, never in an agent's conversation.
4. **The writer never grades its own work.** Review uses a different model, fresh context, and read-only access.
5. **Fail closed.** A missing gate or an unparseable verdict blocks the job; it never passes silently.

Stage procedures (triage, spec, implement, verify) are adapted from [addyosmani/factory](https://github.com/addyosmani/factory); restart and retry behavior borrows from [openai/symphony](https://github.com/openai/symphony).

## Architecture

&#91;embedded content: architecture · daemon, SQLite, job containers\]

GitHub events and the schedule feed the daemon, which starts one container per job and records results in SQLite. The orchestrator steers only through the daemon's MCP tools; only the builder can push a branch, and only you merge.

## Pipeline and state machine

&#91;embedded content: work item states · 7 stages, 1 review loop\]

A work item moves through these stages; each arrow is one job. Triage can also park an issue that is missing facts, with the question posted on the issue.

**Job states** (one row in `jobs`): `queued → running → succeeded | failed | cancelled | timed_out`.

- A failed implement job retries at the same model tier up to 2 times, then one tier up, then the item is parked.
- A crashed or hung container is killed at its timeout and counts as one failed attempt.
- On daemon restart, `running` jobs with no live container go back to `queued`.
- Only the daemon changes states. Agents report through `verdict.json`; the daemon decides the transition.

## SQLite schema

One database file, WAL mode, `busy_timeout=5000`, and a single writer goroutine in the daemon. Workers never touch the database; they report through the daemon's API.

```sql
CREATE TABLE work_items (          -- one per GitHub issue
  id            INTEGER PRIMARY KEY,
  repo          TEXT NOT NULL,
  issue_number  INTEGER NOT NULL,
  title         TEXT NOT NULL,
  stage         TEXT NOT NULL,     -- triage|spec|ready|implementing|reviewing|awaiting_human|done|parked|blocked
  complexity    TEXT,              -- S|M|L (set by triage)
  risk          TEXT,              -- low|high
  priority      INTEGER DEFAULT 0,
  parent_id     INTEGER REFERENCES work_items(id),  -- epic decomposition
  branch        TEXT,
  pr_number     INTEGER,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE(repo, issue_number)
);

CREATE TABLE dependencies (
  item_id       INTEGER NOT NULL REFERENCES work_items(id),
  depends_on_id INTEGER NOT NULL REFERENCES work_items(id),
  PRIMARY KEY (item_id, depends_on_id)
);

CREATE TABLE jobs (                -- one agent run
  id            INTEGER PRIMARY KEY,
  item_id       INTEGER REFERENCES work_items(id),  -- NULL for orchestrator runs
  kind          TEXT NOT NULL,     -- orchestrate|triage|implement|review|critic|verify|...
  state         TEXT NOT NULL,     -- queued|running|succeeded|failed|cancelled|timed_out
  harness       TEXT NOT NULL,     -- claude|codex
  model         TEXT NOT NULL,
  effort        TEXT,
  attempt       INTEGER NOT NULL DEFAULT 1,
  priority      INTEGER DEFAULT 0,
  container_id  TEXT,
  budget_usd    REAL NOT NULL,
  cost_usd      REAL DEFAULT 0,
  tokens_in     INTEGER DEFAULT 0,
  tokens_out    INTEGER DEFAULT 0,
  verdict       TEXT,              -- JSON from the stage's structured output
  error         TEXT,
  log_path      TEXT,
  queued_at     TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT
);
CREATE INDEX jobs_queue ON jobs(state, priority DESC, queued_at);

CREATE TABLE events (              -- append-only audit trail
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  actor     TEXT NOT NULL,         -- daemon|orchestrator|job:<id>|human:<login>
  item_id   INTEGER,
  job_id    INTEGER,
  type      TEXT NOT NULL,
  reason    TEXT,
  data      TEXT                   -- JSON
);

CREATE TABLE gate_results (
  job_id    INTEGER NOT NULL REFERENCES jobs(id),
  gate      TEXT NOT NULL,         -- types|lint|test|build|audit|scenarios
  status    TEXT NOT NULL,         -- pass|fail|misconfigured
  detail    TEXT,
  PRIMARY KEY (job_id, gate)
);

CREATE TABLE spend (               -- rollup for budget checks
  day       TEXT PRIMARY KEY,
  cost_usd  REAL NOT NULL DEFAULT 0
);
```

**Claiming a job** is one atomic statement, so no queue library is needed:

```sql
UPDATE jobs SET state = 'running', started_at = :now
WHERE id = (
  SELECT id FROM jobs WHERE state = 'queued'
  ORDER BY priority DESC, queued_at LIMIT 1
)
RETURNING *;
```

Back up with Litestream, or a nightly `sqlite3 factory.db '.backup'`. Keep the SQL portable so a later move to Postgres is mechanical.

## Harness adapters and model routing

Adapters only shell out to the stock CLIs and parse their JSON output — about 100 lines each. There is no custom agent loop.

```go
type Harness interface {
    Run(ctx context.Context, req RunRequest) (<-chan Event, error)
}

type RunRequest struct {
    Stage     string            // triage|spec|implement|review|verify|orchestrate
    Model     string
    Effort    string            // optional reasoning effort
    Prompt    string            // rendered from the stage template
    Workdir   string            // the job's worktree inside the container
    ReadOnly  bool              // reviewers and the orchestrator
    Env       map[string]string // short-lived tokens only
    Timeout   time.Duration
}

type Event struct {            // normalized from each CLI's JSON stream
    Kind    string             // text|tool_call|tool_result|usage|final|error
    Payload json.RawMessage
}
```

| Harness | Invocation | Output parsed |
| --- | --- | --- |
| Claude Code | `claude -p "<prompt>" --model <m> --output-format stream-json --verbose` with `--permission-mode` set per stage | stream-json events; final result + usage |
| Codex | `codex exec --json --model <m> --sandbox <read-only or workspace-write> "<prompt>"` | JSONL events; final message + usage |

Exact flags change between CLI versions, so the adapter pins the CLI version in the job image and has a smoke test per harness.

**Structured verdicts.** Every stage ends by writing `verdict.json` in the workdir (schema per stage). The daemon validates it. Missing or invalid = job failed.

**Routing config** (`factory.yaml`, version-controlled):

```yaml
routing:
  orchestrate:       {harness: claude, model: opus}
  triage:            {harness: claude, model: haiku}
  spec:              {harness: claude, model: opus}
  implement.S:       {harness: claude, model: sonnet}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: opus}
  review:            {harness: codex,  model: <strongest codex model>}
  review.high_risk:  {harness: codex,  model: <strongest codex model>, effort: high}
  verify:            {harness: claude, model: sonnet}

escalation:
  implement: [sonnet, opus]   # after max_attempts_per_tier failures, move up one tier
  max_attempts_per_tier: 2
  then: park                  # top tier fails -> park for a human
```

Rules:

- Triage sets `complexity` and `risk`; routing reads them. Nothing else picks models.
- Keep the reviewer strong even when the builder is cheap.
- The reviewer must differ from the builder in harness or, failing that, in model at every tier the builder can escalate to. The daemon refuses a config where they could match. Cross-harness review is the default.

## Orchestrator agent

The orchestrator is a short-lived, headless Claude Code run (Opus-tier) that plans and steers through daemon tools. It never edits code, pushes, or merges. The daemon checks every request against policy before acting.

**Responsibilities**

- Decompose epics into child issues with dependencies and complexity hints.
- Choose what runs next, avoiding parallel jobs that touch the same modules.
- Handle stuck or failing jobs: rewrite the spec, split the task, escalate the model, or park it.
- Turn builder–reviewer disagreements into new issues or scenario tests.
- Write the daily summary: shipped, blocked, spend, and what needs a human.

**Tools** (served by the daemon as an MCP server over a Unix socket mounted into the orchestrator's container):

| Tool | Effect | Daemon checks |
| --- | --- | --- |
| `list_items`, `get_item`, `queue_status`, `get_job_log` | Read state | none |
| `create_issue(title, body, parent, labels)` | Opens a GitHub issue + work item | daily issue cap |
| `set_dependencies(item, depends_on[])` | Writes `dependencies` | rejects cycles |
| `set_priority(item, n)` | Reorders the queue | refuses 100 and up, kept for main health fixes |
| `enqueue(item, stage, complexity?)` | Queues a job | stage allowed for item's state; concurrency and review-queue caps; budget; an item waiting on its cost estimate, or one a new complexity puts over `confirm_over` |
| `retry(job, tier?)` | Requeues, optionally one model tier up | max attempts; budget |
| `park(item, reason)` | Moves to `parked`, notifies a human | none |
| `notify_human(message, item?)` | Sends to the notifier | rate limit |

Not available to it: merge, push, close PRs, edit `factory.yaml` or the charter, mark anything verified.

**When it runs.** The daemon starts a fresh orchestrator job on:

- a new issue labeled `factory:epic`;
- any job failing twice in a row;
- the queue draining while ready items exist;
- a schedule: every 60 minutes, plus 08:00 local for the daily summary.

Only one orchestrator job runs at a time; extra triggers coalesce. Each run starts with no memory and reads state through tools. Every tool call is written to `events` with `actor = orchestrator` and the stated reason, so the control room can show why it acted.

**Budget.** A per-run cap (default $3) and a daily cap (default $15) for orchestration alone, separate from build spend.

## Verification and adversarial review

A PR reaches a human only after deterministic gates pass and a different model, with fresh context, fails to block it.

**1. Gates (deterministic).** `gates.sh` runs types, lint, tests, build, and audit, then prints one verdict line the agent may not paraphrase. A required gate with no configured command reports `MISCONFIGURED` and fails the job.

**2. Test-proves-fix check.** The verifier reverts the implementation, keeps the new test, and confirms the test fails. A test that passes without the fix blocks the job.

**3. Adversarial review (Codex).** Runs in its own container with a read-only checkout. Input is only the spec, the diff, and gate results — never the builder's summary. It must write:

```json
{
  "decision": "pass | block | questions",
  "findings": [
    {"severity": "critical|major|minor|pre-existing", "file": "...", "line": 0,
     "claim": "...", "evidence": "..."}
  ],
  "earlier": [{"id": "F1", "ruling": "resolved|upheld|withdrawn", "reason": "..."}],
  "challenged_assumptions": ["..."]
}
```

- `block` (on its critical and major findings, or its minor ones alone) or any `critical` finding sends the job back to implement with the findings attached. A `pre-existing` finding never blocks.
- **Verified before it blocks.** Each blocking finding first goes to a cheaper verifier (`routing.verify`, never a builder's model), which confirms or refutes it against the code. Only confirmed findings block; refuted ones stay as notes on the PR. A verifier that can't run (budget, error, no ruling) fails safe: the finding blocks.
- **Tracked to its end.** Every finding gets an id that holds across rounds. The builder answers each blocking one (`fixed`, `wrong`, `out_of_scope`); the next review sees the answers and rules on each (`earlier`), and an upheld finding blocks again. Findings and outcomes live in `review_findings`.
- **Re-review what changed.** From round two, a reviewer reads the diff since the commit it last reviewed, with its open findings, and the whole change as context only when small.
- At most two fix-and-review rounds. A third block parks the item for a human.
- `high` risk items also get a Claude critic pass, so two independent reviewers see them (side by side with `review.paired_high_risk`). `medium` (raised by the feature map) uses `review.medium_risk` when set. The reviewer is told the risk, and the PR carries a risk label and a "How to review" section scaled to it.

**4. Hidden scenarios.** A `scenarios/` folder outside the builder's checkout holds end-to-end checks (HTTP, CLI, or Playwright). The verify stage runs them; builders never see their contents, only pass/fail.

**5. Human review.** The draft PR carries the evidence: gate verdict, reviewer JSON, scenario results, cost, and models used.

Disagreements logged by the reviewer feed the orchestrator, which proposes new scenarios from them.

## Sandboxing, secrets, budgets

Each job runs in a fresh container that is destroyed when it ends; the host and other jobs are out of reach.

**Containers**

- One image per repo: toolchain, pinned `claude` and `codex` CLI versions, `gh`, `git`.
- Fresh container per job; the repo is cloned into it at the job's base SHA (no shared worktrees on the host).
- Limits: CPU, memory, disk, and a wall-clock timeout per stage (defaults: triage 10 min, implement 60 min, review 20 min).
- Network: egress allowlist only — the model APIs, GitHub, and the package registries the repo needs. Implemented with a dedicated Docker network and an egress proxy.
- Reviewers and the orchestrator get read-only mounts and no push credentials.

**Secrets**

- GitHub: a GitHub App; the daemon mints an installation token per job, scoped to one repo, expiring in 1 hour.
- Model credentials: read by the daemon from a file or env on the host (or a secret manager later) and injected per job as env vars. Never baked into images or written to the repo.
- Job logs are scrubbed of known secret values before storage.

**Budgets** (all enforced by the daemon, not the agents)

| Limit | Default | On breach |
| --- | --- | --- |
| Per job, by complexity | S $2 · M $6 · L $15 | Kill job, mark failed |
| Daily total | $60 | Stop dequeuing until midnight; notify |
| Concurrent jobs | 3 | Queue waits |
| Items awaiting human review | 5 | Stop starting new implement jobs |

Cost per job comes from the usage events each CLI emits; the daemon sums them into `jobs.cost_usd` and `spend`.

## Control room and notifications

The control room is server-rendered HTML with htmx, embedded in the binary, served on localhost or over Tailscale. No Node build.

**Pages**

- **Needs you** (home): items awaiting review, parked items with reasons, budget alerts.
- **Board**: work items by stage, with dependencies and the model each used.
- **Job view**: live log streamed over SSE, gate results, reviewer verdict, cost; buttons to retry, retry one tier up, cancel, park.
- **Attach**: joins the running container's tmux session in a browser terminal (ttyd or xterm.js) for live steering.
- **Orchestrator log**: every orchestrator action with its stated reason.
- **Spend**: today, last 7 days, cost per merged PR.

**Auth.** Bind to localhost by default; remote access through Tailscale or an SSH tunnel. No public port in v1.

**Notifications** go to one configured channel (Slack webhook, Discord webhook, or ntfy for phone push):

- item needs human input or is parked;
- PR ready for review;
- daily budget hit;
- daemon or job image unhealthy;
- the orchestrator's 08:00 summary.

**Local handoff.** `factory pull <item>` fetches the branch and the job's transcript so you can continue locally (for example with `claude --resume`). Treat it as best effort in v1.

### Dashboard v2 (post-MVP, milestone M7)

A richer web app in the style of Agent Orchestrator's desktop UI replaces the htmx pages once the daemon is stable. It is still served by the same Go binary: the frontend is built once and embedded with `go:embed`, so Node is a build-time tool only.

**Stack.** React + Vite + TypeScript, Tailwind with shadcn/ui components, TanStack Query for data. The daemon exposes a JSON API plus the existing SSE event stream; generate TypeScript types from an OpenAPI spec so the two never drift.

**Screens**

- **Board:** work items as cards in columns by stage, with live status, model, cost, and a blocked/needs-you badge.
- **Session view:** live terminal (xterm.js over WebSocket) attached to the job's container, beside the issue, spec, and agent transcript.
- **PR panel:** CI checks, gate results, the Codex verdict with findings linked to diff lines, and approve, retry, or park buttons.
- **Diff viewer:** side-by-side diff (diff2html or Monaco) with reviewer comments inline.
- **Orchestrator feed:** every orchestrator decision with its reason, filterable by item.
- **Spend and metrics:** cost per merged PR, first-pass rate, and escalation rate over time.
- **Notifications center:** needs-input, ready-for-review, and budget alerts, with browser push.
- **Mobile layout:** board, approvals, and notifications usable on a phone.

**Auth.** Still localhost or Tailscale only; add a login before any public exposure.

The factory builds this itself: file each screen as its own issue once M6 is done, with Playwright screenshot checks as the verify stage.

## Restart and recovery

SQLite and GitHub hold all state, so a restart loses at most an agent's in-progress session; the daemon reconciles on startup. Recovery features arrive with the milestone that needs them.

| Milestone | Recovery feature | Behavior |
| --- | --- | --- |
| M0 | Stale-label sweeper | Scheduled workflow resets `factory:running` issues older than the job timeout so they are picked up again |
| M1 | Durable store | SQLite in WAL mode; forward-only migrations; backup before each migration |
| M2 | Container reattach | On startup, each `running` job: container alive → reattach logs; exited → read exit code and `verdict.json`; gone → requeue without using a retry |
| M2 | WIP commits | Agents commit and push work in progress, so a retry resumes from the branch, not from scratch |
| M3 | GitHub catch-up | One poll at startup picks up labels, comments, and PRs missed while down |
| M3 | Graceful shutdown | SIGTERM stops claiming; running jobs get a 10-minute grace, then are marked `interrupted` |
| M5 | Orchestrator coalescing | Triggers queued during downtime fire as one fresh orchestrator run |
| M6 | Session resume | Claude session files on a mounted volume enable `claude --resume` on retry (best effort) |
| M6 | Safe self-upgrade | Back up DB → drain → swap binary (keep old one) → migrate → watch one job → roll back on failure |

Budget totals live in `spend`, so daily caps hold across restarts.

## Metrics and evals

The number that matters is cost per merged PR; everything else explains it.

**Recorded per job:** harness, model, effort, attempt, tokens, cost, wall time, gate results, reviewer decision, and final outcome (merged, closed, parked).

**Derived views**

- Cost per merged PR, by complexity tier and by model.
- First-pass rate: share of implement jobs that pass gates and review without a retry.
- Escalation rate: share of items that needed a higher model tier.
- Reviewer value: review escapes (passed PRs a human changed or closed), and reviewer precision: the share of blocking findings that led to a fix, per reviewer route and model.
- Rounds to pass: review rounds per item that passed review.
- Human time proxy: hours items sat in `awaiting_human`.

**Evals (replay set)**

- Keep 20–50 past issues with known-good merged fixes, each frozen at its base SHA with its acceptance test.
- `factory eval --config <routing.yaml>` runs them all in sandboxes and reports pass rate, cost, and time.
- Run it before changing any model, prompt, skill, or routing threshold. A change that lowers pass rate or raises cost per pass is rejected.

**Tuning loop.** Monthly, the orchestrator reads these metrics and proposes routing or prompt changes as a PR to `factory.yaml`. A human approves it like any other PR.

## M0 Bootstrap: a factory that builds itself

M0 is GitHub Actions workflows plus a few shell scripts — no daemon code. It uses the official, maintained actions for both harnesses, then builds the Go daemon (M1 onward) as its own first issues. Estimated 2–3 days.

**Why Actions first.** Actions already provides what the daemon would: issue-label triggers, a fresh runner per job, secrets, logs, and `concurrency:` groups. [anthropics/claude-code-action](https://github.com/anthropics/claude-code-action) runs Claude Code on your own runner and supports structured JSON outputs. [openai/codex-action](https://github.com/openai/codex-action) runs `codex exec` with controlled privileges; its inputs map to `codex exec` options and it returns the final message as an output ([docs](https://developers.openai.com/codex/github-action)).

**Workflows**

1. `implement.yml` — on `issues: labeled factory:ready` (and `factory:fix`). One job at a time via `concurrency: factory`.
   1. Checkout; branch `factory/issue-<n>`.
   2. `claude-code-action` with `.github/factory/implement.md`, fixed model (Sonnet), max-turns cap. Write a failing test first; stop at a commit.
   3. Verification steps (below) run as separate workflow steps, outside the agent.
   4. `gh pr create --draft` with gate output, models, and turn count in the body; label `factory:review`.
2. `review.yml` — on `pull_request` from `factory/*` branches.
   1. `codex-action` with a read-only sandbox, `.github/factory/review.md`, and a JSON output schema. Input: issue text, diff, gate results only.
   2. Validate `verdict.json` with `jq`. Missing or malformed = block.
   3. `pass` → label `factory:awaiting-you`. `block` → post findings, relabel the issue `factory:fix` (max 2 rounds, counted by a label), then `factory:blocked`.

**Verification in M0** (all deterministic scripts in `.github/factory/checks/`, run by the workflow, never by the agent)

- [ ] **Re-run gates:** `go vet`, `go test -race ./...`, `golangci-lint`. Exit codes decide; the agent's claims are ignored.
- [ ] **Test proves fix:** revert non-test files, keep new tests, require at least one new test to fail.
- [ ] **Diff guard:** block deleted/renamed tests, new `t.Skip` or `//nolint`, lint-config edits, protected-path edits, or more than 400 changed lines.
- [ ] **Coverage floor:** touched packages may not drop below `main`'s coverage.
- [ ] **Strict reviewer contract:** schema-validated verdict; reviewer receives gate results so it cannot approve red code.
- [ ] **Self-test:** once the Go daemon exists (issue 1), every PR also runs it in `--dry-run` with a stub harness against `testdata/fixture-repo`, end to end.

**Built-in protection.** The default `GITHUB_TOKEN` cannot push changes to workflow files, so if the agent's job uses only that token, the factory cannot edit its own workflows. Confirm this for any GitHub App token you give the action, and withhold its workflows permission. Add CODEOWNERS for `.github/` and `factory.yaml`, and branch protection requiring your review on `main`.

**Budgets in M0.** Max-turns and job timeouts per step, one concurrent job, and a spend alert on both API accounts. True dollar caps arrive with the daemon.

**Runner.** Start on GitHub-hosted runners. Move to a self-hosted runner on your box if Actions minutes get expensive; the workflows don't change.

**M0 is done when** the factory produces 3 PRs to its own repo that you merge without editing.

**First issues to feed it** (one PR each; together they become M1–M5):

1. Go module skeleton: SQLite store, job table, atomic claim, `factory status`
2. Docker job runner with limits and timeouts
3. Claude and Codex CLI adapters with `verdict.json` validation
4. Polling intake from GitHub labels, then webhooks
5. Triage stage that sets S/M/L and risk
6. Model routing and escalation from `factory.yaml`
7. Budget enforcement (per-job and daily dollar caps)
8. htmx control room with SSE logs, plus ntfy/Slack notifications
9. Orchestrator MCP tools, then the orchestrator stage

When issue 7 lands, switch intake from Actions to the daemon; the Actions workflows stay as the fallback.

## Reuse before building

No maintained open-source project covers this design end to end with Claude Code and Codex. The well-tested parts to reuse are the two official GitHub Actions and the factory method; Symphony ports exist but are young.

| Project | Use it for | Maturity | Verdict |
| --- | --- | --- | --- |
| [anthropics/claude-code-action](https://github.com/anthropics/claude-code-action) | M0 builder: Claude Code in Actions, structured outputs | Official, v1 | Use in M0 |
| [openai/codex-action](https://github.com/openai/codex-action) | M0 reviewer: `codex exec` in Actions with privilege controls | Official, v1 | Use in M0 |
| [addyosmani/factory](https://github.com/addyosmani/factory) | Stage prompts, `gates.sh`, verifier and critic agents, charter | \~230 stars, has smoke tests | Copy skills and gates |
| [openai/symphony](https://github.com/openai/symphony) | `SPEC.md` for the daemon's poll, retry, and restart logic | 21k stars; reference only, not maintained as a product | Read the spec |
| [nnndv/symphony-code](https://github.com/nnndv/symphony-code) | Bun + Claude CLI + `gh` poller with SSE dashboard and `--dry-run` | 0 stars, small, has tests | Read for ideas |
| [logicalicy/symphony](https://github.com/logicalicy/symphony) | Notes on a TypeScript Claude Code port: budget guards, andon cord, epoch locks on labels | Fork, 0 stars | Read for ideas |
| [dagger/container-use](https://github.com/dagger/container-use) | Per-agent containers + git branches over MCP, written in Go | Dagger-backed, early development | Option for M2 sandboxes |
| [AgentWrapper/agent-orchestrator](https://github.com/AgentWrapper/agent-orchestrator) | Local supervisor with CI and review feedback routing | 8.6k stars, mid-rewrite | Not a base: local-only, weak isolation |

**Recommendation.** Ship M0 from the two official actions plus addyosmani/factory's skills and gates. Use Symphony's `SPEC.md` as the requirements for issue 4 (intake and retries). Evaluate container-use before writing the Docker runner in issue 2.

## Build plan

The first useful milestone, an unattended issue-to-draft-PR loop, is M3, roughly three weeks in for one person working with agents.

| Milestone | Scope | Done when | Est. |
| --- | --- | --- | --- |
| M1 Skeleton | Go module, SQLite schema + migrations, single-writer store, job claim, `factory.yaml` loader | Unit tests for claim race and state transitions pass | 3–4 days |
| M2 Sandbox + adapters | Job image, container runner with limits and timeout, Claude and Codex adapters, `verdict.json` validation | One manual job runs each harness in a container and records cost | 4–5 days |
| M3 Pipeline | GitHub App, webhook intake, triage → implement → gates → Codex review → draft PR, routing + escalation, budgets | A labeled issue becomes a reviewed draft PR with no human action | 1 week |
| M4 Control room | htmx pages, SSE logs, attach terminal, notifier | You can watch, retry, and park from a phone | 4–5 days |
| M5 Orchestrator | MCP tool server, orchestrator stage, triggers, epic decomposition, daily summary | An epic becomes ordered child issues that run | 4–5 days |
| M6 Quality | Hidden scenarios, Playwright verify stage, metrics views, `factory eval` | Replay set runs and reports cost per pass | 1–2 weeks |

**Open questions**

- Language: Go assumed (single binary, pure-Go SQLite). Elixir or Rust only if you already know them well.
- Host: home server or VPS? This decides the egress-proxy setup and how the control room is reached.
- Tracker: GitHub Issues only, or Linear later?

**Prompt to start Claude Code**

```text
Read SPEC.md (this document). Implement milestone M1 only.
Constraints: Go 1.26+, modernc.org/sqlite (no cgo), standard library
where possible, no web framework. Write tests first for the job-claim
statement and the work-item state machine. Stop when `go test ./...`
passes and summarize what is left for M2.
```
