# Software Factory Playbook: turning an existing web app repo into a verified, agent-driven factory

> This folder is how Factory's own repository is developed, not documentation for using Factory. For that, see [docs/using.md](../using.md).

**Audience:** Claude Code, working inside an existing web application repository, with a human supervising.
**Location:** `docs/factory/PLAYBOOK.md` in the target repo. This file is the source of truth; progress lives in `docs/factory/PROGRESS.md`.

## Quickstart (for the human)

1. Copy this folder's contents into the repo root so you have `docs/factory/PLAYBOOK.md` and `docs/factory/PROGRESS.md`. Commit them on a branch.
2. Open Claude Code in the repo (plan mode is fine for Step 0) and paste:

   > Read `docs/factory/PLAYBOOK.md` and `docs/factory/PROGRESS.md`. Resume at the first step not marked passed (Step 0 if none). Follow the operating protocol exactly. Stop after each step and report using Appendix A.

3. Answer Step 0's questions, then reply **"next"** after each step you accept. To resume in a new session later, paste the same prompt.

## Resume protocol (for the agent, at the start of EVERY session)

1. Read this file and `docs/factory/PROGRESS.md`. The last entry with `STATUS: passed` marks the last finished step; work on the one after it. If PROGRESS.md has a `blocked` entry, address that first.
2. **Re-run the previous step's gate** before building on it (the repo may have drifted since). If it fails, say so and fix that first, without advancing.
3. Check `git status` and the current branch; never start a step with unrelated uncommitted changes.
4. Then follow the operating protocol below. Do not rely on memory of earlier sessions; the repo and PROGRESS.md are the record.

---

## 0. What we are building (and what we are not)

A "software factory" here means: **a repo where agents can take work from intent to a merged change, and every claim of "done" is backed by evidence that something independent of the author checked.** The factory's configuration (agent instructions, hooks, skills, CI, review rules) lives in the repo as code, reviewed like any other change.

We are **not** aiming for full automation. The goal is the highest-quality software shipped quickly and cheaply, with human attention spent only where humans are needed. We build **bottom-up**: nothing downstream is trusted until the layer beneath it is verified. Order matters: verification first, automation last.

## 1. Operating protocol (applies to EVERY step)

You work on **one step at a time**. Each step has a **Goal**, **Actions**, a **Gate** (a concrete, runnable check), and **Exit artifacts**. Follow this loop:

1. **Restate** the step's goal and gate in 3 lines. Use plan mode for anything touching more than a few files. Explore first, then plan, then code.
2. **Make the smallest change** that could satisfy the gate.
3. **Run the gate yourself and show the real output.** "It should work" is not evidence. Prefer running a single targeted test over the full suite while iterating; run the full gate at the end.
4. **If the gate fails, diagnose before editing.** Decide which of three things is wrong: the *task* (unclear/wrong goal), the *context* (you lacked information), or the *code*. Fix that, rerun.
5. **Round cap: 3 attempts.** After three failed attempts, stop. Report what you tried, the failing output, and your diagnosis. Do not thrash. (A reviewer/author agent pair can loop for dozens of rounds without converging; the cap forces a step back.)
6. **Negative control ("gate the gate").** Once the gate passes, deliberately break the thing it protects (e.g. introduce a type error, flip an assertion, break a route) and confirm the gate goes **red**, then revert the break. A gate that cannot fail is not a gate.
7. **Checkpoint.** Commit small with a descriptive message. Append to `docs/factory/PROGRESS.md`: step number, date, gate command, result, evidence (paths/links), decisions made, open questions.
8. **Stop and report in 10 lines or fewer.** Do **not** start the next step until the human says "next" (unless they explicitly authorize autonomous mode for the remaining steps; even then, never skip the gate). From Step 13 onward this stop-and-wait rule applies to changes to the factory itself (hooks, CI, `.claude/`, review and risk config, thresholds); product work follows the Step 13 autonomy rules.

### Hard rules (never violate without explicit human approval in this session)

- **Never weaken a check to make it pass.** No skipping/deleting tests, loosening thresholds, adding broad ignores, `--no-verify`, widening timeouts to hide flakiness, or rewriting assertions to match buggy output. (The same failure mode shows up in AI-written proofs: the cheap way to "pass" is to change the thing being checked. Treat any edit to a gate, test expectation, or threshold as a *flagged change* that you call out explicitly in your report.)
- **Protected paths** (the human lists them in Step 0; typical: auth, billing/payments, DB migrations, CI workflow files, release/deploy config, secrets handling, `.claude/` hooks and permissions): never edit without asking first.
- **Never put secrets** in CLAUDE.md, prompts, logs, or screenshots.
- **Never install third-party skills, plugins, MCP servers, or hooks without human review.** They run with your permissions. Show the human the full source (every `SKILL.md`, script, and hook) and the reason, and get approval first. Copy only the specific pieces needed rather than whole plugins, pin the version/commit, and commit them through a normal reviewed change. See Appendix D.
- **Keep context lean.** Context fills quickly and quality drops as it fills. Delegate wide exploration and verification to subagents; keep the main session for decisions.
- **Evidence over assurance.** Every "done" claim names the command run and shows its output, or links a screenshot/artifact.
- **Ask when it's a real decision** (scope, risk tolerance, spending, anything irreversible). Otherwise pick the conventional default, say so, and proceed.

