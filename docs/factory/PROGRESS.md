# Factory progress

| Step | Name | Status | Date | Gate command | Evidence |
|------|------|--------|------|--------------|----------|
| 0 | Orient and baseline | passed | 2026-11-03 | commands in BASELINE.md | [BASELINE.md](BASELINE.md) |
| 1 | One-command local verification | passed | 2026-11-03 | `scripts/verify.sh` | fresh clone at 0e2c81e, 2 runs |
| 2 | Lean CLAUDE.md | passed | 2026-11-03 | fresh-session task, verify from CLAUDE.md only | 3 fresh-agent runs, see log |
| 3 | Deterministic guardrails | passed | 2026-11-03 | live hook demos + `python3 .claude/hooks/guard_test.py` | see log |
| 4 | Required CI mirrors local verify | passed | 2026-11-04 | throwaway PR, clean PR, direct push | #122, #121, see log |
| 5 | Characterization + critical-path tests | not started | | | |
| 6 | Browser E2E + web quality gates | not started | | | |
| 7 | Agent self-verification protocol | not started | | | |
| 8 | AI code review + security review | not started | | | |
| 9 | Risk-tiered approval and ownership | not started | | | |
| 10 | Safe ship and rollback | not started | | | |
| 11 | First automations: chores | not started | | | |
| 12 | First feature through the full loop | not started | | | |
| 13 | Graduate to poteto-mode | not started | | | |
| 14 | Measure and improve | not started | | | |
| 15 | Scale carefully | not started | | | |

## Log
<!-- Newest last. One entry per step using the Appendix A template. -->

### 2026-11-03
```
STEP 0: Orient and baseline   STATUS: passed
Gate command(s): go vet ./...; go build ./...; go test -race ./...;
  cd web && npm ci && npm run build && npm run e2e; scripts/check-web-built.sh
Result: all pass (vet 6s, build 2s, test -race 111s, npm ci 8s, web build 7s,
  e2e 166 passed / 4 skipped in 46s). golangci-lint not run: not installed locally.
Negative control: n/a (read-only step)
Flagged changes: none
Decisions made: protected paths = AGENTS.md "Take care with" + CODEOWNERS +
  deploy/ + .claude/ (confirmed); critical flows picked by the agent (maintainer
  delegated); no running instance, previews and rollback are local with Docker;
  develop docs/factory/ in public; pull the Docker test images in CI (Step 4).
Open questions for human: none
Next step proposed: 1
```

### 2026-11-03
```
STEP 1: One-command local verification   STATUS: passed
Gate command(s): git clone . <scratch> && scripts/verify.sh (twice); scripts/verify.sh --quick
Result: fresh clone at 0e2c81e: run 1 pass in 156s (cold Go test cache), run 2 pass in
  78s (warm); Playwright 166 passed, 4 skipped both times. --quick: pass in 61s cold.
Negative control (each planted in the scratch clone, verify red, reverted: yes):
  Go type error -> go vet; TS type error -> dashboard build; errcheck violation ->
  golangci-lint; flipped assertion in pathglob_test.go -> go test -race; Go import of a
  missing module -> go vet; web import of a missing file -> vite build; unformatted new
  Go file -> first MISSED (gofmt saw tracked files only), fixed in 0e2c81e, now red.
Flagged changes: none (no test, threshold or CI change). gofmt -w on cmd/factory/images.go
  (whitespace only; main had drifted because nothing checked gofmt).
Decisions made: script is scripts/verify.sh (repo has scripts/, no Makefile); it fails if
  golangci-lint isn't the version ci.yml pins; installed golangci-lint v2.14.0 locally
  (asdf reshim golang needed); no .nvmrc, package.json engines and CI's node 24 cover it.
Flaky: e2e/notify.spec.ts (phone) failed once in 4 full runs; quarantined visibly in
  BASELINE.md, fix in Step 6.
Open questions for human: none
Next step proposed: 2
```

### 2026-11-03
```
STEP 2: Lean CLAUDE.md   STATUS: passed
Gate command(s): a fresh agent in a fresh clone, told to learn verification only from
  CLAUDE.md (-> AGENTS.md): "add one sentence to Match's doc comment and prove the repo
  still verifies". Default PATH.
Result: run 3 (at 9eac808) found and ran scripts/verify.sh, was stopped by verify's
  Windows-npm check, followed it, verify passed (166 passed, 4 skipped). No questions
  for a human. Its one gap (where a Linux Node lives) is now printed by verify via asdf.
Negative control: run 1 used the old AGENTS.md (worktree from main, by mistake): it ran
  only the Go checks and skipped the dashboard, unsure what the full gate was. Run 2
  (new AGENTS.md) failed on the Windows npm that `which npm` hid. Both gaps fixed.
Flagged changes: none
Decisions made: CLAUDE.md stays an @AGENTS.md import (Factory's own builders read
  AGENTS.md); AGENTS.md now 82 lines. The npm trap became a check in verify, not prose.
  A truly separate `claude -p` session was blocked by the auto-mode safety check; the
  gate used subagents told to ignore inherited context.
Open questions for human: none
Next step proposed: 3
```

