# Contributing to Factory

Thanks for helping. Issues and pull requests are both welcome.

## Before you open a pull request

- For anything bigger than a small fix, open an issue first so we can
  agree on the approach.
- Run what CI runs:
  - `go vet ./...`, `go build ./...` and `go test -race ./...`
  - `golangci-lint run ./...`
  - If you changed the dashboard under `web/` or the API in
    `internal/web/openapi.yaml`: `cd web && npm ci && npm run build`, then
    commit the regenerated files under `internal/web/app` and
    `web/src/api`. Then run `npm run e2e` (Playwright, desktop and phone).
- Run `gofmt -w` on the Go files you changed.
- Update the docs under `docs/` when you change what a user sees or runs.

## Contributor License Agreement

Before your first pull request can be merged, you need to sign the
[Contributor License Agreement](CLA.md). The CLA Assistant bot comments on
your pull request with a link; you sign by replying with the sentence it
gives you. You only sign once, and it covers your later pull requests too.

You keep the copyright to your work. The agreement gives Avi Leavitt,
Factory's licensor, a license to your contribution that includes the right
to distribute it under any license: Factory's current license (below),
Apache 2.0 when each version converts, or another license in the future.

## License

Factory is under the [Functional Source License, Version 1.1, ALv2 Future
License](LICENSE.md) (FSL-1.1-ALv2), and is distributed with your
contributions under it.