### Why this ordering

- Giving the agent something it can run to check its own work is the single highest-leverage habit; do it before anything else.
- Hooks are deterministic; CLAUDE.md instructions are advisory. Put "must happen every time" things in hooks and CI, and "should know" things in CLAUDE.md.
- The team behind Claude Code reports that most review should happen *before* a human is asked: verified by the author-agent, reviewed by a fresh-context agent, then CI, then risk-scaled human approval, then staged release with fast rollback. We build those layers in that order.

---

## 2. Step map

| # | Step | Layer | Done when (short) |
|---|------|-------|-------------------|
| 0 | Orient and baseline | Foundation | Baseline doc + human-confirmed constraints |
| 1 | One-command local verification | Foundation | `verify` passes clean, fails on injected breaks |
| 2 | Lean CLAUDE.md | Foundation | Fresh session can verify using only CLAUDE.md |
| 3 | Deterministic guardrails (hooks, permissions) | Foundation | Hook demonstrably blocks a violation |
| 4 | Required CI mirrors local verify | Gate | Bad PR blocked, good PR green |
| 5 | Characterization + critical-path tests | Verification | Tests catch seeded regressions, 0 flakes in 5 runs |
| 6 | Browser E2E + web quality gates | Verification | E2E green 5x in CI, catches seeded UI break |
| 7 | Agent self-verification protocol | Verification | Sample task yields an evidence package |
| 8 | AI code review + security review | Review | Seeded-bug eval: caught, with acceptable noise |
| 9 | Risk-tiered approval and ownership | Review | Protected-path PR demonstrably needs a human |
| 10 | Safe ship and rollback | Delivery | Rollback rehearsed and timed |
| 11 | First automations: chores | Automation | N runs, good accept rate, clear owners |
| 12 | First feature through the full loop | Automation | One small feature merged with full evidence |
| 13 | Graduate to poteto-mode (autonomy by exception) | Automation | 3 pilot PRs, hostile tests contained, kill switch works |
| 14 | Measure and improve (factory as code) | Operation | Metrics ledger, one scored experiment, circuit breaker |
| 15 | Scale carefully | Operation | Only after 12 to 14 are solid |

Optional side track: **Step 5b** (stronger guarantees on critical logic).

---

## Step 0: Orient and baseline (read-only)

**Goal:** Understand the repo and agree on constraints before changing anything.

**Actions**
1. Read-only inventory (use a subagent for breadth): language/framework, package manager, monorepo or not, frontend/backend split, database and migrations, build/dev/test/lint/typecheck commands, existing CI, existing tests and their state, deploy target, env vars and secrets handling (names only, never values), existing `CLAUDE.md`/`.claude/`.
2. Run what exists and record reality: does it install, build, start? Do tests pass? How long does each take? Which tests are flaky or skipped? Record numbers, not impressions.
3. Ask the human **at most 5 questions**, in one message: (a) protected paths; (b) deploy target and CI provider; (c) what needs secrets/services to run tests (DB, auth provider, third-party APIs); (d) the 3 to 5 user flows that must never break; (e) who approves changes, and any compliance constraints.
4. Write `docs/factory/BASELINE.md` (stack, commands, baseline numbers, protected paths, critical flows, known problems). If `docs/factory/PROGRESS.md` does not exist, create it with a header and the table in Appendix E.

**Gate:** Every command listed in BASELINE.md was actually executed and its result recorded; the human has confirmed protected paths and critical flows.
**Exit artifacts:** `BASELINE.md`, `PROGRESS.md`. **No source changes in this step.**

---

## Step 1: One-command local verification

**Goal:** A single command (`npm run verify`, `make verify`, or equivalent) that runs install-check, format check, lint, typecheck, unit tests and production build, and is the same thing everyone (human, agent, CI) runs.

**Actions**
1. Add the `verify` script composed from existing commands; fix only what's needed to make it reproducible (pin versions, add `.nvmrc`/toolchain file, make env needs explicit via `.env.example`).
2. Make failures loud: non-zero exit codes, clear output. Add a fast variant (`verify:quick`: lint + typecheck + affected unit tests) for the inner loop.
3. Do not add new tests yet. Existing failing or flaky tests: quarantine them *visibly* (a tracked list in BASELINE.md with owners), never silently.

**Gate**
- From a **fresh clone** (use a temp directory or clean worktree), following only documented commands, `verify` passes.
- Run it **twice** in a row: same result both times.
- **Negative controls:** introduce (a) a type error, (b) a lint violation, (c) a failing assertion in an existing test, (d) a build-breaking import. Each must make `verify` fail. Revert each.

**Exit artifacts:** `verify` script, timing recorded in PROGRESS.md.

---

## Step 2: Lean CLAUDE.md

**Goal:** Give every future session just the context it cannot infer from the code.

**Actions**
1. Run `/init` to draft, then **prune hard.** Include: exact commands (`verify`, `verify:quick`, how to run a single test, how to start the dev server), environment quirks, non-obvious gotchas, architecture in a paragraph (pointers to docs instead of copies), code conventions that differ from defaults, non-goals, protected paths, and the rule "run `verify` before declaring done and show the output."
2. Exclude anything derivable by reading code, file-by-file descriptions, and platitudes ("write clean code"). Aim for short (rule of thumb: under ~100 lines); link out to `docs/` for detail.
3. If a rule keeps being ignored, don't add emphasis; convert it to a hook (Step 3) or a lint rule.

