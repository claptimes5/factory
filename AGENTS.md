# Working on Factory

Factory is a daemon that turns labeled issues into reviewed pull requests:
Go under `cmd/` and `internal/`, the dashboard (React) under `web/`, the
agents' prompts under `prompts/`. [CONTRIBUTING.md](CONTRIBUTING.md) has
what CI runs; this file is what an agent changing the code needs on top.

## This repository is public

Factory works on other repositories too, some of them private. Never name
or describe one, or anything from it (a repo, a company, a person, a
file, an error message, data), in code, comments, tests, fixtures,
commits, pull requests or issues here. Describe a bug in general terms:
"a Rails repo whose lint fails on its base branch", not where it was seen.
Test fixtures use made-up names such as `o/r`, `acme/api` or `you/web`.

## Checks

- `scripts/verify.sh` runs everything CI runs (gofmt, vet, golangci-lint,
  build, `go test -race`, the dashboard build and Playwright) and stops at
  the first failure; about 3 minutes. `scripts/verify.sh --quick` (Go
  without `-race`, plus the dashboard type check) is the inner loop. Run
  the full one before you say a change is done, and show its last lines.
- One Go test: `go test -race -run TestName ./internal/pipeline/`. One
  browser spec: `cd web && npm run e2e -- e2e/home.spec.ts`.
- Run `gofmt -w` on the Go files you change. verify needs golangci-lint at
  the version `.github/workflows/ci.yml` pins, and says how to install it.
- Changed anything under `web/`, or the API in `internal/web/openapi.yaml`?
  Run `cd web && npm ci && npm run build` and commit what it regenerates
  under `internal/web/app` and `web/src/api`, in the same commit.
  `scripts/check-web-built.sh` fails when they're stale. Run the browser
  tests with `npm run e2e`, never `npx playwright test`; where Playwright
  isn't installed, say in the PR that they didn't run.
- The build also runs `npm run check-styles`: font sizes, radii and colors
  come from the design tokens only (see `/app/design`).
- To see the dashboard with sample data: `go run ./internal/web/devserver`
  (127.0.0.1:8090), then `cd web && npm run dev`
  (http://localhost:5173/app/). More in
  [Working on the dashboard](docs/using.md#working-on-the-dashboard).
- On WSL, check `which npm` is the Linux one: a Windows npm on the PATH
  installs a `node_modules` the build can't use.
- A browser test that fails once with nothing changed may be one of the
  known flaky ones listed in `docs/factory/BASELINE.md`; say so, don't
  rerun until green.

## Style

- Read the code around a change first and write like it: the same naming,
  idioms and comment density. Reuse the helpers already there.
- Comments are full sentences that say why, not what. Exported Go names
  get a doc comment that starts with the name.
- Text a person reads (dashboard copy, notifications, docs, errors) is in
  plain words, says what happened and what to do, and names no internals.
- A change to what a user sees or runs updates the docs under `docs/`.
- Tests: one focused test per behavior, named for it
  (`TestFixRoundBuildsOnTodaysBase`), next to its siblings and in their
  style. A fix comes with a test that fails without it.
- Commit titles are one sentence, in the present tense, about the
  behavior: "Retry reruns a parked item's failed review". No prefixes. The
  body says why first, then what changed.

## Take care with

The sandbox (`internal/runner`), the egress allowlist (`internal/egress`),
tokens and secrets, the dashboard's login and request guard
(`internal/web/auth.go`, `guard` in `internal/web/web.go`), and
auto-merge's guard rails (`internal/pipeline/guard.go`) keep untrusted
code and text contained. Keep changes there small and conservative, and
say in the PR what they loosen, if anything.

What steers or ships the code is protected too: `.github/` (CI,
CODEOWNERS), `.claude/`, `prompts/`, `.golangci.yml`,
`factory.example.yaml` and `deploy/`. Change any of these, or the paths
above, only when the task is about them; in a session with a person,
ask first. Never weaken a check to make it pass: no skipped tests,
loosened thresholds or rewritten expectations without saying so.

How this repo is being moved to agent-driven work, step by step, is in
[docs/factory/PLAYBOOK.md](docs/factory/PLAYBOOK.md), with progress in
`docs/factory/PROGRESS.md`.
