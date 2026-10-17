# Factory

A single Go binary that turns labeled GitHub issues into reviewed draft PRs.
Claude Code builds, Codex reviews adversarially, and a human merges. The full
design is in [SPEC.md](SPEC.md).

```
issue labeled factory:ready
  -> triage   (Claude, read-only): complexity S/M/L, risk, or park with a question
  -> implement (Claude, in a fresh container): failing test first, then a commit
  -> gates    (daemon, deterministic): diff guard, vet, lint, test -race, build
  -> review   (Codex, read-only, sees only issue + diff + gate results)
  -> critic   (Claude, high-risk items only)
  -> draft PR with the evidence, labeled factory:awaiting-you
```

Failures retry at the same model tier twice, then one tier up, then park. A
reviewer block sends the findings back to the builder (at most two fix
rounds). Missing or malformed verdicts never pass. Merge is always human.

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

4. **Create the labels** `factory:ready`, `factory:running`, `factory:parked`
   and `factory:awaiting-you` (`scripts/setup-labels.sh` does it with `gh`).

5. **Protect `main`:** require a pull request with your review, and require
   the `ci` check. `.github/CODEOWNERS` makes you the owner of `.github/`,
   `factory.yaml`, `prompts/` and the lint config.

6. **Run it.**

   ```sh
   go build -o factory ./cmd/factory
   ./factory status        # creates data/factory.db
   ./factory serve         # or install deploy/factory.service
   ```

   Then label an issue `factory:ready`.

Set a spend alert on both API accounts. The daemon enforces its own caps
(`budgets` in `factory.yaml`): a per-job cap, a daily total that stops new
jobs until midnight UTC, three concurrent jobs, and no new implement jobs
while five PRs wait for you.

## Commands

| Command | What it does |
| --- | --- |
| `factory serve` | Run the daemon: poll GitHub, run jobs, open PRs |
| `factory run-once` | Poll once, run queued jobs to completion, exit |
| `factory status` | Items by stage, queue depth, today's spend, recent jobs |
| `factory migrate` | Apply database migrations (a backup is written first) |
| `factory backup FILE` | Write a consistent copy of the database |

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

## Where this differs from SPEC.md

- **The daemon pushes, not the builder.** The builder commits; after the
  gates pass, the daemon pushes with a token the container never sees.
- **Checkouts are fresh per job on the host** (under `data/jobs/<id>`),
  mounted into the container and deleted when the job ends.
- **A malformed review retries once, then parks**, so a broken reviewer
  never consumes a fix round or passes code.
- **On restart, live job containers are killed and requeued** without using
  a retry. Reattaching to them comes with container reattach (M2 recovery).
- **No separate spec stage yet.** Triage parks unclear issues with a question.

## Status

M1 (store, claim, config), M2 (runner and adapters) and M3 (the pipeline)
are in. Next: the control room and notifications (M4), the orchestrator
(M5), hidden scenarios and `factory eval` (M6), side-by-side skill
comparison, and Dashboard v2.