**Gate:** Start a **fresh session** (new context) with a trivial task ("add a comment to X and prove the repo still verifies"). It must find and run the right commands and report real output using only CLAUDE.md. Any question it needed to ask means CLAUDE.md is missing something; add that and retest.

---

## Step 3: Deterministic guardrails (hooks and permissions)

**Goal:** Move "must always happen" rules from advice to enforcement.

**Actions**
1. **Hooks** (committed in `.claude/settings.json`): auto-format/lint on file edit; block edits to protected paths (exit with a clear message); block dangerous commands (force push, `rm -rf` outside the workspace, `--no-verify`); a stop/pre-commit check that runs `verify:quick`.
2. **Permissions:** allowlist read-only and verification commands (tests, lint, typecheck, build, git status/diff) so verification never stalls on prompts; keep writes, network calls, and destructive commands gated.
3. Store the hook scripts in the repo and treat `.claude/` as a protected path from now on.

**Gate:** Demonstrate each hook firing: attempt an edit to a protected file (blocked), attempt a commit with a lint error (blocked), and run a normal verification command (no prompt). Show the output of each.

---

## Step 4: Required CI mirrors local verification

**Goal:** The same gate, enforced where it cannot be skipped.

**Actions**
1. CI workflow runs the same `verify` command (plus any cross-platform/browser matrix needed). Cache dependencies; keep it fast.
2. Branch protection on the default branch: required checks, no direct pushes, linear history if desired.
3. PR template with three required fields: **Testing** (how was this verified, with evidence), **Risk** (what could go wrong, which areas), **How to revert**.
4. Add a **merge queue** or "require branch up to date" so two individually green PRs can't combine into a broken main.

**Gate (use throwaway PRs):** (a) a PR with a deliberate failing test is red and unmergeable; (b) a clean PR is green; (c) direct push to default branch is rejected. Close the throwaway PRs. Record links.

---

## Step 5: Characterization and critical-path tests

**Goal:** A safety net around behavior that must not break, **before** agents change things at speed.

**Actions**
1. From BASELINE.md's critical flows, list the modules/endpoints behind each. Write **characterization tests** (capture current behavior, even if imperfect) for the riskiest, least-tested ones first: auth, payments, data mutations, permissions, input parsing, API contracts.
2. Unit/integration level first (fast, deterministic). Use fixtures or a disposable test database; no shared mutable state; no real third-party calls (mock at the boundary or record/replay).
3. Add API contract tests (schemas/types at service boundaries) where frontend and backend meet.
4. Coverage is a **guide, not the gate.** Use it to find holes in critical modules; don't chase a global percentage.

**Gate**
- All new tests pass **5 times consecutively** (flakiness check), locally and in CI.
- **Seeded-regression check:** for each critical module, introduce 2 realistic bugs (off-by-one, wrong permission check, dropped field, swapped condition). The tests must fail for at least most of them. Report the catch rate; tighten tests for any miss. Revert the seeds.

### Step 5b (optional): stronger guarantees on critical logic

Where correctness matters most (pricing, permissions, parsing, serialization, state machines): add **property-based tests** (round-trip encode/decode, idempotence, invariants), **fuzz** the parsers/validators, enable strict types and runtime schema validation at boundaries. Note the research context: LLMs are making formal proofs far cheaper, and one effective trick is to treat an existing test/fuzz suite as the specification, then prove it always passes; but today's large-scale results are for compiled C/C++ libraries, not web apps, so for a web app the practical analog is the property/fuzz tooling above. Either way, "verified" never means "bug-free": the guarantee is only as good as the spec and the assumptions about the environment, so state them.
**Gate:** property tests run in CI and a seeded invariant violation is caught.

---

## Step 6: Browser end-to-end verification and web quality gates

**Goal:** Check the *composed* app the way a user meets it. Unit tests build their own world; many real failures live in composition (a component that passes its tests but never mounts, a renamed form field, hydration silently failing, a stale API schema).

**Actions**
1. Install Playwright (or the repo's existing E2E tool). Create a **deterministic seed** (known test user, known data, resettable state). One test per critical flow from BASELINE.md (login, core create/read/update flow, checkout/payment path if any, permission boundaries).
2. Use role- and text-based locators, not brittle CSS paths. Capture **screenshots/traces/video on failure and for key steps**, uploaded as CI artifacts.
3. Add web quality gates with thresholds (e.g. a Lighthouse-style audit on 3 to 5 representative pages: accessibility, performance, best practices) that fail the build below the thresholds. Start with thresholds just under current scores, then ratchet up; never lower them without approval.
4. Give the agent a **browser tool** (Playwright MCP or equivalent) for *exploratory, per-change* verification. Rule of thumb: **recurring checks become committed tests; one-off, per-change checks stay as directed sessions** where the agent re-finds elements by role/text. Pre-approve read/observe actions; keep form submissions to real external systems gated.
5. Optional: Playwright's planner/generator/healer agents can bootstrap test coverage. Caution (this playbook's own judgment, not a claim from the sources): auto-"healing" selectors can mask a real regression as a locator problem, so a human reviews every healed-test diff.
6. Stop at auth walls (captcha, 2FA); that's a human step by design.

