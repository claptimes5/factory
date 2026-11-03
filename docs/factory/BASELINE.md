# Baseline

Recorded 2026-11-03 on a WSL2 Linux workstation, from a clean worktree of
`main` at c1fcd34.

## Stack

- **Backend:** Go 1.27 (`go.mod`), one binary `cmd/factory`, packages under
  `internal/`. SQLite store (`internal/store`); schema changes live in Go,
  not a migrations folder.
- **Dashboard:** React 19 + Vite + TypeScript under `web/`, npm. The built
  app (`internal/web/app`) and API types (`web/src/api`, generated from
  `internal/web/openapi.yaml`) are committed so the Go build needs no Node.
- **Agent prompts:** `prompts/`. Replay evals: `evals/` (README only, no
  cases yet).
- **Not a monorepo** in the workspace sense: one Go module, one npm package.
- **Existing agent config:** `CLAUDE.md` → `AGENTS.md`;
  `.claude/settings.local.json` (personal, two allow rules). No committed
  `.claude/settings.json`, hooks or skills.
- **Deploy:** self-hosted; `deploy/factory.service` (systemd),
  `deploy/upgrade.sh`, `deploy/job-image`. Releases via the manual
  `release.yml` workflow (GitHub releases, Linux/macOS binaries).

## Commands and results

| Command | Result | Time |
|---|---|---|
| `go vet ./...` | pass | 6s |
| `go build ./...` | pass | 2s |
| `golangci-lint run ./...` | **not run: not installed on this machine** (CI pins v2.14.0) | n/a |
| `go test -race ./...` | pass, 27 packages ok, 3 without tests | 111s |
| `cd web && npm ci` | pass; `npm audit`: 4 vulnerabilities (3 moderate, 1 high) | 8s |
| `cd web && npm run build` (gen, tsc, check-styles, vite) | pass; main JS chunk 1.18 MB (Vite size warning) | 7s |
| `cd web && npm run e2e` | pass: 166 passed, 4 skipped | 46s |
| `scripts/check-web-built.sh` | pass: committed build matches `web/` | <1s |

## Skipped tests (all conditional, none quarantined)

- Go, by environment: no `sh` (2), platform-specific script (1), npm or
  global Playwright missing (4), no Docker or its images (2). Which of
  these ran here was not recorded per test.
- Playwright: 4 skipped on the phone project by design (desktop-only
  tests that change sample data, or need a wide screen).

No flaky tests observed in one run each; flakiness is measured in Step 5.

## CI and repo settings

- `ci.yml` on push to `main` and every PR: `test` (`go test -race`),
  `lint` (vet, build, golangci-lint), `web` (npm ci, build, stale-build
  check, Playwright desktop+phone, report uploaded as an artifact).
- `cla.yml`, `release.yml` (manual, dry run by default).
- **`main` has no branch protection and no rulesets** (API 404 / `[]`).
  Auto-merge is off at the repo level.
- `CODEOWNERS`: `/.github/`, `/factory.example.yaml`, `/prompts/`,
  `/.golangci.yml` → the maintainer. Ineffective until branch protection
  requires code-owner review.

## Protected paths

Confirmed by the maintainer 2026-11-03. From AGENTS.md "Take care with"
and CODEOWNERS, plus `deploy/` and `.claude/`:
`internal/runner`, `internal/egress`, `internal/web/auth.go`, `guard` in
`internal/web/web.go`, `internal/pipeline/guard.go`, `.github/`,
`prompts/`, `factory.example.yaml`, `.golangci.yml`, `deploy/`, `.claude/`,
`docs/factory/` (from Step 3).

## Critical flows

Chosen by the agent on the maintainer's instruction ("use your best
judgment"); revisit in Step 5.

1. **Issue to PR:** a labeled issue is picked up, built, gated, reviewed
   and opened as a pull request.
2. **Gates block bad changes:** a change that fails a required check,
   touches a protected path or exceeds the size limit never becomes a PR.
3. **Review rounds settle:** a blocking review sends findings back, fix
   rounds are capped, and the item parks instead of looping.
4. **Containment:** builder code runs only in the sandbox, behind the
   egress allowlist, and never sees tokens it shouldn't.
5. **Dashboard login and request guard:** no dashboard action works
   without a valid session.

## Environments

- **No running instance.** Preview and rollback (Step 10) mean running
  Factory locally, with Docker running for the job sandbox. A release is
  a GitHub release from `release.yml`; rolling back means installing the
  previous release.
- **Approver:** the maintainer alone. No compliance constraints.
- **Public development:** `docs/factory/` is committed to this public
  repo. It must never name or describe other repositories Factory works
  on (AGENTS.md).

## Tests that never run anywhere

`TestServicesWithDocker` (`internal/runner`) skips both here and, most
likely, in CI: it needs Docker *and* the `redis:7-alpine` and
`alpine:3.20` images already pulled, and nothing pulls them. It covers
how a job's commands reach its service containers without reaching
anything else, inside the sandbox. Proposed for Step 4: pull the two
images in CI so it runs.

## Known problems

- golangci-lint missing locally, so the local check set is not the CI set.
- No branch protection: CI is advisory, CODEOWNERS is not enforced.
- 4 npm audit findings in dashboard dependencies.
- No single command runs everything CI runs.
