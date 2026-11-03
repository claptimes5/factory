# Factory progress

| Step | Name | Status | Date | Gate command | Evidence |
|------|------|--------|------|--------------|----------|
| 0 | Orient and baseline | passed | 2026-11-03 | commands in BASELINE.md | [BASELINE.md](BASELINE.md) |
| 1 | One-command local verification | passed | 2026-11-03 | `scripts/verify.sh` | fresh clone at 0e2c81e, 2 runs |
| 2 | Lean CLAUDE.md | passed | 2026-11-03 | fresh-session task, verify from CLAUDE.md only | 3 fresh-agent runs, see log |
| 3 | Deterministic guardrails | not started | | | |
| 4 | Required CI mirrors local verify | not started | | | |
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
