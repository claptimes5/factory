# Plan: try it, checking a change in the running app

Status: designed, not built. The owner chose the decisions in "Decided"
below.

## Where things stand

Today a change is checked three ways, and none of them uses the app the
way a person would:

| | What it shows | Who wrote the check |
|---|---|---|
| Gates | The repo's tests pass, and with `proves_fix` (Go only) the changed tests fail without the change | The builder, mostly unit tests |
| Hidden scenarios | Fixed end-to-end scripts pass | A person, ahead of time, not about this issue |
| Previews | Screenshots of the pages the change touches | Nobody: the reviewer is told they show what it looks like, not that it works |
| Review | The diff reads right against the issue | A model, reading code |

So nothing opens the app, does what the issue asks and looks at the
result. The builder grades its own work with its own tests, and those
tend to be unit tests even where the issue is about what a user sees.

## What changes

1. **Builders prove changes at the highest level that reaches them.**
   An end-to-end test when the change shows up in the UI and the repo has
   an `e2e` gate; an integration or request test for an API, a job or a
   data flow; a unit test only for logic best checked alone. The test
   must still fail without the change and pass with it; it no longer has
   to be written first.
2. **`proves_fix` works in any language**, on any changed test file.
3. **A new stage, try**, runs after the gates and before review. A fresh
   agent, which never sees the builder's account of its work, writes a
   checklist from the issue, does each step in the running app, and
   reports pass or fail with a screenshot for each. A confirmed fail
   sends the build back like a review finding.
4. **The factory is the first repo try runs on**, checking dashboard
   changes against the devserver's sample data.

## Decided

1. **Where it runs: the sandbox first, a deployed preview later.** Every
   try runs the app in the job's own containers with seeded test data.
   Phase 6 adds the same checks against a repo's own per-PR deploy.
2. **A confirmed fail blocks**, as a review finding does: a fix round,
   and no auto-merge without a pass.
3. **Test data comes from the repo**: a seed command and named test
   accounts in factory.yaml. A builder whose change needs more seed data
   adds it in the same PR.
4. **A separate agent**, not the reviewer and not the builder's own
   tests. The reviewer stays read-only on code; the builder doesn't grade
   itself.
5. **Tools, not a shell.** The try agent reaches the app only through
   tools the factory serves, plus commands the repo declares.
6. **Level 1 of the factory on itself is in this plan**: dashboard
   changes, over the devserver. Level 2, a devserver whose actions really
   run the pipeline over a fake forge and a scripted harness, is a plan
   of its own, after this one.

## Config

Everything sits on a repo's `preview` block, which already says how to
start the app, except `e2e`, which is an ordinary gate command:

```yaml
repos:
  - name: acme/web
    services: {db: {image: postgres:16}}
    gates:
      commands:
        e2e: bin/rails test:system
      proves_fix: bin/rails test {files}
    preview:
      start: bin/rails server -p $PORT
      seed: bin/rails db:prepare db:seed
      login:
        admin: {user: admin@example.test, password_env: FACTORY_QA_ADMIN}
        member: {user: member@example.test, password_env: FACTORY_QA_MEMBER}
      try: true
      try_tools:
        run_jobs: bin/rails jobs:work_off
        mail: curl -s localhost:1080/messages
      url: https://pr-{pr}.preview.acme.dev
```

- **`gates.commands.e2e`.** When a repo has one, the builder is told to
  add or extend an end-to-end test for what the issue asks, where the
  change shows up in the UI.
- **`proves_fix` gains `{files}`**, the changed test files. `{pkgs}`
  keeps working for Go. The check that "code changed, so a test must
  have" looks at every non-test file, not only `.go`.
- **`seed`** runs after install and before `start`, against the job's own
  service containers. It never reaches anything real.
- **`login`** names test accounts. Passwords come from `factory.env` by
  the variable named, reach only the factory's browser driver, and never
  a prompt, the builder's container or the PR.
- **`try`** is off unless set, and needs `start`.
- **`try_tools`** are commands the try agent may call by name, run in the
  app's container. A person writes them, so they're trusted like gate
  commands. `{arg}` in one takes a single argument from the agent,
  shell-quoted.
- **`url`** is today's live preview link; phase 6 also tries the change
  there.

