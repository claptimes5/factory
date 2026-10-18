# Configuration

Everything lives in [`factory.yaml`](../factory.yaml): routing per stage,
escalation tiers, timeouts, budgets, Codex token pricing, gate commands and
the diff guard. Stage prompts are in [`prompts/`](../prompts). Secrets and
keys go in `factory.env` instead (see [Manual setup](setup.md#manual-setup)).

`factory setup` writes most of this for you. Read on when you want to
change something by hand.

- [Several repositories](#several-repositories)
- [Workspaces: one change across repos](#workspaces-one-change-across-repos)
- [GitLab repos](#gitlab-repos)
- [Each repo's toolchain and checks](#each-repos-toolchain-and-checks)
- [Services: databases and the like](#services-databases-and-the-like)
- [Files a repo doesn't commit](#files-a-repo-doesnt-commit)
- [Disk space](#disk-space)
- [Skill sets](#skill-sets)
- [Models and escalation](#models-and-escalation)
- [Auto-merge](#auto-merge)
- [Budgets](#budgets)
- [Tuning](#tuning)
- [Learning from your edits](#learning-from-your-edits)
- [PR previews](#pr-previews)
- [Claude-only review](#claude-only-review)

The gates, the coverage floor, risk and the feature map are in
[Quality](quality.md). The egress allowlist and the control room's address
are in [Security](security.md).

## Several repositories

One factory can work on several repos. Each is worked on separately: its
own issues, branches, PRs and checks. They share the queue, the budgets,
the models and the dashboard, which can filter by repo.

```yaml
repos:
  - you/api                     # takes the top-level base_branch and gates
  - name: you/web
    base_branch: develop
    gates:                      # overrides the top-level gates for this repo
      required: [types, test]
      commands: {types: npm run typecheck, test: npm test}
```

`repo: you/api` is the short form for one repo.

A repo's `gates` override the top-level ones field by field. Its
`commands` replace the top-level commands. Anything it leaves out, such as
the diff guard or `proves_fix`, is inherited. Set `proves_fix: none`,
`coverage: none` or `scenarios_dir: none` to turn one off for that repo.

With a GitHub App, install it on every repo (setup walks you through
this). Repos under different accounts need the app to be public, which
setup sets up for you. Anyone could then install it, but only on their own
repos, and its private key stays with you.

The orchestrator sees every repo and names one when it opens an issue.
With several repos, name an issue on the command line as `owner/name#12`.

## Workspaces: one change across repos

Some repos change together, like a Rails API and the React app that calls
it. Put them in a workspace and an issue in either one becomes a single
change across both:

```yaml
repos: [you/api, you/web]
workspaces:
  shop: [you/api, you/web]      # the order their PRs merge in
```

- **One branch, every repo.** The builder gets every repo of the workspace
  checked out side by side, `api/` and `web/`, on one branch named for the
  issue (`factory/web-12` for `you/web#12`), and is told each repo's checks.
- **Gates per repo.** The gates run in each repo the change touches, in
  that repo's own image and with its own commands, feature map and
  scenarios. Their results are named for the repo: `api: test`,
  `web: build`.
- **One review.** The reviewer reads one diff across the repos, every path
  under its repo's folder, and is asked to check the repos still agree.
- **A PR per repo.** Each repo the change touches gets its own draft PR.
  The PRs link to each other and list the order to merge them in. Only the
  one that merges last closes the issue, so it stays open until the whole
  change is in.
- **Done when all merge.** The item waits on you once every repo passes,
  and is done when every PR is merged. A PR closed without merging parks it.

The builder and reviewer run in the first repo's image with its `env`, so
that image needs every member's toolchain (Ruby and Node, say). Services
of all the members start together; where two name a service the same, the
first repo's wins. Triage reads only the issue's own repo.

## GitLab repos

Repos can be on GitLab, gitlab.com or self-hosted, alongside GitHub ones.
Name them by host and path:

```yaml
repos:
  - you/api                          # GitHub
  - gitlab.com/team/web              # gitlab.com
  - git.example.com/platform/infra/deploy   # self-hosted, nested groups
```

A group with a dot in its name needs `host:` set, as in
`{name: my.team/web, host: gitlab.com}`.

The factory works them the same way: it picks up issues labeled
`factory:ready` and opens a draft merge request for each.

**The token.** The factory reaches GitLab with `GITLAB_TOKEN`, which
`factory setup` asks for and checks against each project.

- A project access token limits it to one project. For several, use a
  group access token or a personal token for a bot account.
- The token needs the `api` and `write_repository` scopes.
- It needs the Developer role, or Maintainer if Developers can't push to
  the base branch.

**CI.** On GitLab a pushed branch runs its own `.gitlab-ci.yml` with the
project's runners and CI variables. So builders may never change
`.gitlab-ci.yml` or `.gitlab/`: the diff guard adds both for every GitLab
repo.

**Differences from GitHub:**

- GitLab doesn't record why an issue was closed, so the readiness check
  never files a gap again once its issue is closed.
- The dashboard's CI panel shows the commit's pipeline jobs.
- To start work, a person needs Developer or higher (see
  [Who can start work](security.md#who-can-start-work)).
- Each repo's own self-hosted GitLab is added to the egress allowlist for
  you.

## Each repo's toolchain and checks

`factory setup` reads each repo's files and suggests checks and a job image
for its stack: Rails, Ruby, Node (Vite, Next.js, React), Python, Rust or Go.
If the repo has a dev container with an `image`, the job image is built on
that. You confirm each suggestion, and setup writes it to `factory.yaml`.
For a Rails API and a Vite frontend it writes something like:

```yaml
repos:
  - name: you/api
    image_from: ruby:3.3.6-bookworm # ./factory images builds factory-job-you__api on it
    gates:
      install: bundle install
      required: [lint, test]
      commands: {lint: bundle exec rubocop, test: bin/rails db:prepare test}
      proves_fix: none              # the proves-fix and coverage checks are Go-only for now
      coverage: none
  - name: you/web                   # Node is in the default image
    env: {VITE_API_URL: http://localhost:3000}
    gates:
      install: npm ci
      required: [lint, test, build]
      commands: {lint: npm run lint, test: CI=true npm run test, build: npm run build}
      proves_fix: none
      coverage: none
```

| Key | What it does |
| --- | --- |
| `install` | Runs first on each commit's export, with the network on. If it fails, nothing else runs. The builder is told to run it too, along with the repo's own check commands. |
| `image_from` | The base for the repo's job image: any Debian or Ubuntu image with the toolchain. `./factory images` builds the missing images (`--rebuild` rebuilds them all). |
| `image` | An image you build yourself, when there's no `image_from`. It needs the agent CLIs, so start it from `deploy/job-image/Dockerfile` with `--build-arg BASE=...`. |
| `env` | Set in the repo's agent and gate containers. A top-level `env` applies to every repo. Agents can read it, so it is not for secrets. |

**Dependency cache.** A repo with an install step gets a cache at `/cache`
(in `data/cache/<owner>__<name>`) for npm and pnpm. Their stores check
every package against the lockfile's hashes, so one job can't plant a
package a later job trusts. Only install may write to it. Gems
(`vendor/bundle`), Python virtualenvs and Cargo's home are built inside
each commit's export instead, so they start fresh every time.

## Services: databases and the like

When a repo's tests need a database, cache or queue, list it under the
repo (or at the top level, for every repo). The factory starts it for each
build and check:

```yaml
repos:
  - name: you/api
    services:
      postgres:
        image: postgres:16
        env: {POSTGRES_PASSWORD: factory}
        ready: pg_isready -U postgres     # run until it succeeds, before anything else
      redis: {image: redis:7, ready: redis-cli ping}
    env:
      DATABASE_URL: postgres://postgres:factory@postgres:5432/api_test
      REDIS_URL: redis://redis:6379
```

How services behave:

- **Reach a service by its name** as the host.
- **Each job gets a fresh set.** Every implement job, and the readiness
  check, gets its own set on a private network that has no way out.
  `factory eval` gets a set too.
- **Who joins.** The builder and every check join that network; reviewers
  don't. Hidden scenarios join it as well, but they still reach nothing
  else.
- **Cleanup.** The services are removed when the job ends, and the daemon
  clears any that a crash left behind.
- **Each job starts empty.** Create the test database in the install step
  (for example `bin/rails db:prepare`).
- **Checks share data within a job.** The builder and every check share
  one set, so data one check writes is there for the next. Tests that
  clean up after themselves (a transaction per test) keep that from
  mattering.

Services need the docker runner on Docker 28 or newer, and can't be used
with `network: host`. Don't put secrets in their `env`, since it goes on
the docker command line.

If a check fails because it can't connect to a database and the repo has
no services, the readiness check says so. It suggests the services to add,
read from the repo's `docker-compose.yml` or `config/database.yml`, instead
of filing an issue for a builder, who can't edit `factory.yaml`.

## Files a repo doesn't commit

Some repos need a file to build or run their tests that isn't in git, such
as a `.env` or `config/master.key`. List them under `files`, with `from`
set to a checkout of the repo on the factory's machine, and the factory
copies them into every job's checkout: the builder's, and each check's,
including the readiness check and evals.

```yaml
repos:
  - name: you/app
    files:
      from: ~/code/app            # a checkout of you/app on this machine
      paths: [.env, config/master.key, certs/]   # files or folders
```

`from` must be readable by the user the daemon runs as, and `~` is that
user's home. Run as a service, that's the `factory` user, so give it a
path like `/home/you/code/app` and read access to it. A folder in `paths`
brings only the files git doesn't track there.

Leave out `paths` and the `.worktreeinclude` file in `from` picks them,
the way Claude Code does for its worktrees: the files that match its
patterns (gitignore syntax) and are gitignored.

```
# .worktreeinclude
.env
config/*.key
```

A copied file never ends up on the branch. It is left out of every commit
the factory makes, even if the builder edits it or changes `.gitignore`.
If the repo itself has a file at that path, the repo's wins. Since the
agent can read the files, give it test credentials, never production
ones: the builder could still copy a secret into a file it commits.
If `from` or a listed path is missing (the checkout moved, say), that
repo's jobs fail with the path in their error; other repos keep working.

## Disk space

Each job checks out the repo, installs its dependencies and runs in a
container. A full disk fails jobs halfway, usually during install, where it
looks like the builder's fault. So while jobs are waiting, the daemon
checks free space at most once a minute. It starts no build while any of
these has less than `budgets.min_free_disk_gb` (default 10; a negative
value turns the check off):

- **The data directory.**
- **Where Docker stores containers.** For an engine on this machine, that's
  its data root. Docker Desktop keeps its disk in its own VM, and a remote
  engine (`DOCKER_HOST` or a docker context) is on another machine. For
  those the daemon asks a short-lived container, using a job image already
  on the machine; it never pulls one.
- **Under WSL, the Windows drive** holding WSL's and Docker Desktop's
  virtual disks (`runner.wsl_drive`, default `/mnt/c`). Inside WSL, `df`
  shows the virtual disk's full size, not what's left on the drive under
  it.

While builds are stopped:

- You're told once when builds stop, and again when they start.
- They start again only at the limit plus 10% (at least 2 GB), so a disk
  hovering at the limit doesn't stop and start them over and over.
- If Docker can't be measured, they stay stopped.
- The control room shows a banner.
- `factory run-once`, `eval` and `compare` fail with "low on disk" rather
  than report jobs that never ran.
- The orchestrator still runs, since it writes next to nothing.

## Skill sets

A skill set is a folder under `skills/` holding Claude Code skills
(`<skill>/SKILL.md`). Name it on any routing entry and the daemon mounts it
read-only as the worker's `~/.claude/skills` (and `~/.codex/skills`):

```yaml
routing:
  implement.M: {harness: claude, model: sonnet, skills: tdd-strict}
```

Every job records the skill set it ran with. `factory status` shows it,
and so does the PR's job table.

To try two skill sets on the same issue, use
[`factory compare`](using.md#comparing-skill-sets).

### Skills and instructions for each step

Any routing entry, including `critic` and `orchestrate`, can also say which
skills in its set to use, in order, and add instructions of its own, inline
or from a file (a relative path is relative to `factory.yaml`). A repo can
add instructions for `triage`, `implement`, `review` or `critic`:

```yaml
routing:
  implement.M:
    harness: claude
    model: sonnet
    skills: go
    use: [tdd, table-tests]          # each needs skills/go/<name>/SKILL.md
    instructions: Keep each commit to one behavior.
    instructions_file: prompts/house-style.md

repos:
  - name: you/app
    instructions:
      implement: Run `make generate` after editing the schema.
      review: Block any handler that skips the auth middleware.
```

All of these are added to the end of that step's prompt, under
"Instructions for this step": the skills first, then the route's
instructions, then the file, then the repo's. The daemon checks at startup
that each skill and file exists. Escalation keeps the instructions of the
size an item started on, so an `implement.S` item that climbs to Opus still
gets `implement.S`'s. In a [workspace](#workspaces-one-change-across-repos), the build and review
get every member repo's instructions, each labeled with its repo.

## Models and escalation

`routing` picks the harness, model and effort for each step: `triage`,
`implement.XS` to `implement.L`, `review` (and `review.medium_risk`,
`review.high_risk`), `critic`, `orchestrate`, `tune` and `learn`. Triage rates each issue
`XS`, `S`, `M` or `L`, and the item builds on the matching
`implement.<size>` entry.

The defaults lean on the cheap models where a miss is cheap to retry:

- Small builds start on Haiku 5.5 and climb to Sonnet, then Opus, after two
  failed attempts at a tier (`escalation.max_attempts_per_tier`, 2 by
  default). The tiers are `escalation.implement`. After the last tier, the
  item parks.
- Text-only (`XS`) builds use `routing.implement.XS`, Haiku by default (see
  [Text-only changes](quality.md#text-only-changes)).
- Larger builds start on Sonnet or Opus.
- Reviews run on GPT-6.1 Sol.

The daemon refuses a config where a reviewer could run the same harness
and model as the builder it reviews.

**Fast mode.** `fast: true` on a Claude route runs Claude Code's fast mode,
which answers up to 2.5 times faster at a higher price per token:

```yaml
routing:
  implement.L: {harness: claude, model: opus, fast: true}
```

Only Opus 4.8 and Opus 5 and later have it, so the daemon refuses `fast` on
any other model. If escalation moves a job to a model without it, that job
runs at normal speed. A fast job's dollar cap is its usual budget times
`budgets.fast_multiplier` (2 by default); raise it if fast jobs stop at
their cap. The critic doesn't inherit `fast` from `implement.L`; give
`routing.critic` its own entry for that.

The job table and `factory status` show `opus (fast)` for a job that asked
for fast mode. Claude Code runs at normal speed instead if your account
doesn't have fast mode turned on, or its fast-mode limit is reached. On a
subscription it draws on usage credits, not the plan's limits. The cost
recorded is what Claude Code reports, so check it against your console the
first few times.

**Session resume.** With the Docker runner, an implement retry on the same
model continues the previous attempt's Claude session. This is best
effort: a missing session starts fresh.

**Pricing.** Claude Code reports its own cost. Codex reports only tokens,
which the daemon prices from `pricing`, cached input included. Keep those
prices current when OpenAI changes them. On a
[ChatGPT plan](setup.md#codex-on-a-chatgpt-plan), Codex jobs record their
tokens and cost $0, so they don't count against the budgets; the plan's
usage limits hold them instead. Claude Code on a subscription still
reports a dollar figure, and that counts.

## Auto-merge

By default every PR waits for you. `auto_merge` lets the factory merge its
own PRs for small, low-risk changes, once the repo's CI passes:

```yaml
auto_merge:
  up_to: low        # the highest risk that merges itself: low or medium
  max_size: S       # the largest triage size: XS, S (default), M or L
  method: squash    # merge (default), squash or rebase

repos:
  - name: you/payments
    auto_merge: {up_to: none}   # a repo's setting replaces the top level's
```

A PR qualifies when its risk (after the [feature map](quality.md#feature-map)
has had its say) is at or below `up_to` and its size is at or below
`max_size`. High-risk changes, workspace changes, and changes whose feature
map couldn't be read always wait for you. For a qualifying PR, the factory:

1. Marks the draft ready and turns on GitHub's auto-merge (or GitLab's
   "merge when the pipeline succeeds") for the commit the gates and
   reviewer passed. GitHub refuses if the branch has moved since.
2. Labels the PR and issue `factory:auto-merge` instead of
   `factory:awaiting-you`, and notifies you.
3. Lets GitHub merge it when the branch's required checks and reviews pass.
   If CI fails, nothing merges and the PR waits for you.

To stop one, park the item, turn off auto-merge on the PR, or close it. The
factory also turns auto-merge off itself before it builds on the branch
again; if it can't, it parks the item rather than push a commit that could
merge unchecked. If you push
to the PR yourself, turn auto-merge off first: GitHub keeps it on for a
push from anyone with write access.

The repo has to be set up for it:

- **Allow auto-merge** must be on in the repo's settings (GitHub).
- **Make your CI checks required** in branch protection. GitHub's
  auto-merge waits only for required checks. If nothing is required, the
  factory merges only when every check on the commit has already passed,
  and otherwise leaves the PR for you. CI that only posts commit statuses,
  not check runs, counts as no checks here. On GitLab, the commit's
  pipeline must be running or passed, and you should turn on **Pipelines
  must succeed** in the project's merge settings.
- **Required reviews still apply.** If branch protection requires an
  approving review, the PR merges the moment you approve it. To let these
  PRs merge without you, require status checks but no review. If you use
  rulesets, put the review rule in its own ruleset and give the factory's
  GitHub App a bypass on that one only, so the checks still apply to it.

If the factory can't turn on auto-merge, it says why and the PR waits for
you as usual. On GitLab, `method: rebase` and `merge` both follow the
project's own merge method; only `squash` changes it. PRs waiting to
auto-merge count toward `awaiting_human_max` until they merge.

## Budgets

The daemon enforces its own caps, under `budgets` in `factory.yaml`. The
values below are this repo's.

| Key | What it caps |
| --- | --- |
| `per_job` | Dollars per implement job, by size: `{XS: 0.5, S: 2, M: 6, L: 15}`. Without an `XS` entry, XS uses S's. |
| `triage`, `review` | Dollars per triage or review job. |
| `daily_total` | Total dollars a day. When it's hit, no new jobs start until midnight UTC. |
| `concurrent_jobs` | Jobs running at once (3). |
| `awaiting_human_max` | No new implement jobs while this many PRs wait for you (5). |
| `min_free_disk_gb` | The [disk space](#disk-space) floor (default 10). |
| `pause_at_usage` | Holds a harness's jobs once a subscription usage window is this full, in percent (default 95). See below. |

The orchestrator's spend has its own caps, `orchestrator.run_budget` per
run and `orchestrator.daily_budget` per day, which don't count against the
build budget. `factory compare` arms split what's left of today's budget.

**Subscription usage limits.** On a Claude subscription, Claude Code has a
five-hour and a weekly usage window, and so does Codex on a
[ChatGPT plan](setup.md#codex-on-a-chatgpt-plan). The factory records the latest
reading each run reports, and `factory status`, the dashboard's
**Spend** page and the orchestrator show how much is used and when it
resets. When the five-hour or weekly window reaches `pause_at_usage`, that
harness's jobs wait in the queue until it resets, instead of failing and
using up retries. The other harness keeps working, and you're notified
when it pauses and resumes. A window that only one model has, such as a
weekly Opus limit, is shown but doesn't pause anything. With API keys
there are no windows, so nothing pauses.

If a window resets early, or a reading is wrong, run
`factory limits clear claude` (or `codex`, or no name for both) and the
jobs start on the daemon's next pass.

Claude Code reports these readings in its output, but this isn't in its
documentation, so a reading may be missing. Codex writes them only to its
session file, which the factory copies out inside the job container (the
Docker runner only, and the job image needs `sh`). Codex only has windows
on a ChatGPT plan; on an API key there are no Codex readings to pause on.

Set a spend alert on both API accounts as well. Spend from a job killed
mid-run is not recorded (the CLIs report cost only at the end), so the
daily cap can undercount after a crash.

## Tuning

The [tuning loop](using.md#tuning) proposes changes to the settings from
the metrics, on a cadence. Its settings, with their defaults:

```yaml
tuning:
  every: 720h                 # how often, at least 1h; 0 or none turns it off
  min_items: 10               # skip a run with fewer builds since the last
  budget: 3                   # dollars per run; default orchestrator.run_budget
  repo: you/infra             # optional: also open a draft PR there
  path: deploy/factory.yaml   # where this file lives in that repo; default factory.yaml
```

| Key | What it does |
| --- | --- |
| `every` | The cadence, at least `1h`. The clock starts the first time the daemon runs with tuning on, and survives restarts. A clock more than two cadences old (tuning was off a while) starts again. A run reads at most two cadences back. |
| `min_items` | A run with fewer items built since the last one is skipped, and you're told. `factory tune` ignores it. |
| `budget` | The dollar cap for one run. Its spend counts toward `orchestrator.daily_budget`, not the build budget. A run waits while either of those is spent for the day. |
| `repo`, `path` | Where this `factory.yaml` lives in one of the factory's repos. With `repo` set, each proposal with changes opens a draft PR there on `factory/tuning-<date>`, made to that copy of the file. The file must exist there as a plain file, not a symlink or behind one, and still load with the change. If the day's branch has commits the factory didn't make, it uses `factory/tuning-<date>-2` and so on, rather than push over them. |

With tuning on, the daemon won't start without `prompts/tune.md`.

The run uses `routing.tune`, which defaults to the `orchestrate` route.
Unlike the orchestrator it has no tools, so it can run on Codex too:

```yaml
routing:
  tune: {harness: claude, model: opus, effort: high}
```

## Learning from your edits

The factory proposes lines for each repo's `AGENTS.md` (loaded from
`CLAUDE.md` for Claude builders) from the changes you make to its PRs and
from findings its reviewers repeat (see
[Learning from your edits](quality.md#learning-from-your-edits)). It's on
unless you turn it off:

```yaml
learning:
  enabled: true     # false turns it off
  min_findings: 2   # alike findings, from two or more items, that start a run
  budget: 0.5       # dollars per learn job; left out, budgets.triage
```

| Key | What it does |
| --- | --- |
| `enabled` | On by default. Off, the factory records nothing and runs no learn jobs. |
| `min_findings` | How many alike blocking findings, from at least two items, start a learn job when no edit of yours does. At least 2 (default 2). |
| `budget` | Dollars per learn job. Left out, it's `budgets.triage`. |

`routing.learn` picks its model. Left out, it runs on triage's harness:
Sonnet on Claude, since triage often runs on Haiku, or triage's own model
on Codex. `timeouts.learn` is 10 minutes by default. A learn job counts
toward `budgets.daily_total`, and waits like a build when the cap is hit.

## Main health

The factory can watch CI on each repo's base branch and file a fix, first
in the queue, when a check goes red or flakes there (see
[Keeping main green](using.md#keeping-main-green)). It's off unless you
turn it on, since the fixes it files spend on builds:

```yaml
main_health:
  enabled: true            # off by default
  every: 10m               # how often it reads CI, at least 1m
  daily_issue_cap: 3       # issues it may file per repo a day
  require_approval: false  # true: issues wait for you to add factory:ready
```

| Key | Default | What it does |
| --- | --- | --- |
| `enabled` | `false` | Reads CI on each repo's `base_branch` and files issues. |
| `every` | `10m` | How often. Each reading is a few API calls per repo. |
| `daily_issue_cap` | `3` | Issues it may file per repo per UTC day. Past it, a red check is tracked and filed the next day. |
| `require_approval` | `false` | Off, an issue is filed with `factory:ready` and built at once, approved as the factory: its text has no CI output, and its fix never merges itself. On, it's filed with the output quoted, and waits for someone with write access to add the label. Either way the fix goes first in the queue and through the gates and review. |

`daily_issue_cap: 0` is the default, 3; to stop filing, turn
`enabled` off.

On GitHub it reads check runs, which the factory's app may read
(`checks: read`), and workflow runs, to name checks by their workflow
(`actions: read`; for an app made before, add it in the app's settings
and accept it on the installation, or checks are told apart by name only). On GitLab it reads the pipelines' jobs and, for a failed one, the
end of its log.

## Sources

The factory can file issues from Sentry errors, Dependabot alerts and
`TODO(factory)` comments, for you to approve (see
[Issues from Sentry, Dependabot and TODOs](using.md#issues-from-sentry-dependabot-and-todos)).
Each is off unless you turn it on, and none builds anything until someone
with write access adds `factory:ready` to its issue.

```yaml
sources:
  daily_issue_cap: 3            # issues per repo a day, all sources together
  sentry:
    enabled: true
    every: 15m                  # at least 5m
    org: acme                   # the organization slug
    projects:                   # each repo's Sentry project
      acme/api: api-backend
      acme/web: other-org/web   # org/project for another organization
    min_events: 10
    min_users: 0
    sort: freq                  # or user
    # base_url: https://sentry.example.com   # self-hosted
  dependabot:
    enabled: true
    every: 1h                   # at least 15m
    min_severity: high          # low, medium, high or critical
    # repos: [acme/api]         # default: every GitHub repo
  todos:
    enabled: true
    every: 6h                   # at least 1h
    markers: [TODO(factory), FIXME(factory)]   # add TODO, FIXME for all
    max_per_run: 5
    skip_paths: [legacy, "*.gen.ts"]
    # repos: [acme/api]         # default: every repo
```

| Key | Default | What it does |
| --- | --- | --- |
| `daily_issue_cap` | `3` | Issues all sources together may file per repo per UTC day. |
| `sentry.enabled` | `false` | Files an issue per unresolved Sentry issue over the thresholds. |
| `sentry.every` | `15m` | How often it reads Sentry; at least `5m`. |
| `sentry.base_url` | `https://sentry.io` | A self-hosted Sentry's address: `https`, or `http` only on this machine, since the token goes with every request. |
| `sentry.org` | none | The organization slug, for projects written without one. |
| `sentry.projects` | none | Repo to Sentry project slug, or `org/project`. Only these repos are read. |
| `sentry.min_events`, `min_users` | `10`, `0` | What a Sentry issue needs, both, to be filed. |
| `sentry.sort` | `freq` | `freq` files the most events first; `user`, the most users. |
| `dependabot.enabled` | `false` | Files an issue per open Dependabot security alert. GitHub only. |
| `dependabot.every` | `1h` | How often it reads the alerts; at least `15m`. |
| `dependabot.min_severity` | `high` | The lowest severity filed. |
| `dependabot.repos` | every GitHub repo | Which repos. With a GitLab repo in `repos:`, list the GitHub ones here: GitLab's dependency scanning isn't supported, and the config won't load otherwise. |
| `todos.enabled` | `false` | Files an issue per marked comment on each repo's base branch. |
| `todos.every` | `6h` | How often it scans; at least `1h`. Each scan is a shallow clone. |
| `todos.markers` | `TODO(factory)`, `FIXME(factory)` | What a comment must start with. |
| `todos.max_per_run` | `5` | Issues per scan; the rest wait for the next. |
| `todos.skip_paths` | none | Folders, or globs (a glob with no `/` matches a name anywhere), to leave out on top of vendored, generated and prose files. |
| `todos.repos` | every repo | Which repos. |

**The Sentry token** is `SENTRY_AUTH_TOKEN` in `factory.env` (setup asks
for it when `sentry` is on): an internal integration's token, or a personal
one, with the `project:read` and `event:read` scopes. Without it, Sentry
is reported unreadable once and nothing is filed.

**Dependabot alerts** need the alerts turned on in the repo's
*Settings > Code security*. A GitHub App needs the *Dependabot alerts:
Read-only* repository permission: setup's app has it, and an app made
before this needs it added (then accept it on the installation page). A
`GITHUB_TOKEN` needs the `security_events` scope, or for a fine-grained
token *Dependabot alerts: read*.

## PR previews

For a web app, the factory can screenshot the pages a change touches once
it passes its gates, show them to the reviewer, and put them on the PR
(see [Screenshots on the PR](using.md#screenshots-on-the-pr)). A repo opts
in with a `preview` block; the only thing it needs is the command that
runs the app:

```yaml
repos:
  - name: acme/web
    gates:
      install: npm ci
    preview:
      start: npm run dev -- --port $PORT --host 127.0.0.1
      # port: 3000              # where it listens; $PORT is set to it
      # ready: /                # polled until it answers below 500
      # wait: 60s               # how long it has to get there
      pages: [/, /pricing]      # always captured; the builder adds its own
      # viewports: [desktop, phone]
      # before: true            # the base branch too, side by side
      # url: https://pr-{pr}.preview.acme.dev   # your own per-PR deploy
```

| Key | Default | What it does |
| --- | --- | --- |
| `start` | required | Runs the app, in the export the gates passed on, after `install`. `$PORT` is set. Bind to `127.0.0.1` or `0.0.0.0`. |
| `port` | `3000` | Where the app listens. |
| `ready` | `/` | The path polled every half second until it answers with a status below 500. |
| `wait` | `previews.wait` | How long the app has to answer `ready`. |
| `pages` | `/` | Paths always captured, in this order. The builder lists the pages its change shows up on, and those follow. |
| `viewports` | `previews.viewports` | `desktop` (1280×800), `tablet` (820×1180), `phone` (390×844), or `WIDTHxHEIGHT`. |
| `before` | `false` | Also runs the base branch, installed on its own export, so each page shows before and after. It roughly doubles the time a preview takes. |
| `url` | none | A link to a live preview your own hosting (Vercel, Netlify, a review app) deploys for each PR, with `{pr}`, `{branch}` and `{sha}` filled in. The factory only links it. |

The top-level `previews` block holds the defaults and limits for every
repo:

```yaml
previews:
  enabled: true          # false turns every repo's preview off
  timeout: 5m            # one capture: start, wait and every screenshot
  wait: 60s
  max_screenshots: 12    # per build, before and after together
  viewports: [desktop, phone]
  keep_for: 336h         # how long a PR's images stay up
```

| Key | Default | What it does |
| --- | --- | --- |
| `enabled` | `true` | Each repo still needs its own `preview`. |
| `timeout` | `5m` | One capture, from starting the app to the last screenshot; at least `30s`. Past it, the screenshots taken so far are kept. |
| `wait` | `60s` | The default `wait`. |
| `max_screenshots` | `12` | Images per build, at most 50. Pages × viewports × sides stays under it: the builder's pages are dropped first, then pages, then viewports. |
| `viewports` | `desktop`, `phone` | The default `viewports`. |
| `keep_for` | `336h` | How long the images on an open PR stay after its last build; at least `1h`. A PR's images are removed as soon as it merges or closes. |

**The job image needs Chromium.** Screenshots are taken with the job
image's own Playwright, so the repo doesn't depend on it. `factory images`
builds a repo's image with `--build-arg PLAYWRIGHT=1` once it has a
`preview`; run `factory images --force` after adding one. An image you
build yourself needs Playwright installed globally (`npm install -g
playwright && playwright install --with-deps chromium`, with `NODE_PATH`
pointing at the global modules). Without it, the PR says so and the build
goes on.

**On GitLab**, the factory's token needs the Maintainer role and GitLab
17.2 or later to remove the images when a PR closes. With less, they're
uploaded and shown as usual, but stay until you delete them (Settings >
General > Uploads); the comment says so.

A page with a long scroll is cut at 4000 pixels. Previews aren't
supported for a repo in a [workspace](#workspaces-one-change-across-repos)
yet.

## Claude-only review

By default Codex reviews what Claude builds, so the reviewer is never the
model that wrote the code. If you only have Claude, a different Claude
model can review instead.

The rule: the reviewer's model must differ from every model the builder
can reach, including the tiers a retry escalates to. So a reviewer on Opus
means Opus cannot be a build tier. Claude models compare by family, so
`claude-sonnet-4-5` counts as `sonnet`.

Change these entries in `factory.yaml` and leave the rest:

```yaml
routing:
  implement.S:       {harness: claude, model: haiku}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: sonnet}
  review:            {harness: claude, model: opus}
  # drop review.high_risk, or point it at opus (or fable) too
escalation:
  implement: [haiku, sonnet]
```

No OpenAI key is needed once no route names `codex`.

This is weaker than cross-vendor review: two models from one family tend
to share blind spots. The rule covers the reviewer, whose approval is
needed before any PR opens. The critic that also reads high-risk diffs
runs only after the reviewer approves and can only add a block. So it uses
`routing.critic` (default: `implement.L`) even when that is the builder's
model.