**Gate**
- E2E suite green **5 consecutive CI runs**; record median runtime.
- **Seeded UI breaks** (hide a button, break a route, rename a field, break an API response shape): E2E must fail on each.
- Failure artifacts (screenshot/trace) are visible in the CI run.

---

## Step 7: Agent self-verification protocol

**Goal:** "Done" from an agent arrives as an **evidence package**, checked by something other than the author.

**Actions**
1. Create a `/verify` skill (in `.claude/skills/`) describing the protocol: run `verify`; run the relevant E2E flows; for UI changes drive the real app in a browser and capture before/after screenshots; confirm each acceptance criterion with a command or observation; list anything *not* verified.
2. Create a **fresh-context verifier subagent** (read-only tools plus the browser). It receives the task description and the diff, not the author's reasoning. Keeping browser-driving out of the coding agent's context also lets the write-verify loop run many more times before context pressure bites.
3. Extend the PR template: Evidence section (commands + output, screenshots, verifier summary).
4. Also create `/simplify` and `/bug-hunt`-style review skills only if you find a recurring need; don't build speculatively.
5. Before hand-writing the verification skill, you may use the pstack accelerator in Appendix D (D1) as a starting point, subject to the third-party rule in the hard rules.

**Gate:** Run a **small real task** (a copy change plus one small behavior change) end-to-end. The result must include: author's `verify` output, E2E results, UI screenshots, and an independent verifier report that **cites what it observed**. Then seed a subtle defect in a second sample task: the verifier must flag it.

---

## Step 8: AI code review and security review

**Goal:** Every PR is read deeply by machines before a human is asked.

**Actions**
1. Add an automated reviewer on PRs (the Claude Code GitHub Action / Code Review, plus `/code-review` locally) and a **separate security review** pass. Require that findings are **resolved (fixed or closed with a one-line reason)** before merge; open findings block.
2. Write `REVIEW.md` (short): what failure means in each area, severity labels, caps on nitpicks, paths to skip, "cite file and line as evidence," "say plainly when there is nothing to fix," and the risk list from BASELINE.md.
3. **Make review converge:** cap at 3 rounds, then the author reconsiders the approach; after the first passes, re-review only the new diff; flag the *class* of bug so the author fixes all instances; no repeating already-posted findings.
4. Start on **one repo/area**, tune for a couple of weeks, then widen.

**Gate: build a small review eval.** Create 5 PRs with realistic seeded bugs (include one security issue and one subtle logic bug) and 3 known-clean PRs. Report: recall (seeded bugs caught), false-positive rate (comments on clean PRs that would lead to a harmful change), and rounds-to-pass. Only proceed if recall is acceptable to the human and noise is tolerable. Keep this set: **every future incident or revert adds a case to it**, and every change to review prompts/models is rescored against it.

---

## Step 9: Risk-tiered approval and ownership

**Goal:** Spend human attention where risk is.

**Actions**
1. Write the tiers down in `docs/factory/RISK.md`, **set by the change, not the author**:
   - Low (docs, copy, internal-only, additive behind a flag): may merge on green automated checks once the human enables it, which at first they should not.
   - Medium (ordinary product changes): one human approval; the person who directed the agent counts.
   - High (auth, billing, migrations, CI/CD, secrets, release config): always a human; consider two.
2. `CODEOWNERS` + branch protection so protected paths always require the right human. **An agent's approval never counts as a human's.** Dismiss approvals when new commits land.
3. Start strict. Loosen only with evidence from the metrics in Step 14.

**Gate:** Throwaway PRs: one touching a protected path (cannot merge without the owner), one docs-only change, one push of a new commit after approval (approval dismissed). Record results.

---

## Step 10: Safe ship and rollback

**Goal:** Mistakes will still get through, so make them cheap to find and undo.

**Actions**
1. **Preview deployment per PR**, so verification and humans see the real thing.
2. **Feature flags** for new behavior; flag-off works without a deploy.
3. Staged release if the platform supports it (canary/internal first); error monitoring and basic health alerting wired to a place a human will see it.
4. A **documented one-step rollback** (revert + redeploy, or flag off).

**Gate:** **Rehearse a rollback** on a harmless change in a non-production environment (or production if the human approves): time it, record the steps and any surprise. Verify the monitoring would have surfaced a deliberately introduced error.

---

## Step 11: First automations: chores only

**Goal:** Build trust and muscle memory with low-risk, high-signal work, **not** a backlog-eating loop.

**Actions**
1. Pick 1 to 3 chores with clear owners, e.g.: dependency-update PRs that must pass the full gate; issue triage/repro (label and reproduce, no code); changelog/docs updates; CI-failure summaries; flaky-test reports.
2. Each automation: trigger → agent → output with a **named owner** who reviews it. Run through the same CI and review layers; no special lanes.
3. Do **not** start by auto-fixing the whole backlog: PRs that nobody reviews turn a team against the factory.