## The stage

```
implement → gates → preview screenshots → try → review → PR
```

Try is a job kind of its own, `try`, with its own row, cost, log and
retry. It runs after the gates, so a build that fails its tests is never
tried, and before review, so a change that doesn't work skips a review
round.

**It runs when** the repo has `preview.try`, triage didn't rate the item
text-only, and the diff touches something a user sees: a file
`features.yaml` maps to a feature with pages, or, without a
`features.yaml`, any non-test file. Otherwise the PR says "Not tried: no
user-facing change."

**Two containers.**

1. **The app's.** Today's preview capture, extended: the checked export
   with its dependencies, `seed`, then `start`, on the job's services
   network, with the gate's egress allowlist and no secrets. A browser
   driver in the job image (Playwright, as previews and hidden scenarios
   already use) holds one headless Chromium session and takes commands as
   JSON lines from the factory, recording each action it performs.
2. **The agent's.** A read-only stage like the critic: Read, Glob and
   Grep on the checkout, no network, `dontAsk`. It reaches the app only
   through tools the factory serves over the MCP socket read-only stages
   already use (`withTools`), each relayed to the driver.

**The tools.**

| Tool | Does |
|---|---|
| `open(path)` | Loads a path on the app's own address; nothing else |
| `click`, `fill`, `select`, `press` | Act on an element by role and name, or by visible text |
| `snapshot()` | The page's accessibility tree, as text |
| `screenshot(note)` | Saves evidence the agent can then Read |
| `log_in(as)` | The driver signs in as a named `login` account; the model never sees the password |
| `console()` | The browser's errors this session |
| `http(method, path, body)` | A request to the app's own address |
| `query(sql)` | Read-only SQL, in a read-only transaction, on the job's own database service |
| `app_log(since)` | The app's output |
| each `try_tools` entry | The repo's command, by name |

No shell, no `eval`, no address but the app's. Every call is logged, and
each browser action saves a screenshot.

**What the agent is given:** the issue, triage's spec, the diff, and on
later rounds its own checklist from round one and any check the builder
disputed, with the builder's reason. Not the builder's summary, test list
or claims.

**What it does:** writes a checklist from what the issue asks, each item
a thing a user does and what they should then see, plus one check that
the nearest thing the change could break still works; does each; and
replies with:

```json
{"verdict": "pass | fail | unverifiable",
 "checks": [{"id": "C1", "step": "...", "expected": "...", "observed": "...",
             "status": "pass | fail | blocked", "evidence": ["shot-3.png"]}],
 "note": "..."}
```

**Unverifiable** is its own verdict: the app never answered, the seed
failed, a step needs a login the repo hasn't set, or the behavior has no
way to be seen with these tools. A blocked check makes the verdict
unverifiable, never fail.

Turns, budget and model come from the item's tier, like other stages,
and the stage has a timeout. Both containers go when it ends.

## Fails, findings and fix rounds

**A fail is checked before it counts.** The factory replays each failed
check's recorded actions on a fresh page, without the model. Fails again:
confirmed. Passes: `flaky`, reported, not blocking.

**A confirmed fail is a finding**: `kind: try`, major, blocking, its
claim the check and what happened instead, its evidence the screenshots,
console errors and log lines of that step. It skips the verifier, which
checks claims by reading code and so could only weaken a reproduced
failure. The builder answers it as any finding: `fixed`, `wrong` or
`out_of_scope`. `wrong` and `out_of_scope` go to the next try, which
runs the check again: a check that still fails stands, and the try
agent may drop a check only by saying why the builder is right. The
dashboard shows both sides.

**Rounds.** After a fix: gates, then the whole checklist again, so a fix
that breaks another check is caught. Try's blocks count toward the same
cap as review's (`countBlocks`), and an item that reaches it parks with
the try report.

| Verdict | Build | Review | Auto-merge |
|---|---|---|---|
| pass | goes on | runs, and sees the report | allowed by try |
| fail, confirmed | fix round | skipped this round | no |
| fail, all flaky | goes on | runs | no: waits for you |
| unverifiable, or the stage errored | goes on | runs | no: waits for you |

## On the PR and the dashboard

**The PR.** Try goes at the top of the factory's one preview comment,
which keeps its screenshots table below, folded:

