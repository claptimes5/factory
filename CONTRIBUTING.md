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

## Versions and releases

Factory follows [semantic versioning](https://semver.org). A maintainer
labels each pull request with how much it changes:

| Label | When | Example |
|---|---|---|
| `semver:major` | It breaks something people rely on: a config key, a command or the API changes or goes away | v1.4.2 → v2.0.0 |
| `semver:minor` | It adds something without breaking anything | v1.4.2 → v1.5.0 |
| none | A fix, docs, or anything else | v1.4.2 → v1.4.3 |

Merging doesn't release. A maintainer releases when it's worth it, from
**Actions > release > Run workflow**: it takes the biggest label among the
pull requests merged since the last tag, builds `factory` for Linux and
macOS (amd64 and arm64), and publishes a GitHub release with notes from
those pull requests. Leave **dry_run** checked to see the version and its
pull requests first; uncheck it to release. It only releases a commit whose
CI passed on `main`. The first release is v0.1.0. While the version is
0.x, the config and API may still change between minor versions.

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