**Gate:** After ~10 runs per automation, report accept rate (merged or acted on vs ignored), noise (outputs a human discarded), and cost per run. Proceed only if the human judges signal-to-noise good. Fix or retire the rest.

---

## Step 12: First feature through the full loop

**Goal:** Prove the whole loop on something **simple and low-stakes**, not the scariest part of the app.

**Loop:** intent → (triage/repro if a bug) → **spec** (human reviews) → implement → author self-verification (Step 7) → AI review + security review (Step 8) → CI → human approval per tier (Step 9) → merge queue → flagged release (Step 10) → monitor.

**Actions:** Choose a small user-facing feature or bug with a clear acceptance test. Write acceptance criteria as **runnable checks** *before* implementing. Keep humans at the points the research says matter: spec, risky code, and reading the evidence.

**Gate:** One change merged with a complete evidence trail, and the human states afterward what they would have wanted to check but couldn't from the evidence. Each such gap becomes a new check, skill, lint rule or CLAUDE.md line (fix recurring problems at the source, not by repeating review comments).

---

## Step 13: Graduate to poteto-mode (autonomy by exception)

**Goal:** Run pstack's `/poteto-mode` with its shipped defaults for product work, with the factory doing the blocking instead of a chat prompt. Humans are consulted by exception: at PR approval per risk tier (enforced by CODEOWNERS and branch protection), on poteto-mode's own always-pause list, and on the harness paths.

**Why here:** poteto-mode's defaults ("never block on the human", proceed on reversible work, use any connected tool without asking) are only safe when the actions the agent must not take are blocked mechanically. By Step 12 those layers exist and a full loop has been proven with a human watching. Before this step, autonomy would depend on the agent restraining itself; after it, the environment does. It sits before measurement on purpose: autonomous runs generate the volume Step 14 needs to measure, and Step 14 is what justifies loosening tiers further.

**Prerequisites (re-run each gate; do not assume):** hooks and denies (3), CI, branch protection and merge queue (4), critical-path and E2E tests (5, 6), the repo's `verify-<app>` skill and fresh-context verifier (7), AI review with the seeded-bug eval (8), risk tiers and CODEOWNERS (9), rollback (10), and one full loop (12). poteto-mode also expects `control-ui`, `control-cli` and `deslop` from Cursor's separate `cursor-team-kit` plugin, which Claude Code does not have. Map `control-ui` and `control-cli` to the repo's `verify-<app>` skill, drop `deslop`, and list any other missing dependency with what you did about it.

**Where poteto-mode's defaults meet this playbook, and how each is resolved**

| poteto-mode default | Playbook rule | Resolution at graduation |
|---|---|---|
| Starts working without waiting for confirmation; never blocks on the human | Stop after each step and wait for "next" | Retired for **product work**. Still applies to any change to the factory itself (see the harness-path row). |
| Uses any connected tool; external actions (chat, tickets) proceed without asking | Writes and network calls are gated (Step 3) | The permissions allowlist defines "any". Widen it one reversible, observable action at a time, with a hook test for each. Customer-facing messages stay denied. |
| Always pauses for force-push to shared branches, deploys, data deletion, customer messages | Hooks block force push, `rm -rf`, `--no-verify` | Convert the whole always-pause list into hook or permission denies, including deploys and production data writes, so it is enforced, not remembered. |
| Asks only for genuine product or preference calls; otherwise runs a prototype or applies a default and reports it | Ask on real decisions | Aligned. In unattended runs the agent applies the default and records it under "Decisions made" in the PR description and the decision log. |
| Edits whatever the task needs | Protected paths: never edit without asking | **Product protected paths** (auth, billing, migrations, secrets handling): the agent may open a PR, but CODEOWNERS requires the owner and merge is impossible without them. **Harness paths** (`.claude/`, CI workflows, `REVIEW.md`, `RISK.md`, gate thresholds, vendored pstack skills): hard-blocked by hook in autonomous sessions. Changes to them come only from a human-driven session and a human-approved PR, because a harness the agent can edit is a harness nobody has verified. |
| Long autonomous runs ("run until done") | Round cap of 3 attempts | Keep the cap. After 3 failed attempts the agent stops, writes its findings on the PR or an issue, and labels it `needs-human`. It does not keep looping. |
| Autopilot-full and Shipping playbooks merge PRs on their own | Merges follow the Step 9 tiers; auto-merge only for the low tier and only when the human enables it | Out of scope for this step. Merges stay with the merge queue and tier approvals. Revisit the Autopilot playbooks after Step 14 shows low reverts. |
| Heavy fan-out (`how`, `architect`, `arena`, `interrogate`) on non-trivial work | Cost matters | Record cost per PR from the first pilot. Small edits skip the heavy playbooks. |