### 2026-11-03
```
STEP 3: Deterministic guardrails   STATUS: passed
Gate command(s): live, in this session, with .claude/settings.json installed:
  git push --force nowhere-remote HEAD; git add <planted errcheck file> && git commit;
  scripts/verify.sh --quick; Edit of .claude/hooks/guard.py.
  Plus python3 .claude/hooks/guard_test.py (16 tests, in verify).
Result: force push refused by the hook; the commit refused with golangci-lint's
  errcheck finding; verify --quick ran, no prompt (auto mode, so this doesn't prove
  the allowlist); the protected edit got "ask", which auto mode's classifier
  approved: no person saw it. The maintainer chose to keep "ask" (see decisions).
Negative control: 6 breaks of guard.py on a copy each turned its tests red. The live
  demo found two holes, both fixed with tests: (1) the hook found guard.py from
  $CLAUDE_PROJECT_DIR, the main checkout, so in a worktree it ran nothing; it now
  follows the event's cwd. (2) `printf > bad.go && git add && git commit` passed,
  since the hook runs before the whole command; a commit must now be its own
  command (cd and git add before it are fine).
Flagged changes: verify.sh gains the hooks' tests and a tool check that installs
  golangci-lint and Playwright's Chromium at the pinned versions (stricter, not looser).
Decisions made: one stdlib Python script (no jq on the second workstation); protected
  edits ask, and in auto mode the classifier answers (maintainer's choice over a
  refuse-and-unlock file); shell writes to protected paths are caught best effort
  (redirects, sed -i, cp, mv, rm, tee), with review and CODEOWNERS behind them; crashes
  refuse; settings deny reading factory.env, factory.yaml and *.pem; the agent may not
  write .claude/settings.json (auto mode refused twice), so the maintainer installs it,
  in the main checkout too, since Claude Code reads settings from there. Factory's
  builders run with --setting-sources user and never load these hooks.
Second workstation: golangci-lint was missing for asdf's Go 1.27.3 (now verify installs
  it); Node 22.13. Full verify: 339s cold, then 122s with one new desktop flake
  (get-started.spec.ts:299, now in BASELINE), then 132s, 166 passed, 4 skipped, then
  125s with the same flake again (165 passed, 1 failed).
Open questions for human: none
Next step proposed: 4
```

### 2026-11-04
```
STEP 4: Required CI mirrors local verify   STATUS: passed
Gate command(s): (a) throwaway PR #122 breaking an assertion in e2e/home.spec.ts, its
  description the empty template; (b) #121 (this step); (c) git push origin HEAD:main
  with an empty commit on origin/main.
Result: (a) web, verify and pr-body red, merge state BLOCKED; the failure was the planted
  "Checking nothing at all" (desktop and phone). Filling the description reran pr-body
  -> pass, still BLOCKED on verify. Closed, branch deleted.
  (b) all checks pass (lint 38s, test 1m56s, web 2m40s, verify, pr-body, cla), merge state
  CLEAN. (c) rejected: GH006 "Changes must be made through a pull request"; main unchanged.
Negative control: (a) is it. Also check-pr-body.py fails #120's description (no sections).
Flagged changes: TestServicesWithDocker (internal/runner, protected) fails instead of
  skipping when FACTORY_REQUIRE_DOCKER is set; CI sets it. Stricter. Its first CI run came
  back "(cached)" from a run that skipped; with the variable it ran (internal/runner 30s).
Decisions made: CI's jobs each run one --part of verify.sh (same speed as before) and a
  verify job aggregates them, with if: always() so a skipped part can't pass; the
  golangci-lint action only installs the binary. Required: verify, pr-body, cla; strict
  up to date (no merge queue for personal accounts); PRs required with 0 approvals (sole
  maintainer; Step 9); admins included; no force push or deletion. The pr-body check runs
  the PR's own script, like every test; review is the guard. Auto mode refused a merge
  attempt on the red PR, so (a) is shown by GitHub's merge state, not by a refused merge.
Open questions for human: none
Next step proposed: 5
```
