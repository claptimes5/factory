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

- `go vet ./...`, `golangci-lint run ./...`, `go test -race ./...` and
  `go build ./...` must pass. Run `gofmt -w` on the Go files you change.
- Changed anything under `web/`, or the API in `internal/web/openapi.yaml`?
  Run `cd web && npm ci && npm run build` and commit what it regenerates
  under `internal/web/app` and `web/src/api`, in the same commit.
  `scripts/check-web-built.sh` fails when they're stale. Then run the
  browser tests with `npm run e2e`, never `npx playwright test`; where
  Playwright isn't installed, say in the PR that they didn't run.
- The build also runs `npm run check-styles`: font sizes, radii and colors
  come from the design tokens only (see `/app/design`).

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