**Actions**
1. Re-run every prerequisite gate and the canary (a deliberately broken PR that must be rejected). Stop if any is red.
2. Turn the always-pause list into denies and test each one by attempting the action and showing it blocked. Add denies for deploys and production data writes.
3. Run autonomous work in an isolated git worktree on a branch, never on the default branch.
4. Vendor the pstack skills poteto-mode needs into `.claude/skills/` per Appendix D, resolve the Cursor-to-Claude-Code mappings inside the copy, and record the pstack version. This is a reviewed PR, since `.claude/` is protected.
5. Add a short Autonomy section to CLAUDE.md: poteto-mode is allowed for product work; harness paths are hard-blocked; escalation means a `needs-human` label on the PR, not a chat question; how to switch autonomy off.
6. Write and test a **kill switch**: the documented way to disable autonomous runs (revoke the added allowlist entries and disable the plugin). Run it once.
7. Pilot on 3 small, low-stakes tasks (one bug fix, one refactor, one small feature), each started as `/poteto-mode <goal and how to verify it>`. The human does not intervene mid-run and reviews only at PR approval.
8. **Hostile test (the negative control for autonomy).** Give poteto-mode four tasks built to tempt it: (a) the fastest path edits a harness file, (b) the fastest path needs a force-push, (c) the fastest path weakens a test, (d) the task hinges on an ambiguous product decision. Each must end blocked or escalated, not violated. Record which mechanism stopped each.

**Gate**
- The 3 pilot PRs each carry the Step 7 evidence package and pass the Step 8 reviewers, with the human acting only at tier approval.
- Zero hook bypasses. Hostile tests (a) to (d) are all contained, with the stopping mechanism named for each; any task that got through is a failed gate and becomes a new hook or check.
- The kill switch works when tested.
- Report mid-run interventions (target 0 per task, report the actual count), cost per PR, and what the human wanted to do outside PR approval. Each such item becomes a new check, hook, skill or CLAUDE.md line.

**Exit artifacts:** hook denies, CLAUDE.md Autonomy section, vendored skills with version headers, pilot report in PROGRESS.md.

---

## Step 14: Measure and improve (factory as code)

**Goal:** Make the factory a measured, improvable system rather than vibes.

**Actions**
1. **Factory as code:** everything that governs agents is in the repo and changes via PR: `CLAUDE.md`, `REVIEW.md`, `.claude/` (hooks, skills, agents, permissions), CI workflows, PR template, `RISK.md`. For a small team, this repo-committed setup is the factory; a hosted platform is optional later.
2. **Metrics ledger** (a script or a doc updated weekly): share of merged PRs that went through the factory; throughput; cycle time; cost per PR; human approvals per PR and human wait time; **revert and incident rate as the guardrail** (if it rises, human time was moved, not saved); review rounds-to-pass; review recall/noise from the eval set.
3. **Incidents become evals:** every revert or incident adds a case to the Step 8 eval set (confirmed bug + where it lived). Rescore the reviewer on every change to its prompts, skills, or model.
4. **Experiments:** change one thing (model choice, review depth, a skill), run it against the eval set and a sample of past tasks, compare quality and cost, keep it only if it wins.
5. **Circuit breaker.** Define a threshold on revert and incident rate (agreed with the human) above which autonomy tightens automatically or by one documented command: autonomous runs off, approval tiers up one level. Test it by simulating a breach.
6. **Verify the harness itself.** Field studies of agent-built codebases found the results verified while the harness around them was mostly unenforced. So: keep gate configs in protected paths, and run a periodic **canary** (a deliberately broken PR that must be rejected) to confirm every layer is still alive.
7. **Optional: skill review loop.** Monthly, and once on the Step 13 pilot sessions, run the vetted `skill-doctor` (Appendix D, D6), scoped to this repo. Use only its failed-conversation findings and drafted skill edits, and ignore the overall grade. Each suggested edit lands as a human-approved PR (`.claude/skills/` is protected) and is kept only if it improves the Step 8 eval set or measured cost per PR.

**Gate:** A ledger with at least two weeks of data, one completed experiment judged against the eval set, one successful canary run, and a circuit breaker that was exercised once.

---

## Step 15: Scale carefully

Only after Steps 12 to 14 are solid:
- Parallel agents in separate git worktrees, scheduled/background agents, more repos or services, work intake from chat and issue trackers (meet people where they work), cloud-hosted agent runs for unified traces and security.
- Expect new bottlenecks: environments that are hard to reproduce, harder attribution of what helped, cost growth on bigger tasks, security/audit needs, PR pile-ups. Define explicit policy for what gets human review versus agentic verification.
- Encourage rather than mandate adoption; celebrate shipped work, not token spend. Track adoption first, then throughput, cycle time, cost per PR.
- Relax approval tiers only where the evidence (low reverts, low incidents) supports it.

---

## Appendix A: Report template (use at the end of every step)

```
STEP N: <name>   STATUS: passed | blocked
Gate command(s): <exact commands>
Result: <real output summary, or link to artifact>
Negative control: <what I broke, what went red, reverted: yes>
Flagged changes: <any edit to a gate/test expectation/threshold, with reason; or "none">
Decisions made: <list>
Open questions for human: <list or "none">
Next step proposed: N+1
```

## Appendix B: Troubleshooting a stuck step

- **Gate keeps failing:** is it the task, the context, or the code? Narrow the scope; use a subagent to explore; reproduce with the smallest failing test.
- **Flaky tests:** never retry-until-green. Find the shared state, time, or network dependency and remove it; quarantine visibly while fixing.
- **Agent ignores a rule:** convert to hook/lint/CI. Prune CLAUDE.md of low-value lines.
- **Context bloat:** start a fresh session; point it at `PROGRESS.md` and the step; delegate exploration.
- **Review loops forever:** apply the round cap, switch to diff-only, flag the bug class.
- **Verification is slow:** run targeted tests while iterating, full gate once; split fast/slow checks; cache in CI.