> **Tried it: 4 of 4 checks pass** · round 2 · full report
>
> | | Check | |
> |---|---|---|
> | ✅ | Open an item with an estimate → the estimate shows under the title | screenshot |
> | ⚠️ | At phone width → the estimate wraps without being cut off *(flaky: passed on replay)* | screenshot |

Unverifiable reads, in the factory's own words, "Couldn't try this
change: the app didn't answer on / within 1m. A person needs to check
it." No app log or console text goes on the PR; they can quote the
repo's `files`. The PR description gets one line: "Tried: pass", "fail"
or "couldn't verify".

**The dashboard.**

- The item's stamps gain Try: Plan, Build, Try, Review, You, Live. Its
  Try panel lists the checks, each with its steps and a screenshot per
  step, and its console errors and log lines; disputed checks show the
  builder's reason and the ruling.
- A try job's transcript shows each browser call with its screenshot.
- Spend gains try's cost per item, its first-round pass rate, and try
  escapes: PRs that passed try that you then changed or reverted, beside
  review escapes.
- "Couldn't verify" and "auto-merge held by try" reach you through the
  existing waits-for-you notice.

## The factory on itself

The factory's own entry, the first repo try runs on:

```yaml
- name: claptimes5/factory
  gates:
    install: cd web && npm ci && npm run build
    commands: {e2e: cd web && npm run e2e}
    diff_guard:
      protected_paths: [internal/runner/**, internal/egress/**, internal/web/auth.go,
                        internal/pipeline/guard.go, prompts/**]
  preview:
    start: go run ./internal/web/devserver -listen 0.0.0.0:$PORT
    try: true
```

The devserver seeds its own sample data, with made-up names, so there is
no `seed` and nothing private to show. Its actions are refused ("would
retry job 12"), so level 1 checks what the dashboard shows and how it
responds, not that the pipeline acts. The protected paths keep the
factory from loosening its own sandbox on its own: those changes always
wait for a person.

## Testing

- `proves_fix` with `{files}`: a Ruby or JS change whose test passes on
  the base is refused; one whose test fails there passes.
- The driver: each tool against a small fixture app, including `open`
  refusing another host and `log_in` never echoing the password.
- `query` refuses a write.
- Replay: a check that fails then passes is flaky and doesn't block; one
  that fails twice blocks.
- The stage's verdicts, end to end in the pipeline tests with a scripted
  harness: pass reaches review; fail makes a finding with no verifier
  run and skips review; unverifiable holds auto-merge; disputes reach the
  next try.
- The skip rule: text-only items and diffs with no user-facing files
  aren't tried.
- The PR comment and the item's Try panel, in the dashboard's e2e tests
  over the devserver.

## Phases

0. **Implement aims higher, and `proves_fix` in any language.** The
   prompt change, `{files}`, and the `e2e` instruction. Useful on its
   own, and before the rest.
1. **The app container and its driver.** `seed`, `login`, the driver, the
   tools served over MCP, behind `preview.try`. No agent yet: tested by
   driving it directly.
2. **The try agent.** The prompt, the verdict, the stage between gates and
   review, unverifiable holding auto-merge. While this phase lands, a
   fail only holds auto-merge, so a new stage can't loop builds.
3. **Fails as findings.** Replay, `kind: try` findings, fix rounds,
   disputes, the round cap. Fails now block.
4. **The PR and the dashboard.** The comment, the stamp and panel,
   metrics, notices. Docs: configuration.md, using.md, quality.md and
   security.md (what try's tools can reach).
5. **The factory on itself.** Its entry above, and a first run on a real
   dashboard issue, as the check that the whole thing works.
6. **The deployed preview.** The same checks against `preview.url` once
   the PR's deploy answers, with egress opened to that host for that
   stage only, reported as their own section.

Each phase ships with its tests and docs. Phases 1 to 3 change what
`internal/egress` and the read-only stages allow; their PRs say exactly
what they loosen.

## Out of scope

- A shell or `eval` for the try agent.
- Video recordings of a session.
- Changes with no way to be seen through these tools beyond what
  `try_tools` adds: they come out unverifiable, and a gap that keeps
  coming up becomes a built-in tool.
- Level 2 of the factory on itself: its own plan.
