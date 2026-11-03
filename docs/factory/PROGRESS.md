# Factory progress

| Step | Name | Status | Date | Gate command | Evidence |
|------|------|--------|------|--------------|----------|
| 0 | Orient and baseline | passed | 2026-11-03 | commands in BASELINE.md | [BASELINE.md](BASELINE.md) |
| 1 | One-command local verification | not started | | | |
| 2 | Lean CLAUDE.md | not started | | | |
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