## Appendix C: Sources this playbook draws on

- Warp, "7 core components for building a software factory": https://www.warp.dev/blog/7-ways-to-build-a-software-factory (factory config as code, shared history, benchmarks, artifacts, end-to-end verification)
- Warp, "Tactics for rolling out software factories": https://www.warp.dev/blog/rolling-out-software-factories (start with chores, owners, signal-to-noise, adoption metrics)
- Warp, "Adopting the software factory model: crawl, walk, run": https://www.warp.dev/blog/adopting-the-software-factory-model-crawl-walk-run (prove a loop on a simple surface first; triage→spec→implement→review→verify→monitor)
- claude.dev, "How the Claude Code team does code review": https://claude.dev/blog/solving-code-review-at-anthropic/ (verify before PR, fresh-context review, REVIEW.md, risk tiers, round caps, incidents as evals, rollback)
- Anthropic, "LLMs are making formal software verification feasible at a much larger scale": https://www.anthropic.com/research/formal-verification (tests as specs, checker-as-trust-anchor, reward-hacking caution, "verified is not bug-free")
- Rajiv Pant, "Your coding agent should watch its own work run": https://rajiv.com/blog/2026/06/27/your-coding-agent-should-watch-its-own-work-run/ (browser verification, committed gates vs directed sessions)
- Ranger, "Autonomous Coding Loops": https://docs.ranger.net/concepts/autonomous-loop (separate verification from coding to protect context)
- Claude Code best practices (verification, CLAUDE.md contents, hooks vs advisory instructions; read via mirrors): https://learn-anthropic.pages.dev/en/articles/engineering/claude-code-best-practices
- NeurIPS 2026 field study, "Results Are Verified, the Harness That Shaped Them Is Not": https://neurips.cc/virtual/2026/172963
- Playwright agents (planner/generator/healer): https://dev.to/majdizlitni/playwright-agents-the-architecture-of-self-healing-2b97
- pstack v0.15.36 by Lauren Tan, read from the installed plugin source (see Appendix D for what was and was not reviewed): https://github.com/cursor/plugins/tree/main/pstack
- Flavio Copes, "A deep dive into pstack" (secondary overview; differs from the source on `interrogate` output): https://flaviocopes.com/pstack
- Warp, `skill-doctor` SKILL.md (provided by the user; scripts and rubrics not reviewed)

---

## Appendix D: Optional third-party accelerators (pstack, skill-doctor; adopt late and selectively)

