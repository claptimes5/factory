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
| `e2e/notify.spec.ts` "When to tell you…" (phone) | 1 in 4 full runs, 2026-11-03 | `apiResponse.json: Response has been disposed` at line 16 | The settings route handler is still fetching when the page closes | maintainer | **Fix in Step 6, not proven:** the test stops its handlers before the page closes. Not reproduced, even with the handler slowed; stays listed until 5 CI runs and the second workstation show it clean |
| `e2e/get-started.spec.ts` "repositories are chosen by where they're hosted…" (desktop) | 2 in 4 full runs, 2026-11-03, second workstation | `locator.click` timeout at line 327: the Remove button resolves, then is detached from the DOM, again and again | The page fetches the setup again (every 2s while a check runs) and gets the server's repo list, which never had the test's played save | maintainer | **Fixed in Step 6:** later fetches answer from the played save. Reproduced with the 2s polling forced on, passed after the fix; stays listed until the second workstation shows it clean |
| ~~`TestAPIReads` (`internal/web`)~~ | CI on PR #119, run crossing 00:00 UTC, 2026-11-04 | `notifications = …` at `api_test.go:263`, no `budget` entry | Spend is recorded before midnight UTC and read after it; "today's spend" (`store.Today()`, wall clock) is then $0. Fails only across UTC midnight | maintainer | **Fixed in Step 5:** the Store has a Clock, the dashboard tests pin it |

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
- **GitHub plan (recorded 2026-11-04):** the repo is **public** and owned
  by a **personal account** (`owner.type: User`). The CLI's token can't
  read the account's plan, but for a public repo Free and Pro offer the
  same controls:

  | Control | Available | Compensating control where not |
  |---|---|---|
  | Branch protection | yes, in use | |
  | Rulesets (branch) | yes, none yet | branch protection covers `main` |
  | CODEOWNERS enforcement | yes, not yet required (Step 9) | |
  | Merge queue | **no** (organization repos only) | "require branches to be up to date" (on) |
  | Push rulesets | **no** (private and internal repos only) | the guard hook and the sandbox locally; CODEOWNERS on `.github/` and `.claude/` once required (Step 9); the bot's token gets no `workflow` scope (Step 9) |
- **Workflow secrets (recorded 2026-11-04):** the repo has no secrets or
  variables (`gh secret list`, `gh variable list`: empty). `ci.yml` and
  `pr.yml` run the PR's code with `contents: read` and no `secrets.*`.
  `cla.yml` runs on `pull_request_target` with a write token but never
  checks out the PR's code. `release.yml` runs only by hand, on `main`.
  No workflow runs an agent, so the CI agent limits don't apply yet. Every
  action is pinned to a commit SHA; `scripts/verify.sh` fails if one isn't.
- **Install hygiene (since 2026-11-04):** `web/.npmrc` turns off packages'
  install scripts and sets a 7-day `min-release-age`;
  `web/package-lock.json` and `go.sum` are committed and `npm ci` installs
  from the lockfile. Only `fsevents` (macOS only) has an install script.
  npm before 11.10 ignores the cooldown with a warning: CI's Node 24 has
  npm 11.19; this workstation's Node 24.4.1 has 11.5.2, and the second
  workstation's Node 22 has npm 10.

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

Revisited in Step 5 (2026-11-04). The code behind each, which
[seeds.py](seeds.py) seeds bugs into:

| Flow | Code | Seeds | Caught |
|---|---|---|---|
| 1. Issue to PR | `internal/state` (stage edges), `internal/pipeline/stages.go` (review routing) | 2 | 2 |
| 2. Gates block bad changes | `internal/gates` (run, pass, diff guard), `stages.go` (gates stop the build) | 6 | 5 |
| 3. Review rounds settle | `stages.go` (round cap, `countBlocks`), `internal/pipeline/guard.go` (breaker) | 4 | 4 |
| 4. Containment | `internal/egress` (allowlist, blocked addresses), `internal/runner` (docker args) | 7 | 7 |
| 5. Login and request guard | `internal/web/auth.go`, `guard` in `internal/web/web.go` | 5 | 5 |

The one miss can't be caught: `DiffGuard`'s parsing of copied files never
runs, since it asks git for renames only (`-M`). The API contract is
checked by `TestOpenAPIMatchesGo` (Go types against `openapi.yaml`, which
the dashboard's types are generated from); a dropped field fails it.

## Environments

- **No running instance.** Preview and rollback (Step 10) mean running
  Factory locally, with Docker running for the job sandbox. A release is
  a GitHub release from `release.yml`; rolling back means installing the
  previous release.
- **Approver:** the maintainer alone. No compliance constraints.
- **Rollout mode A** (recorded 2026-11-04): the maintainer controls the repo
  and is its only developer, so changes go straight to it with no shadow fork.
- **Secrets when running the baseline:** the tests need none. Checked
  2026-11-04: the agent's environment holds no secret variables apart
  from Claude Code's own session token, and no `factory.env` is loaded.
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
