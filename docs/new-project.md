# Starting a new project for the factory

Hand this file to the agent (or person) setting up a new repository. It
lists what the repo needs before the factory can work on it. The factory
proves each change with the repo's own checks, so a new repo needs those
checks in place, and passing, before its first issue.

## The goal

The first commit on the default branch should score **ready** in
`factory readiness`, which means it has no blockers. Readiness blocks on
these:

1. **It installs from a clean checkout** with one command.
2. **Every required check has a command and passes** on the default branch.
3. **The checks can reach the services they need**, such as a database
   (see the note on services below).
4. **It has tests**, at least one test file, and they pass twice in a row:
   no network, no sleeps, no dependence on time or test order.

Readiness only reports the rest as gaps, but set them up anyway. The
factory can't prove a change without them:

- **A recognisable stack** from a manifest at the repo root (see the table
  below). Setup reads it to pick the install, lint and test commands. If it
  recognises nothing, it falls back to the Go checks at the top of
  `factory.yaml`, which will fail on any other stack.
- **A committed lockfile**, so every job installs the same versions.
- **Test, lint and build checks**, plus types where the stack has them.
- **Real tests**, one for each behavior that exists so far.

These are recommended from day one too:

1. **A feature map**, `features.yaml` at the root (format below).
2. **CI** that runs the same checks, such as `.github/workflows/ci.yml`.
3. **One end-to-end smoke test** of the main flow, such as Playwright,
   Rails system specs or an HTTP-level API test.

## Per stack

Use the stack's standard layout and tools. These are the commands the
factory runs, and the files that make it pick them.

| Stack | It reads | Install | Checks it runs |
| --- | --- | --- | --- |
| Go | `go.mod` | (modules download on build) | `go vet ./...`, `golangci-lint run ./...`, `go test -race ./...`, `go build ./...` |
| Node / TypeScript | `package.json`, plus `package-lock.json`, `pnpm-lock.yaml` or `yarn.lock` | `npm ci` (`npm install` with no lockfile; pnpm and yarn run through `corepack`) | `typecheck` (or `npx tsc --noEmit`), plus the `lint`, `test` and `build` scripts |
| Python | `pyproject.toml` or `requirements.txt` (`uv.lock` if you use uv) | a `.venv` in the checkout; `[build-system]` or `[dependency-groups]` in `pyproject.toml` decides how | `.venv/bin/ruff check .` and `.venv/bin/mypy .` if your dependency files mention them, and `.venv/bin/python -m pytest` if they mention pytest or there's a `tests/` directory |
| Ruby / Rails | `Gemfile` | `bundle install` | `bundle exec rubocop` if it's in the Gemfile, then `bundle exec rspec` (with rspec or a `spec/` directory), `bin/rails db:prepare test` (Rails) or `bundle exec rake test` |
| Rust | `Cargo.toml` + `Cargo.lock` | `cargo fetch` | `cargo clippy --all-targets -- -D warnings`, `cargo test`, `cargo build` |

Some notes for each stack:

- **Node:** give `package.json` real `lint`, `test` and `build` scripts.
  The test script must exit on its own; the factory sets `CI=true`, so
  vitest and jest won't wait in watch mode.
- **Python:** put ruff, mypy and pytest in a dev group or extra so the
  install step gets them.
- **Go:** commit a `.golangci.yml`. Each change is also held to a coverage
  floor and has to come with a test that fails without the fix, so start
  with well-tested packages.
- **A database or other service:** declare it under the repo's `services`
  in `factory.yaml` (see [Services](configuration.md#services-databases-and-the-like)), and make the tests read its address
  from an environment variable.

## Feature map

```yaml
features:
  - name: Sign up
    description: Create an account with email and password
    paths: [src/auth/, src/routes/signup.ts]
    tests: [src/auth/signup.test.ts, e2e/signup.spec.ts]
    check: npm test -- src/auth          # optional: runs just these tests
    risk: high                           # optional: low, medium or high
```

List each user-facing feature as it's built. `paths` take files,
directories or globs (`*` doesn't cross `/`). Mark anything touching auth,
payments or data loss as `risk: high`.

## Don't

- **Don't commit failing or skipped tests**, or placeholder tests that
  assert nothing. "No tests" and "fails on main" are blockers.
- **Don't let tests reach the internet.** Jobs run behind an allowlist, so
  tests that do will fail.
- **Don't add agent instruction files the factory protects,** such as
  `CLAUDE.md`, `AGENTS.md` or `.claude/`, unless you mean to steer every
  future reviewer. The factory's builders can't edit them.

## Check your work

With the factory set up for the repo (`./factory setup` adds it):

```sh
./factory readiness you/new-repo
```

Fix every ✗. Then run `./factory readiness -queue you/new-repo` to hand
the remaining gaps to the factory as issues, and start filing feature
issues labeled `factory:ready`.