**Status:** Reviewed from the installed plugin source: pstack v0.15.36, MIT license, by Lauren Tan (https://github.com/cursor/plugins/tree/main/pstack). Read in full: the manifest, `poteto-mode`, `create-verification-skill`, `maintain-verification-skill`, `interrogate`, the Claude Code adapter (`docs/harness/claude-code.md`), and the Feature, Bug fix and Refactoring playbooks. **Not audited:** the other ~50 skills, and the scripts in `poteto-mode/scripts/` (only grepped; they shell out to `git` and `gh`). The plugin ships no hooks and no MCP servers: it is skills plus two subagents (`pstack:poteto-agent`, `pstack:comment-sicko`). pstack assumes none of this playbook's foundation (CI, hooks, risk tiers, rollback), so vendor individual skills only after Steps 1 to 6 pass, and run `/poteto-mode` itself only at Step 13.

**Ground rules**
1. **This playbook wins until Step 13.** Do not run Steps 0 to 12 under `/poteto-mode`. Its autonomy defaults (proceed on reversible work without asking, "never block on the human", use any connected tool without asking) conflict with stop-after-each-step and the protected-path rule. Before Step 13, invoke one pstack skill for one job inside a step, then return to the playbook. Step 13 is where `/poteto-mode` runs with its shipped defaults for product work, once hooks, branch protection and CODEOWNERS enforce what the human used to; its table lists how each conflict is resolved. Changes to the factory itself always follow the stop-and-report protocol.
2. **A user-level plugin install is not part of the repo.** Teammates and CI do not have it, and a plugin update can change behavior silently. For anything the factory depends on, vendor the specific skill folder into `.claude/skills/`, record the pstack version in a header comment, and land it through a reviewed PR (`.claude/` is protected). Whether Claude Code can pin a plugin in project settings was not verified; check current docs before relying on it.
3. **pstack is written for Cursor.** Each skill tells the agent to read `docs/harness/claude-code.md` first. That adapter maps `Task` to `Agent`, `.cursor/skills/` to `.claude/skills/`, and runs any non-Claude model slug on the session model, so multi-model diversity shrinks to Claude models only. Cursor-only features (`/loop`, Bugbot, Automations) are skipped with a one-line note. When vendoring, resolve these mappings inside the copy so it does not depend on a file outside the repo.
4. **The useful skills run only when named** (`disable-model-invocation: true`). In Claude Code, type `/pstack:<name>`.
5. **`/setup-pstack` writes `~/.claude/rules/pstack-models.md`,** which loads into every Claude Code session on that machine, in every repo. Skip it unless you want that; without it, roles fall back to the session model.
6. Every adopted skill passes the usual gate: run it on a real small task, show the output, run a negative control (seed a defect it should catch), and record token cost against the hand-written alternative.

**What each piece actually does, and where it fits**

| ID | Step | pstack piece (from source) | Fit and caveats |
|----|------|----------------------------|-----------------|
| D1 | 7 (after 6) | `create-verification-skill`: interviews the repo (surface, run, drive, observe, isolate), then writes a project-local `verify-<app>` skill (Launch, Doctor, Drive, Evidence, Cleanup, Helpers) plus a feature map of the top 3 to 5 features. It must then run its own output once: launch, doctor, drive one feature, capture evidence, clean up, and confirm the evidence survived cleanup. Proof standards: real user path (no test-only endpoints), verify side effects, evidence outlives teardown. `maintain-verification-skill`: parallel read-only source readers per feature, one live pass over every feature, at most one PR of proven doc or harness fixes, never edits product code. | **Best fit.** Writes to `.cursor/skills/` by default, so map to `.claude/skills/verify-<app>/`; that is a protected path, so the human approves. It produces a directed-session skill, not a committed gate: recurring checks still belong in Step 6 Playwright tests. It has no fresh-context verifier subagent; Step 7 still adds one. |
| D2 | 5, 12 | **Bug fix** playbook: reproduce yourself on the real surface first, every shipped line traces to runtime evidence, failing repro committed before the fix, failing-then-passing output pasted. **Refactoring** playbook: pin behavior first (characterization test, snapshot, or equivalence harness; "type check and lint are not a pin"), move in small steps that keep the pin green, revert if reader load did not drop. | **Good fit** and consistent with Step 5. Ignore its Cursor model assignments and `/loop` references. |
| D3 | 8 | `interrogate`: states the intent, spawns adversaries (via the arena skill's worktree fan-out) told to break the change and prove each finding with a run; the coordinator re-runs every finding's command and drops anything not reproduced. Output: Intent, Findings, Dropped. Reports only, applies no fixes. | **Local pre-PR supplement** for risky or contested changes; the prove-it-by-running rule is a strong anti-noise filter. It is not a CI check and does not block merges, so it does not replace the Step 8 required reviewer and security review. Score it on the Step 8 seeded-bug eval and compare cost. |
| D4 | 12 | **Feature** playbook: `how` over the subsystem, `architect` for parallel design options, delegate code to a subagent, verify on the matching surface, small ordered commits, `interrogate` if the design is contested. | **Heavy.** It mandates delegation and the arena skill whenever multiple valid shapes exist, and it is tuned to Cursor model roles. Try only after D2 is proven; consider the lighter parts only. |
| D5 | 14 | `show-me-your-work` (decision-log TSV), `reflect`, Hillclimb playbook. | **Lead only, not read.** Proposals must wait for human approval and land as PRs; metrics still come from the Step 14 ledger. |

**D6 (not pstack): Warp's `skill-doctor`, Step 14, optional.** Grades recent local agent sessions on efficiency, code quality, procedure compliance and verbosity, then drafts edits to skills from the failing sessions only, into a scratch directory outside the repo. **Reviewed:** only its `SKILL.md`. Its scripts (`collect_sessions.py`, `render_report.py`), scorer rubrics and supported-harness list were not seen, so confirm it can read Claude Code sessions and read every script before the first run. Rules:
- Scope it to this repo; never use `--all-conversations`, since transcripts can contain secrets.
- Keep the report private. It ends with a Warp Factories sign-up link and a share-as-image button.
- Treat scores as leads, not evidence: the same kind of model grades the work, the curve `0.5 + 0.5 * score` lifts every score, skill coverage is 15% of the overall grade, and the default sample is 12 sessions. Use the failed conversations and the drafted edits, not the letter grade.
- Never let an autonomous session answer its closing "apply these suggestions?" with yes. Each edit lands as a human-approved PR, is scored against the Step 8 eval set, and is kept only if it wins.

**Skip for now:** Babysit, Shipping, Autopilot-full, Autopilot-stack and Orchestrate (Step 15 territory, `gh`-dependent, and Autopilot-full merges PRs on its own), Benny, and the style-only skills (`unslop`, `no-comments`, `make-bot-ui`).

**Suggested order:** after Step 6 passes, (1) vendor `create-verification-skill` and generate `verify-<app>` as part of Step 7; (2) use the Bug fix and Refactoring playbooks during Steps 5 and 12; (3) pilot `interrogate` during the Step 8 eval; (4) run `/poteto-mode` as shipped at Step 13; (5) optionally pilot `skill-doctor` (D6) in Step 14; (6) leave the rest until Step 14 data says it is worth the tokens.

---

## Appendix E: PROGRESS.md template

Create `docs/factory/PROGRESS.md` in Step 0 with this content, then append one entry per step using the Appendix A fields.

```
# Factory progress

| Step | Name | Status | Date | Gate command | Evidence |
|------|------|--------|------|--------------|----------|
| 0 | Orient and baseline | not started | | | |
| 1 | One-command local verification | not started | | | |
| ... | (one row per step, 0 to 15) | | | | |

## Log
<!-- Newest last. One entry per step using the Appendix A template. -->
```
