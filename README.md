# Factory

Factory is a single Go binary that turns labeled GitHub (or GitLab) issues
into reviewed draft PRs. Claude Code builds each change in a fresh
container, deterministic gates check it, Codex reviews it adversarially,
and you merge. It only merges on its own if you turn on
[auto-merge](docs/configuration.md#auto-merge) for small, low-risk changes.
The full design is in [SPEC.md](SPEC.md).

```
issue labeled factory:ready
  -> triage    (Claude, read-only): rates it XS/S/M/L and low or high risk,
               or parks it with a question
  -> implement (Claude, in a fresh container): failing test first, then a commit
  -> gates     (daemon, deterministic): diff guard, vet, lint, test -race, build,
               test-proves-fix (the new tests must fail on the old code),
               the coverage floor, and hidden scenarios if you set them up
  -> review    (Codex, read-only, sees only issue + diff + gate results)
  -> verify    (a cheap model checks each blocking finding; only confirmed ones block)
  -> critic    (Claude, high-risk items only)
  -> draft PR with the evidence, labeled factory:awaiting-you
```

Each model tier gets `max_attempts_per_tier` attempts (2 by default). Then
the item moves one tier up, and after the last tier it parks. A reviewer
block sends the findings back to the builder, for at most two fix rounds; a
third block parks the item. The builder answers each finding by its id, and
the next review rules on those answers while it reads only what changed. A missing or malformed verdict never passes.

## Quickstart

You need a Linux machine (or [WSL2 on Windows](docs/setup.md#windows-wsl2))
and admin rights on the repo you want the factory to work on.

1. **Install the dependencies.**

   | What | Version | Why |
   | --- | --- | --- |
   | Go | 1.26+ | Builds the `factory` binary (it fetches the 1.27 toolchain itself). |
   | Docker | 28+ for the egress allowlist; any recent one with `mode: open` | Runs every job in a container. |
   | git | any recent | Worktrees and pushes. |
   | Node.js | 22.12+ | Only to rebuild the dashboard; the built app is committed. |
   | A Claude API key or subscription token | | The builder and Claude reviewers. |
   | An OpenAI API key or a ChatGPT plan | | Only if a reviewer uses Codex. |

   You don't install Claude Code, Codex or Playwright: setup builds them
   into the job image. See [Setup](docs/setup.md#dependencies) for details.

2. **Check and build.** `scripts/install.sh` checks Go, git and Docker,
   prints the command that fixes anything missing on your system, and
   builds the binary. It installs nothing itself.

   ```sh
   git clone https://github.com/claptimes5/factory && cd factory
   sh scripts/install.sh
   ```

   Or build it yourself: `CGO_ENABLED=0 go build -o factory ./cmd/factory`.

3. **Run `./factory serve`, then open http://127.0.0.1:8080/app/get-started.**
   The page walks you through the rest: your repos, Docker, a GitHub App it
   creates for you, and your keys, checking each as you go and saving them
   in `factory.env`. Finish starts the factory. On a server or in a script,
   `./factory setup` asks the same in the terminal.
   [Setting up in the browser](docs/setup.md#setting-up-in-the-browser).

4. **Keep it running.** `./factory serve` is the daemon. To keep it running
   across reboots, [run it as a service](docs/setup.md#running-it-as-a-service).
   Something not working? `./factory doctor` checks your whole setup and
   says how to fix each problem ([Checking your setup](docs/setup.md#checking-your-setup)),
   and the get-started page shows whether setup is still complete.

5. **Protect your repo's default branch.** This is the one thing setup
   can't do for you. In the repo's settings, require a pull request with
   your review before merging, plus your CI checks if it has any. The
   factory only opens draft PRs, so this keeps every merge in your hands.

6. **Open the dashboard** at http://127.0.0.1:8080/app/.

7. **Label an issue `factory:ready`.** Watch it move across the board.

Set a spend alert on your API accounts too. The factory has its own
[budgets](docs/configuration.md#budgets), but a second line of defense is
cheap.

## Starting a new project

The factory can only prove a change works as far as the repo's own checks
reach. Before a new repo gets its first issue, give
[docs/new-project.md](docs/new-project.md) to whoever sets it up. It lists
what the repo needs, stack by stack.

For an existing repo, run `./factory readiness` to see what's missing.
See [How ready a repo is](docs/quality.md#how-ready-a-repo-is).

## Day to day

| You | The factory |
| --- | --- |
| Label an issue `factory:ready` | Triages, builds, checks and reviews it, then opens a draft PR |
| Review PRs labeled `factory:awaiting-you` and merge the good ones | Tells you how closely to read each one (`factory:risk-low`, `-medium`, `-high`) |
| Read why an item parked (it leaves a comment), fix or answer it, then add `factory:retry` | Sends it back through triage |
| Set [`budgets.confirm_over`](docs/using.md#cost-estimates), then OK the costly items with `factory:go` | Estimates each item's cost from its own history after triage, and holds anything over the limit for you before building it |
| Read the plan the factory posts on an `L` issue, then add `factory:go`, or comment and add `factory:replan` | [Plans big items](docs/using.md#plans-before-big-builds) before building them, so a wrong approach is caught in a comment rather than a finished PR |
| Glance at the dashboard's **Floor** or your phone | Notifies you when something parks, a PR is ready or the budget runs out |
| Turn on [`main_health`](docs/using.md#keeping-main-green) | Files a fix first in the queue when a check goes red or flaky on your default branch, and closes it once the check passes |
| Turn on [`sources`](docs/using.md#issues-from-sentry-dependabot-and-todos), then label the issues you want built | Files issues from Sentry errors, Dependabot alerts and `TODO(factory)` comments, which wait for your approval, and closes them if the error, alert or comment goes away first |
| Turn on [`grooming`](docs/using.md#backlog-grooming), then confirm what you agree with | Flags likely duplicate, stale and too-big issues with a comment and a `?` label; closes one when you add `factory:close`, or opens its proposed sub-issues under an epic when you add `factory:split`. It never closes or splits on its own |
| Give a web app's repo a [`preview`](docs/using.md#screenshots-on-the-pr) | Runs the app once a change passes its gates, screenshots the pages it touches at desktop and phone sizes (before and after, if you like), shows them to the reviewer, and keeps one comment on the PR with them |

Only someone with write access to the repo can start work, so outsiders can
file issues and you choose which ones to build. See
[Who can start work](docs/security.md#who-can-start-work).

## Guides

| Guide | What's in it |
| --- | --- |
| [Setup](docs/setup.md) | Dependencies, what setup asks, running as a service, upgrades, manual setup with a GitHub App, WSL2 |
| [Using the factory](docs/using.md) | The dashboard and control room, notifications, every command, the orchestrator, keeping main green, issues from Sentry, Dependabot and TODOs, backlog grooming, screenshots on the PR, `factory pull`, `eval` and `compare` |
| [Configuration](docs/configuration.md) | `factory.yaml`: several repos, workspaces, GitLab, toolchains and checks, services, files a repo doesn't commit, disk space, skill sets, models, budgets, main health, sources, backlog grooming, PR previews, Claude-only review |
| [Quality](docs/quality.md) | The gates, proves-fix, the coverage floor, text-only changes, review scaled by risk, review findings, the feature map, readiness, hidden scenarios, metrics, the replay set |
| [Security](docs/security.md) | Who can start work, how jobs are isolated, sources' tokens and untrusted text, grooming's untrusted issue text, running a preview, the egress allowlist, the control room's login |
| [Starting a new project](docs/new-project.md) | What a new repo needs before the factory can work on it |
| [Status](docs/status.md) | What's built, and where this differs from [SPEC.md](SPEC.md) |

## License

Factory is source-available under the [Functional Source License, Version
1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2), copyright Avi Leavitt.
You can use, change and share it for anything except offering a product or
service that competes with Factory. Two years after each version is
released, that version is also available under the Apache License 2.0.

Contributions need a signed [Contributor License Agreement](CLA.md); see
[CONTRIBUTING.md](CONTRIBUTING.md).
