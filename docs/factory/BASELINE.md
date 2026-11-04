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

## Flaky tests (quarantine list)

Still running and still required; listed so a red run from them is
recognized, not retried until green.

| Test | Seen | Symptom | Likely cause | Owner | Fix in |
|---|---|---|---|---|---|
| `e2e/notify.spec.ts` "When to tell you…" (phone) | 1 in 4 full runs, 2026-11-03 | `apiResponse.json: Response has been disposed` at line 16 | The settings route handler is still fetching when the page closes | maintainer | Step 6 |
| `e2e/get-started.spec.ts` "repositories are chosen by where they're hosted…" (desktop) | 2 in 4 full runs, 2026-11-03, second workstation | `locator.click` timeout at line 327: the Remove button resolves, then is detached from the DOM, again and again | The repo list re-renders after the save while the test clicks | maintainer | Step 6 |
| `TestAPIReads` (`internal/web`) | CI on PR #119, run crossing 00:00 UTC, 2026-11-04 | `notifications = …` at `api_test.go:263`, no `budget` entry | Spend is recorded before midnight UTC and read after it; "today's spend" (`store.Today()`, wall clock) is then $0. Fails only across UTC midnight | maintainer | Step 5: a clock the store's tests can set |

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
- **Since Step 4 (2026-11-04):** `test`, `lint` and `web` run
  `scripts/verify.sh --part go|lint|web`; a `verify` job passes only when
  all three did. `pr.yml`'s `pr-body` fails a PR whose Testing, Risk or
  How to revert section is empty. `main` is protected: PRs only (0
  approvals), `verify`, `pr-body` and `cla` required, branch up to date
  before merging (no merge queue on a personal account's repo), no force
  pushes or deletion, admins included.

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
anything else, inside the sandbox. **Since Step 4:** CI pulls the two
images and sets `FACTORY_REQUIRE_DOCKER`, which makes the test fail
rather than skip without them (and keeps Go's cache from reusing a
skipped result). It runs in CI now.

## Known problems

- ~~golangci-lint missing locally~~: verify installs it (Step 3).
- ~~No branch protection~~: done in Step 4. CODEOWNERS is still not
  enforced: code-owner review is Step 9.
- 4 npm audit findings in dashboard dependencies.
- ~~No single command runs everything CI runs~~: `scripts/verify.sh`
  (Step 1), and CI runs it (Step 4).
