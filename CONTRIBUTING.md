# Contributing to Factory

Thanks for helping. Issues and pull requests are both welcome.

## Before you open a pull request

- For anything bigger than a small fix, open an issue first so we can
  agree on the approach.
- Run what CI runs:
  - `go test -race ./...`
  - `golangci-lint run ./...`
  - `gofmt -l internal cmd` (it should print nothing)
  - If you changed the dashboard: `cd web && npm ci && npm run build`, and
    commit the rebuilt files under `internal/web/app`.
- Update the docs under `docs/` when you change what a user sees or runs.

## Contributor License Agreement

Before your first pull request can be merged, you need to sign the
[Contributor License Agreement](CLA.md). The CLA Assistant bot comments on
your pull request with a link; you sign by replying with the sentence it
gives you. You only sign once, and it covers your later pull requests too.

The agreement lets Avi Leavitt, Factory's licensor, distribute your
contribution under Factory's license (see below) and under Apache 2.0 when
each version converts. You keep the copyright to your work.

## License

Factory is under the [Functional Source License, Version 1.1, ALv2 Future
License](LICENSE.md) (FSL-1.1-ALv2). Your contributions are distributed under
the same terms.
