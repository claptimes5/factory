---
name: verify-factory
description: Use before saying a change to this repo is done, and before opening its pull request. Builds the evidence package (verify output, browser tests, screenshots of dashboard changes, each acceptance criterion checked) and hands the task and diff to the fresh-context verifier agent.
---

# Verify a Factory change

"Done" is a claim someone else can check. Collect the evidence below, then
have the `verifier` agent check it without your reasoning. Paste the result
into the pull request's "Testing and evidence" section.

## 1. Write down what "done" means

List the acceptance criteria from the task, one line each, before you look
at the results. Each one gets a command or an observation in step 4.

## 2. Run the checks

- `scripts/verify.sh` (the full one, not `--quick`). Keep its last lines.
- Changed the dashboard or its API? Also run the browser tests for the
  flows you touched: `cd web && npm run e2e -- e2e/<spec>.ts`. The full
  verify runs all of them; the targeted run is what you cite.
- A failure that also fails on `main` with nothing changed may be a known
  flake (`docs/factory/BASELINE.md`). Say so; don't rerun until green.

## 3. Look at the dashboard (UI changes only)

Start `go run ./internal/web/devserver` (127.0.0.1:8090) on `main` and on
your branch in turn, or use `git stash` around it. With the Playwright MCP
tools, open the page you changed on each and save a screenshot to
`.playwright-mcp/before-<page>.png` and `.playwright-mcp/after-<page>.png`.
Rebuild with `cd web && npm run build` first: the devserver serves the
committed build under `internal/web/app`, not your source.

## 4. Check each criterion

For each line from step 1: the command you ran or what you saw, and the
result. A criterion with nothing behind it is "not verified", not "done".

## 5. Hand it to the verifier

Spawn the `verifier` agent (`.claude/agents/verifier.md`). Give it only:

- the task as it was given to you, word for word;
- the branch name, so it can run `git diff main...<branch>` itself;
- the acceptance criteria from step 1.

Don't give it your reasoning, your results or what you think is risky: it
checks the change, not your account of it. If it finds a problem, fix it
and run it again with a fresh agent.

## 6. Report

```
Acceptance criteria: <each, with the command or observation and result>
verify: <last lines of scripts/verify.sh>
Browser tests: <specs run, passed/failed>
Screenshots: <paths, or "no UI change">
Verifier: <its verdict and the observations it cites>
Not verified: <anything you couldn't check, and why>
```
