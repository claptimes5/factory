# Configuration

Everything lives in [`factory.yaml`](../factory.yaml): routing per stage,
escalation tiers, timeouts, budgets, Codex token pricing, gate commands and
the diff guard. Stage prompts are in [`prompts/`](../prompts). Secrets and
keys go in `factory.env` instead (see [Manual setup](setup.md#manual-setup)).

`factory setup` writes most of this for you. Read on when you want to
change something by hand.

- [Several repositories](#several-repositories)
- [Workspaces](#workspaces)
  - [Repos that change together](#repos-that-change-together)
- [GitLab repos](#gitlab-repos)
- [Each repo's toolchain and checks](#each-repos-toolchain-and-checks)
- [Services: databases and the like](#services-databases-and-the-like)
- [Files a repo doesn't commit](#files-a-repo-doesnt-commit)
- [Disk space](#disk-space)
- [Skill sets](#skill-sets)
- [Models and escalation](#models-and-escalation)
- [Auto-merge](#auto-merge)
- [Sensitive code](#sensitive-code)
- [Risk from the diff](#risk-from-the-diff)
- [Budgets](#budgets)
- [Asking before costly builds](#asking-before-costly-builds)
- [Tuning](#tuning)
- [Learning from your edits](#learning-from-your-edits)
- [Backlog grooming](#backlog-grooming)
- [PR previews](#pr-previews)
- [PR descriptions](#pr-descriptions)
- [Claude-only review](#claude-only-review)
- [Codex-only](#codex-only)
- [How review checks itself](#how-review-checks-itself)
- [The security pass](#the-security-pass)

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

A repo with no `base_branch`, here or at the top level, uses its default
branch on GitHub or GitLab, read when the factory starts (`main` if the
host can't say).

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

## Workspaces

A workspace is a project: the repos that belong together. The factory
scopes its work to it:

- **The dashboard** shows one workspace at a time; switch at the top of
  the sidebar.
- **Each workspace has its own orchestrator,** which reads and acts on
  its repos only, and **Ask** works in the workspace on screen. Neither can
  file an issue in another workspace's repo.
- **Trust** is the workspace's: one level for all its repos (see
  [Trust](using.md#trust)).
- **A budget of its own,** if you give it one, under the factory's.

```yaml
repos: [you/api, you/web, you/site]
workspaces:
  shop:
    repos: [you/api, you/web]
    daily_total: 30               # dollars a day, under budgets.daily_total
    orchestrator: {interval: 2h, daily_budget: 5}
    auto_merge: {up_to: low}      # its trust level, for both repos
  site:
    repos: [you/site]
```

- **A repo in no workspace** has one of its own, named for the repo
  (`you/site` alone would be `site`). With no `workspaces:` at all, each
  repo is its own workspace.
- **Every repo is in one workspace,** and each workspace has at least one.
- `repos:` still holds each repo's settings, such as its gates and image;
  a workspace only names its repos.
- **`orchestrator:`** takes the top-level [orchestrator](using.md#the-orchestrator)
  settings for anything it leaves out: `enabled`, `interval`,
  `daily_budget` and `daily_issue_cap`. The top level's `daily_budget`
  still caps them all together.
- **`auto_merge:` and `self_start:`** are the workspace's trust level. A
  repo's own may only be stricter: where both are set, the stricter risk
  and the smaller size apply, and only the self-start sources both allow.
  A repo's `auto_merge` may leave out `up_to` to set only its `method` and
  `breaker`. A workspace of several repos with no level of its own merges
  at the lowest of theirs.
- **`daily_total:`** is dollars a day for the workspace's builds and
  reviews. Once it's spent, its jobs wait for tomorrow, and the other
  workspaces go on. The factory's `budgets.daily_total` still stops
  everything.

### Repos that change together

Some repos change together, like a Rails API and the React app that calls
it. Set `change_together` and an issue in either one becomes a single
change across both:

```yaml
repos: [you/api, you/web]
workspaces:
  shop:
    repos: [you/api, you/web]   # the order their PRs merge in
    change_together: true
```

The older form, a plain list, means the same: `shop: [you/api, you/web]`.

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
that image needs every member's toolchain (Ruby and Node, say). For a Rails
API and a Vite app, list the API first: its image is built on a Ruby base,
and `deploy/job-image/Dockerfile` adds Node to any base that lacks it, so
that one image builds and checks both. Services
of all the members start together; where two name a service the same, the
first repo's wins. Triage reads only the issue's own repo.

A workspace's PRs never merge themselves: `auto_merge` covers one repo's
changes only. On GitLab each repo gets a draft merge request, linked by
GitLab references (`group/api!12`), and the last to merge closes the issue
with `Closes group/api#7`; GitLab closes an issue in another project this
way only when the factory's token can also see that project, and the merge
request targets its project's default branch.
[`docs/examples`](examples/) has a complete GitLab workspace of a Rails
API and a Vite app, on Claude alone and on Codex alone.

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

**The token.** The factory reaches GitLab with `GITLAB_TOKEN`. Either kind
below works; `factory setup` asks which you'll make, shows just its steps,
then checks the token against each project.

- The token's owner needs the Developer role on each project, or
  Maintainer if Developers can't push to the base branch. GitLab checks
  both the token's permissions and its owner's role.
- **A fine-grained personal token** (beta in GitLab 18.10, generally
  available in 19.2) can be
  limited to just the factory's projects. In your profile, go to Access >
  Personal access tokens, choose Generate token > Fine-grained token, pick
  the projects (or their group) under Group and project access, and give
  them the permissions below.
- **A legacy token** is quicker to make: give it the `api` and
  `write_repository` scopes. On gitlab.com,
  <https://gitlab.com/-/user_settings/personal_access_tokens?name=factory&scopes=api,write_repository>
  opens the form with both ticked (use your own host if self-hosted). A
  project access token limits it to one project; for several,
  use a group access token or a bot account's personal token.

The fine-grained permissions, from each call the factory makes. The
selector groups resources under headings; its search box finds each by name.

| Tab | Heading | Resource | Permissions | What for |
| --- | --- | --- | --- | --- |
| Group and project | (use search) | Code | Download, Push | Cloning and pushing branches |
| Group and project | Projects | Project | Read | The default branch and settings |
| Group and project | Repository | Repository | Read | Files and the tree on the base branch |
| Group and project | Repository | Branch | Read | Branch heads |
| Group and project | Repository | Commit | Read | Commits and their statuses |
| Group and project | CI/CD | Pipeline | Read | CI results |
| Group and project | CI/CD | Job | Read | Failed jobs' logs |
| Group and project | Project Planning | Work Item | Read, Create, Update | Issues, labels on them, and comments on issues and merge requests |
| Group and project | Repository | Merge Request | Read, Create, Update, Merge | Draft merge requests; Merge only for auto-merge |
| Group and project | Project Planning | Label | Read, Create | The factory's labels |
| Group and project | System Access | Member | Read | Who may start work |
| Group and project | Projects | Markdown Upload | Create, Delete | Screenshots on merge requests |
| User | System Access | User | Read | Which account the token is |
| Global | System Access | Metadata | Read | The GitLab version |

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
If the repo's `Dockerfile` has a stage for its tests (`test`, `ci`,
`development` or `dev`), the job image is built from that stage, so the
checks have the system libraries and programs the app has installed.
Otherwise, a dev container that builds from a Dockerfile is used, then a
dev container's `image`, then a public image for the stack. If the repo
has a compose file, the services it runs (Postgres, MySQL, Redis, Mongo)
and the app's settings that find them (`PGHOST`, `DATABASE_URL` and the
like) are suggested from it, with its own names and credentials. You
confirm each suggestion, and setup writes it to `factory.yaml`.
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
| `image_from` | The base for the repo's job image: any Debian or Ubuntu image with the toolchain. `factory serve` builds the missing images as it starts, and any built on a base the config no longer names, and jobs wait for them; `./factory images` builds them too (`--rebuild` rebuilds them all). Old copies are removed ([Job images](setup.md#job-images)). |
| `image_build` | Builds the base from the repo's own Dockerfile instead of `image_from`: `dockerfile` and `context` are paths in the repo (default `Dockerfile` and its root), `target` is the stage (default the last), `args` its `--build-arg` values and `secrets` its build secrets (`RUN --mount=type=secret`) by id, each a file: a path in `files.from`'s checkout, or an absolute one. Setup takes `args` from the CI line that builds the same stage, and names any secret the Dockerfile uses for you to set. `./factory images` clones the base branch with the factory's token and builds it on this machine's Docker, outside the jobs' sandbox, as you would build the app's image yourself; then the factory's tools go on top. The stage must be Debian or Ubuntu, and its entrypoint is ignored. Entries on its `PATH` into its own copy of the app (such as `/app/bin`) point at the checkout instead, and if it has the app's gems installed (its `BUNDLE_PATH`), each checkout's `vendor/bundle` starts from them, so `bundle install` only builds what a change adds: a test stage often has no compiler. When the Dockerfile changes on the base branch, `factory serve` notices within five minutes and rebuilds the image: you get a notification, the repo's items say they wait for their image, and its jobs wait for it. A change to a file the Dockerfile copies in doesn't count, since each job installs its own dependencies anyway; `./factory images --rebuild` rebuilds for one. |
| `image` | An image you build yourself, when there's no `image_from`. It needs the agent CLIs, so start it from `deploy/job-image/Dockerfile` with `--build-arg BASE=...`. |
| `env` | Set in the repo's agent and gate containers. A top-level `env` applies to every repo. Agents can read it, so it is not for secrets. |

**Dependency cache.** A repo with an install step gets a cache at `/cache`
(in `data/cache/<owner>__<name>`) for npm and pnpm. Their stores check
every package against the lockfile's hashes, so one job can't plant a
package a later job trusts. Only install may write to it. Gems
(`vendor/bundle`), Python virtualenvs and Cargo's home are built inside
each commit's export instead, so they start fresh every time. Go's
modules and build cache sit beside each commit's export, so its install
(`go mod download`) and every check share them, and they go when it does.
Each commit's checks start with an empty cache.

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
The same goes for any check that fails because the job's sandbox lacks
something: a refused connection to a database or cache, a system library
that can't be loaded, a program that isn't installed, a host the
[egress allowlist](security.md#network-the-egress-allowlist) doesn't let
it reach (it names the host to add to `runner.egress.allow`). Readiness reports it
as a setup blocker with no issue. A build that fails that way, or whose
builder reports it, parks at once with the line that shows it, rather than
trying the next model. Its first button is **Fix setup**: it reads the
repo's Dockerfile, compose file and CI as setup does, and shows the change
to the repo's entry in `factory.yaml` that would give the checks what they
lack (the job image built from a test stage, the services and the settings
that find them), as a list and as before and after. **Apply and retry**
saves it, restarts the factory once running jobs finish (no new job starts
meanwhile, and the job image is rebuilt if it changed), then retries the
item. It needs the dashboard password, or the dashboard opened on the
factory's machine, as settings do. When the repo's files don't say what's
missing, it says so: add it yourself, then press Retry.

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
patterns (gitignore syntax) and are gitignored. Folders a job's install
makes (`node_modules`, `vendor/bundle`, `.venv`, `.bundle`) are left out:
copying yours would be slow, and built for your machine, not the job's.

The readiness check notices a repo that commits a `.worktreeinclude`
while `files` isn't set. A check that fails naming one of its files (a
missing `config/master.key`, say) is a setup blocker for you rather than
an issue for a builder, and it says what to add, with the path of a
checkout of the repo it found under your home folder.

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
gets `implement.S`'s. In a [workspace whose repos change together](#repos-that-change-together), the build and review
get every member repo's instructions, each labeled with its repo.

## Models and escalation

`routing` picks the harness, model and effort for each step: `triage`,
`implement.XS` to `implement.L`, `review` (and `review.medium_risk`,
`review.high_risk`), `critic`, `verify`, `security`, `orchestrate`, `tune` and `learn`. Triage rates each issue
`XS`, `S`, `M` or `L`, and the item builds on the matching
`implement.<size>` entry.

The defaults lean on the cheap models where a miss is cheap to retry:

- Small builds start on Haiku 5.5 and climb to Sonnet, then Opus, after two
  failed attempts at a tier (`escalation.max_attempts_per_tier`, 2 by
  default). The tiers are `escalation.implement`. After the last tier, the
  item parks. A build whose checks fail because the sandbox lacks a
  service, system library or program they need (see
  [Services](#services-databases-and-the-like)) parks at once instead:
  every model would fail the same way. A build that runs out of turns or hits its dollar cap isn't
  a failed attempt: it keeps its work and waits for you (see
  [When a build runs out of turns or budget](using.md#when-a-build-runs-out-of-turns-or-budget)).
- Text-only (`XS`) builds use `routing.implement.XS`, Haiku by default (see
  [Text-only changes](quality.md#text-only-changes)).
- Larger builds start on Sonnet or Opus.
- Reviews run on GPT-6.1 Sol, and their blocking findings are checked by
  GPT-6 Luna (`routing.verify`, see
  [How review checks itself](#how-review-checks-itself)).
- The [security pass](#the-security-pass) runs on Opus, so a change to
  security-sensitive code is read by both vendors' strongest models.

A reviewer, or the verifier, may run the same harness and model as the
builder it reviews: each is a job of its own in a fresh session, never the
builder's. Cross-vendor review, as here, is still the stronger setup.

The escalation tiers are model names on the implement route's own harness,
so a Codex builder escalates through Codex models: the daemon refuses a
Codex route whose tiers lack a `pricing` entry, such as the Claude tiers
above. Left out, `routing.orchestrate` is Opus on Claude, or, when no route
names Claude, `implement.L`'s harness and model; `tune` follows it.

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
own PRs for small, low-risk changes, once the repo's CI passes. The
dashboard's [Trust](using.md#trust) screen sets it for each repo as a
level, from its record:

```yaml
auto_merge:
  up_to: low        # the highest risk that merges itself: low or medium
  max_size: S       # the largest triage size: XS, S (default), M or L
  method: squash    # merge (default), squash or rebase
  breaker:          # optional; these are the defaults
    threshold: 2    # strikes that turn auto-merge off; -1 never
    window: 168h    # the rolling period strikes count in
    # cool_off: 72h # unset: stays off until you reset it

repos:
  - name: you/payments
    auto_merge: {up_to: none}   # a repo's setting replaces the top level's
```

A PR qualifies when its risk (after the [feature map](quality.md#feature-map)
and [its diff](#risk-from-the-diff) have had their say) is at or below
`up_to` and its size is at or below `max_size`. High-risk changes,
workspace changes, changes whose feature map couldn't be read, and changes
of a kind `risk.needs_person` lists (a migration, a new dependency, auth
code or a deleted test, by default) always wait for you. For a qualifying
PR, the factory:

1. Makes sure it's tested against the current base branch: where the
   base has a merge queue (GitHub's, or GitLab's merge trains) the PR goes
   into it; otherwise, if the branch is behind, the factory merges the
   base into it and waits for CI there first (see
   [Auto-merge guard rails](using.md#auto-merge-guard-rails)).
2. Marks the draft ready and turns on GitHub's auto-merge (or GitLab's
   "merge when the pipeline succeeds") for the commit the gates and
   reviewer passed, or the up-to-date commit CI passed. GitHub refuses if
   the branch has moved since.
3. Labels the PR and issue `factory:auto-merge` instead of
   `factory:awaiting-you`, and notifies you.
4. Lets GitHub merge it when the branch's required checks and reviews pass.
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

**The breaker.** A factory PR that's reverted on the base branch, or
whose merge commit main health sees a check go red on, is a strike
against its repo. `threshold` strikes within `window` turn auto-merge off
for that repo: new PRs wait for you, the PRs waiting to merge themselves
have their auto-merge turned off, and you're told once. It stays off until
you run `factory automerge reset owner/name` or press Reset on the
dashboard, or, with `cool_off` set, until that long after it tripped.
Every merged factory PR counts, whether you merged it or it merged
itself. A strike from red needs the check to have passed on the commit
before the merge. A cool-off counts from when it ends:
strikes after that count at once. See
[Auto-merge guard rails](using.md#auto-merge-guard-rails), which also
covers how reverts are found and what that costs in API calls.

A GitHub merge queue merges with its own method, so `method` doesn't apply
to PRs that go through one. On GitLab with merged results pipelines on
(with or without merge trains), the factory turns on auto-merge directly,
since each pipeline already tests the request merged with its target.

| Key | Default | What it does |
| --- | --- | --- |
| `breaker.threshold` | `2` | Strikes within the window that trip it. `-1` never trips. |
| `breaker.window` | `168h` | The rolling period strikes count in. |
| `breaker.cool_off` | unset | How long after tripping it resets itself, at least `1h`. Unset, only you reset it. |

A repo's `auto_merge` replaces the top level's whole, breaker included.

If the factory can't turn on auto-merge, it says why and the PR waits for
you as usual. On GitLab, `method: rebase` and `merge` both follow the
project's own merge method; only `squash` changes it. PRs waiting to
auto-merge count toward `awaiting_human_max` until they merge.

## Picking its own work

Trust level 6 for a repo (see [Trust](using.md#trust)): the issues the
factory files from its own findings start without the intake label, and
merge themselves under the repo's `auto_merge`. The Trust screen writes
it; by hand:

```yaml
repos:
  - name: you/api
    auto_merge: {up_to: medium, max_size: M}
    self_start:
      sources: [main_health, dependabot]   # also todos, sentry, hunt, orchestrator
      daily: 3        # issues a day that start on their own (default 3)
      budget: 12      # dollars a day for that work (default daily_total / 5)
```

| Source | What it finds |
| --- | --- |
| `main_health` | A check failing on the base branch (its fixes start whatever the level; at 6 they may merge themselves) |
| `dependabot`, `todos`, `sentry` | What [sources](using.md#issues-from-sentry-dependabot-and-todos) file |
| `hunt` | What the [old-bug hunt](#old-bug-hunting) files |
| `orchestrator` | The issues the orchestrator opens on its own schedule (they start whatever the level; at 6 they may merge themselves) |

Without `self_start`, or for a source it leaves out, the work waits for a
person: to label it, or to merge it. A tripped breaker stops it starting
its own work until it's reset.

## Sensitive code

One list says which code is sensitive: where a mistake is an exploit, a
leak or lost money. The security pass, risk from the diff and REVIEW.md's
`skip` all read it, so a path is treated the same by each:

- A change to a file in any area gets a [security pass](#the-security-pass).
- A change to code in `auth`, `crypto`, `secrets` or `payments` is the
  `auth` kind of change, and one in `migrations` a migration: both high
  risk, and waiting for you while `risk.needs_person` lists them (see
  [Risk from the diff](#risk-from-the-diff)).
- REVIEW.md's `skip` never covers a file in any area, nor one whose name
  has a word such as auth, secret, token, payment or billing in it, which
  config can't take away.

The `sensitive` block names areas, each a list of globs:

```yaml
sensitive:
  payments: [internal/ledger/, "*payment*"]  # replaces payments' defaults
  sql: []                                     # turns sql off
  reports: [internal/export/]                 # an area of your own
```

Setting an area replaces its defaults and `[]` turns it off; left empty
or null (`auth:`), it keeps them. An area of another name (lowercase letters, digits, `_` and `-`) is added: it gets
the security pass and REVIEW.md's guard, but isn't auth code. The block
applies to every repo. A glob the daemon can't read refuses the config.

A glob ignores case. One with no `/` matches any one part of a path, a
directory's name or the file's: `*auth*` catches `internal/auth/check.go`,
`AuthController.php` and `authors.go` alike; ending in `/`, only a directory's (`migrations/` matches
`db/migrations/0003.sql` at any depth). Any other is matched from the
repo's root, part by part, where `*` stops at `/` and a part that is only
`**` matches any number of parts (`app/**/keys.rb`). It matches what it
names and everything under it, so `internal/billing` names the directory
too; ending in `/`, only what's under it. A leading `/` or `./` changes
nothing. `\`, empty, `.` and `..` parts, and `**` inside a part (`a**b`)
are refused; to match a literal `[`, write `[[]` (`app/[[]id]/`). The
same globs, read the same way, are used by `security.paths` and
`risk`'s paths.

Two changes from before this list, when the security pass and risk each
read globs their own way. In `security.paths` and `extra_paths`, a
trailing `/` now means a directory only: `src/*/` no longer matches
`src/main.go`, nor `x/` a file named `x`. In `risk`'s paths, a `\`
(once an escape) or a `.`, `..` or empty part now refuses the config,
where before it was taken or matched nothing.

The areas that hold a merge for you (`auth`, `crypto`, `secrets`,
`payments`, `migrations`) use narrow names, so `authors.go`, a lexer's
`tokens.go` or `git/checkout.go` don't wait for a person. The wide
wildcards that would catch those are in `access`, which only starts a
pass (a pass that finds nothing is cheaper than a miss). The defaults:

- **`auth`:** `auth`, `auth.*`, `auth_*`, `*_auth*`, `auth-*`, `*-auth*`,
  `authn*`, `authz*`, `authenticat*`, `authoriz*`, `*authcontroller*`,
  `*authservice*`, `*authhandler*`, `*authprovider*`, `oauth*`, `oidc*`,
  `saml*`, `sso*`, `*jwt*`, `login*`, `logout*`, `session`, `session.*`,
  `session_*`, `sessions*`, `password*`, `passwd*`, `credential*`,
  `permission*`, `rbac*`, `acl`, `acl.*`, `devise*`.
- **`crypto`:** `*crypt*`, `*cipher*`, `*hmac*`, `tls`, `tls.*`, `cert`,
  `certs`, `cert.*`, `cert_*`, `cert-*`, `*certificate*`.
- **`secrets`:** `*.pem`, `*.key`, `*.crt`, `*.p12`, `*.pfx`, `id_rsa*`,
  `*secret*`, `*apikey*`, `*api_key*`, `.env`, `.env.*`, `*.env`,
  `.npmrc`, `.pypirc`.
- **`payments`:** `payment*`, `*_payment*`, `billing*`, `*_billing*`,
  `invoice*`, `*_invoice*`, `stripe`, `stripe.*`, `stripe_*`,
  `*_stripe*`, `wallet*`.
- **`access`** (wide names, a pass only): `*auth*`, `*oidc*`, `*saml*`,
  `*login*`, `*logout*`, `*signin*`, `*signup*`, `*session*`, `*cookie*`,
  `*password*`, `*permission*`, `*rbac*`, `*accesscontrol*`,
  `*access_control*`, `*csrf*`, `*cors*`, `*middleware*`, `*token*`,
  `*credential*`, `*payment*`, `*billing*`, `*invoice*`, `*checkout*`,
  `*wallet*`, `*stripe*`.
- **`input`** (untrusted input, files and outbound requests):
  `*sanitiz*`, `*upload*`, `*download*`, `*multipart*`, `*serializ*`,
  `*webhook*`, `*redirect*`, `*proxy*`, `*unzip*`, `*untar*`, `*archive*`.
- **`exec`:** `exec`, `exec.*`, `exec_*`, `exec-*`, `*exec`, `*exec.*`,
  `*_exec_*`, `*shell*`, `*subprocess*`.
- **`sql`** (and the code that runs migrations): `*.sql`, `*sql*`,
  `migrate`, `migrate.*`.
- **`migrations`:** `migrations`, `migrate/`, `migration/`, `alembic/`,
  `flyway/`, `liquibase/`.
- **`ci`:** `.github/workflows/`, `.gitlab-ci.yml`.
- **`dependencies`:** `go.mod`, `go.sum`, `go.work`, `vendor`,
  `package.json`, `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `bun.lock`, `bun.lockb`, `requirements*.txt`,
  `Pipfile`, `Pipfile.lock`, `poetry.lock`, `pyproject.toml`, `uv.lock`,
  `setup.py`, `Gemfile`, `Gemfile.lock`, `*.gemspec`, `Cargo.toml`,
  `Cargo.lock`, `composer.json`, `composer.lock`, `pom.xml`,
  `build.gradle`, `build.gradle.kts`, `gradle.lockfile`, `*.csproj`,
  `packages.lock.json`, `mix.exs`, `mix.lock`, `Package.swift`,
  `Package.resolved`.
- **`infra`:** `Dockerfile`, `Dockerfile.*`, `*.dockerfile`,
  `Containerfile`, `docker-compose*.y*ml`, `*.tf`.

## Risk from the diff

Once a change is built, the factory rates its risk again from the diff and
keeps the higher of that and triage's rating (see
[Risk from the diff](quality.md#risk-from-the-diff)). The `risk` block sets
what it looks for; these are the defaults:

```yaml
risk:
  medium_lines: 150     # changed lines (lockfiles left out) for medium risk
  high_lines: 300       # ... and for high; -1 turns either off
  medium_files: 10      # changed files for medium risk
  high_files: 25        # ... and for high
  high_paths: []        # e.g. [billing/, "config/secrets*"]
  # medium_paths: see below; auth code and migrations: the sensitive list
  needs_person: [migrations, new_dependencies, auth, deleted_tests]
```

| Key | Default | What it does |
| --- | --- | --- |
| `medium_lines`, `high_lines` | `150`, `300` | A change of at least this many changed lines is at least medium, or high, risk. Lockfiles and binary files don't count. `-1` turns one off. |
| `medium_files`, `high_files` | `10`, `25` | The same for the number of changed files. |
| `medium_paths` | deploy and infrastructure files: `Dockerfile`, `Dockerfile.*`, `*.dockerfile`, `docker-compose*.yml`/`.yaml`, `*.tf`, `terraform/`, `infra/`, `deploy/`, `k8s/`, `helm/`, `Procfile`, `fly.toml`, `vercel.json`, `netlify.toml` | A change touching one is at least medium risk. |
| `high_paths` | none | A change touching one is high risk. |
| `auth_paths` | none | Globs added to the [sensitive list](#sensitive-code)'s `auth`, `crypto`, `secrets` and `payments` areas. A change to a file matching one of those is the `auth` kind of change: high risk. Tests, docs and images (`.md`, `.txt`, `.png` and the like) and vendored code (`vendor/`, `node_modules/`, `third_party/`) don't count. |
| `migration_paths` | none | Globs added to the sensitive list's `migrations` area. A change to a file matching one, other than a test or a doc, is a migration: high risk. |
| `needs_person` | all four | The kinds of change that never merge themselves, whatever their risk: `migrations`, `new_dependencies`, `auth`, `deleted_tests`. `[]` lists none; the kinds still raise the risk. |

Setting `medium_paths` or `high_paths` replaces its defaults, so list the
defaults you want to keep too. Which code is auth code or a migration is
the [sensitive list](#sensitive-code)'s to say; `auth_paths` and
`migration_paths` only add to it. The reason names the area and the
glob: "changes crypto code: `*crypt*` in `internal/crypto/aes.go`".
Patterns match as the sensitive list's globs do.

The block applies to every repo. A diff the factory can't rate (it can't
read a manifest, say) waits for you as if it needed a person.

## Budgets

The daemon enforces its own caps, under `budgets` in `factory.yaml`. The
values below are this repo's.

| Key | What it caps |
| --- | --- |
| `per_job` | Dollars per implement job, by size: `{XS: 0.5, S: 2, M: 6, L: 15}`. Without an `XS` entry, XS uses S's. |
| `triage`, `review` | Dollars per triage or review job. |
| `verify` | Dollars per verifier run, which checks a review's blocking findings (0.5 here; left out, `triage`'s). |
| `daily_total` | Total dollars a day. When it's hit, no new jobs start until midnight UTC. A [workspace](#workspaces) can have a smaller `daily_total` of its own: once that's spent, only its jobs wait. |
| `concurrent_jobs` | Jobs running at once (3). A paired critic (`review.paired_high_risk`) or a [security pass](#the-security-pass) runs inside its review and isn't counted. |
| `awaiting_human_max` | No new implement jobs while this many PRs wait for you (5). |
| `min_free_disk_gb` | The [disk space](#disk-space) floor (default 10). |
| `pause_at_usage` | Holds a harness's jobs once a subscription usage window is this full, in percent (default 95). See below. |
| `confirm_over` | Dollars. An item whose cost estimate is over this waits for your OK before it's built (default 0, off). See [Asking before costly builds](#asking-before-costly-builds). |
| `max_turns` | Turns (model calls) per build, by size: `{XS: 40, S: 80, M: 150, L: 300}`; one number sets every size. Other steps such as triage and review keep a cap of 80. A build that runs out, or hits its `per_job` cap, stops and waits for you; see [When a build runs out of turns or budget](using.md#when-a-build-runs-out-of-turns-or-budget). The defaults are a starting point, set above what builds usually take, so that the turn cap, which warns the builder to commit and note what's left, comes before the dollar cap, which doesn't. Raise or lower them as you see how your builds run. |

The orchestrator's spend has its own caps, `orchestrator.run_budget` per
run and `orchestrator.daily_budget` per day, which don't count against the
build budget. Tuning runs and the dashboard [chat](using.md#chat-opening-issues-with-the-orchestrator)'s
replies (each up to `run_budget`) share that daily cap. Each workspace's orchestrator and chats can have a smaller one of their own, the workspace's `orchestrator.daily_budget`. `factory compare` arms split what's left of today's budget.

Files people attach are removed once no one needs them.
`orchestrator.keep_files_for` (default `720h`, 30 days; at least `1h`) is
how long a chat's files stay after its last message. Files sent to a
running build go as soon as its item is done (its PR merged or closed, or
its issue closed), or that long after the build ended. The tuning loop
never changes this setting.

**Subscription usage limits.** On a Claude subscription, Claude Code has a
five-hour and a weekly usage window, and so does Codex on a
[ChatGPT plan](setup.md#codex-on-a-chatgpt-plan). The factory records the latest
reading each run reports, and `factory status`, a line under the
dashboard's header, its **Spend** page and the orchestrator show how much
is used and when it resets. When the five-hour or weekly window reaches `pause_at_usage`, that
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

## Asking before costly builds

After triage, the factory estimates what building an item will cost. With
`budgets.confirm_over` set, an item whose estimate is over it waits for
your OK before implement starts:

```yaml
budgets:
  confirm_over: 5             # dollars; 0 or left out is off

repos:
  - name: you/api
    confirm_over: 20          # this repo's own; 0 turns it off here
  - you/web                   # uses budgets.confirm_over
```

The estimate is a range, from the median to the 75th percentile (p75) of
what past items of the same size cost to build: every implement attempt
and escalation, the gates, the review and the critic. Triage's own cost
isn't in it, since it's spent by then. The factory picks its history in
this order:

1. Items of that size in the same repo, once there are at least 5.
2. Items of that size in every repo, once there are at least 5.
3. Your budgets: half to all of one build's `per_job` cap for that size,
   plus one review's (and the critic's, for a high-risk item). With
   `fast: true` on the route, the build's cap is scaled as for the job.

Items count once nothing is queued or running for them, whether they
merged, wait for your review, or parked along the way. Only an item's
latest cycle counts: the jobs since its last triage, so a build sent back
with `factory:retry` isn't counted twice. The estimate always
says what it's based on, for example "based on 14 past M items in you/api:
median $1.80, p75 $3.10".

The p75 is what's compared with `confirm_over`, not the median. Half of
all items cost more than their median, so a median under the limit would
let through many items that end up over it. The p75 is what three in four
items of that size came in under, without being set by the one build that
went badly.

**On a subscription plan.** When Claude Code runs on a subscription token
(`CLAUDE_CODE_OAUTH_TOKEN`, with none of `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK` or
`CLAUDE_CODE_USE_VERTEX`, which all bill by use), its work draws on
your plan's usage windows instead of billing dollars. The estimate splits
it out job by job, by the harness each past job ran on: only the dollars billed, such as a Codex review on an API key, count against
`confirm_over`, and the plan's part is shown as "on your plan", with the
cost Claude Code reports and how full its windows are now. An item built
and reviewed entirely on the plan never waits. Plan runs are recorded at
the cost Claude Code reports for them, which is what an API key would
have paid, so the history works for both. Codex counts as on a plan too
when it's signed in to ChatGPT (`FACTORY_CODEX_AUTH=chatgpt` or
`CODEX_ACCESS_TOKEN`), and as billed on an API key.

Main health fixes are never held: a red main is urgent. That's items the
factory filed for a red or flaky main, marked when they came in; a high
priority doesn't count. Everything else
is, including issues the orchestrator files and ones from
[sources](using.md#issues-from-sentry-dependabot-and-todos). For how an
item waits and how to approve it, see
[Cost estimates](using.md#cost-estimates).

## Plans before big builds

An item triage rates `L` gets a short plan before it's built, posted on
the issue, and waits for your `factory:go` (see
[Plans before big builds](using.md#plans-before-big-builds)):

```yaml
plan:
  enabled: true      # the default; false turns plans off
  sizes: [L]         # sizes whose plan waits for you (default [L])
  medium: off        # critic: the critic checks an M item's plan instead
```

The plan runs on the builder's route for the item's size
(`routing.implement.L` for an L item), read-only, with `budgets.review`
as its cap and `timeouts.plan` (default 15m) as its limit. The critic's
check runs on `routing.critic` (else `implement.L`). A size in `sizes`
waits for you even if `medium` is `critic`. Main health fixes never plan.
An item re-sized to a planned size after triage (by the orchestrator's
`enqueue`), or retried from an old build on the dashboard with no plan
since its triage, is planned first. A plan already asked for keeps
holding the build if you later drop its size or turn plans off: it waits
for its `factory:go`, or `factory:retry` to start over.
`factory eval` and `compare` turn plans off: nobody is there to approve
one.

### Concise asks

```yaml
concise_asks: true   # the default; false writes plans and chat replies as before
```

Plans and chat replies are what you read and act on, so while
`concise_asks` is on their prompts end with `prompts/concise.md`: lead
with what you decide, short numbered steps, at most five items a list, no
preamble or recap. It's adapted from the MIT-licensed
[i-have-adhd](https://github.com/ayghri/i-have-adhd) skill; edit the file
to change the rules. It's under Settings > Asks too.

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
  review_eval: false          # run the reviewer evals before keeping a review change
  review_eval_set: evals      # where the reviewer cases are
  review_eval_tolerance: 0.05 # how much worse the reviewer may get, as a share
  review_eval_cases: 20       # the most cases each run reviews
```

| Key | What it does |
| --- | --- |
| `every` | The cadence, at least `1h`. The clock starts the first time the daemon runs with tuning on, and survives restarts. A clock more than two cadences old (tuning was off a while) starts again. A run reads at most two cadences back. |
| `min_items` | A run with fewer items built since the last one is skipped, and you're told. `factory tune` ignores it. |
| `budget` | The dollar cap for one run. Its spend counts toward `orchestrator.daily_budget`, not the build budget. A run waits while either of those is spent for the day. |
| `repo`, `path` | Where this `factory.yaml` lives in one of the factory's repos. With `repo` set, each proposal with changes opens a draft PR there on `factory/tuning-<date>`, made to that copy of the file. The file must exist there as a plain file, not a symlink or behind one, and still load with the change. If the day's branch has commits the factory didn't make, it uses `factory/tuning-<date>-2` and so on, rather than push over them. |

| `review_eval`, `review_eval_set`, `review_eval_tolerance`, `review_eval_cases` | Off by default. On, each proposed change is checked on its own for whether it changes how review works, as resolved with every fallback: the review route for each risk, the verifier's, critic's and security pass's routes (so `routing.implement.L` counts while `routing.critic` is unset), the `review` and `security` settings, the review and verifier budgets (so `budgets.triage` counts while `budgets.verify` is unset), fast mode's multiplier, the review, critic, verify and security timeouts, and `prompts_dir`. If any does, the [reviewer evals](quality.md#reviewer-evals) in `review_eval_set` run on the current config and on the proposal: at most `review_eval_cases`, taken in turn from the planted, clean and revert cases in name order, so a small set still holds each kind. If recall falls or the false-positive rate rises by more than `review_eval_tolerance` (0 allows no fall at all), fewer reverts are blocked, or more cases get no ruling, the changes that alter review are dropped with why, naming the cases that got worse; the others stay. The proposal's summary gives both runs' numbers. Each run may spend half of what's left of today's budget, counted toward it, and waits like the tuning run when the budget is spent. If the evals can't run or finish (no cases, an error, the cap reached), the review changes are dropped. |

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
| `min_findings` | How many alike blocking findings, from at least two items, start a learn job when no edit of yours does. At least 2 (default 2). The same count of alike findings you overruled starts one that may propose lines for `REVIEW.md`. |
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
| `every` | `10m` | How often. Each reading is a few API calls per repo. The [revert watch](using.md#auto-merge-guard-rails) runs on this cadence too, on or off. |
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

## Backlog grooming

On a cadence, the factory can read each repo's open issues and flag likely
duplicates, stale issues and ones too big for one change, for you to
confirm (see [Backlog grooming](using.md#backlog-grooming)). It's off
unless you set `every`, since it comments on people's issues:

```yaml
grooming:
  every: 168h        # weekly; at least 24h; 0 or none (the default) turns it off
  stale_after: 90d   # no activity this long, and it may be called stale; at least 7d
  max_issues: 50     # issues one run reads, at most 200
  budget: 0.5        # dollars per groom job; left out, budgets.triage
  # repos: [acme/api]  # default: every repo
```

| Key | Default | What it does |
| --- | --- | --- |
| `every` | off | How often each repo is groomed, at least `24h`. The clock starts the first time the daemon runs with grooming on and survives restarts, so the first run comes one cadence later. `factory groom` runs one now. |
| `stale_after` | `90d` | An issue with no activity (a comment, an edit, a label) for this long may be called stale. Days (`90d`) or hours (`2160h`); at least 7 days. |
| `max_issues` | `50` | How many issues one run reads: half the newest, half the longest idle. The rest wait for a later run. |
| `budget` | `budgets.triage` | Dollars per groom job. It counts toward `budgets.daily_total`, and a run that's due waits while today's budget is spent. |
| `repos` | every repo | Which repos. |

`routing.groom` picks its model. Left out, it runs on triage's harness, like
`routing.learn`: Sonnet on Claude, or triage's own model on Codex.
`timeouts.groom` is 10 minutes by default. With grooming on, the daemon
won't start without `prompts/groom.md`. `factory eval` and
`factory compare` never groom.

## Old-bug hunting

When the factory has nothing else to do, it can review one area of a
repo's [feature map](quality.md#feature-map) at a time for bugs already
in the code, and file what it finds for you to approve (see
[Hunting for old bugs](using.md#hunting-for-old-bugs)). It's off unless
you turn it on, since it spends on its own schedule:

```yaml
hunt:
  enabled: true
  every: 24h          # at least 1h
  budget: 3           # dollars per hunt; left out, budgets.review
  daily_budget: 3     # dollars on hunts a day; left out, one hunt's budget
  max_findings: 3     # issues one hunt files at most, up to 10
  daily_issue_cap: 3  # issues hunts file per repo a day; left out, max_findings
  # repos: [acme/api] # default: every repo
```

| Key | Default | What it does |
| --- | --- | --- |
| `enabled` | `false` | Turns hunting on. |
| `every` | `24h` | How often a hunt may start, at least `1h`. The first starts as soon as the factory is idle; the clock survives restarts. A hunt starts only while no other job is queued or running, and is queued behind every other kind. |
| `budget` | `budgets.review` | Dollars per hunt. |
| `daily_budget` | `budget` | What hunts may spend a day. A hunt also counts toward `budgets.daily_total`, and starts only when a whole hunt's `budget` still fits in both. |
| `max_findings` | `3` | The most issues one hunt files; the most severe go first. |
| `daily_issue_cap` | `max_findings` | How many issues hunts may file per repo a day, apart from `sources.daily_issue_cap`. Findings it holds back aren't lost: the area counts as not hunted, and comes round again next. |
| `repos` | every repo | Which repos. Each hunt takes the repo hunted longest ago. |

`routing.hunt` picks its model; left out, it's the reviewer's
(`routing.review`'s harness, model and effort). `timeouts.hunt` is 20
minutes by default. With hunting on, the daemon won't start without
`prompts/hunt.md`. A repo with no `features.yaml` isn't hunted.
`factory eval` and `factory compare` never hunt.

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
`preview`; run `factory images --rebuild` after adding one. An image you
build yourself needs Playwright installed globally (`npm install -g
playwright && playwright install --with-deps chromium`, with `NODE_PATH`
pointing at the global modules). Without it, the PR says so and the build
goes on.

**On GitLab**, the factory's token needs the Maintainer role and GitLab
17.2 or later to remove the images when a PR closes. With less, they're
uploaded and shown as usual, but stay until you delete them (Settings >
General > Uploads); the comment says so.

**Branch deploys.** A hosting integration that deploys every branch
(Vercel, Netlify) sees each rewrite of `factory/previews`; exclude that
branch in its settings.

A page with a long scroll is cut at 4000 pixels. Previews aren't
supported for a repo whose [workspace changes together](#repos-that-change-together)
yet.

## PR descriptions

What goes in a PR's description is in
[The PR description](using.md#the-pr-description). One setting decides
whether its Testing section quotes the end of each passing check's output:

```yaml
pr:
  check_output: private   # private (the default), always or never
```

- `private` quotes it only on a repo the forge reports as private: its
  `visibility` must be `private` on GitHub and on GitLab (an internal repo
  or project counts as public). The factory trusts each answer for five
  minutes; if it can't tell, the repo counts as public.
- `always` quotes it on every repo. The output is redacted, but redaction
  only knows the secrets it's told about, so use this for public repos
  only if their checks print nothing private.
- `never` quotes no output anywhere.

Each check's pass or fail, and what the test-proves-fix check and the
coverage floor found, show either way.

## Claude-only review

Setup routes jobs to what you connect: connect only Claude and it builds
and reviews; connect Codex too and Codex reviews what Claude builds (or
the other way round, if Codex came first). The presets on the Settings
screen write routing like the below for whichever you have. This section
explains what they do with one harness.

With both, the reviewer is never the model that wrote the code. If you
only have Claude, Claude reviews too.

A reviewer may run the same model as the builder, including a tier a retry
escalates to: every review is a job of its own in a fresh session, which
never resumes the builder's session or sees its reasoning, only the change.
So Opus can both build (as the top escalation tier) and review. A weaker
reviewer would miss more, so keep the reviewer at least as strong as the
strongest builder; prefer a different model where it is as strong.

Change these entries in `factory.yaml` and leave the rest:

```yaml
routing:
  triage:            {harness: claude, model: haiku}
  implement.XS:      {harness: claude, model: haiku}
  implement.S:       {harness: claude, model: haiku}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: sonnet}
  review:            {harness: claude, model: opus}
  review.high_risk:  {harness: claude, model: opus, effort: high}   # or fable
  verify:            {harness: claude, model: opus, effort: low}
  # drop security: it runs on a builder tier that didn't build the item,
  # never on the reviewer's model
escalation:
  implement: [haiku, sonnet, opus]   # a build that keeps failing ends on Opus
```

No OpenAI key is needed once no route names `codex`, and `factory setup`
doesn't ask for one.

Every other step defaults to a route this allows: the verifier to the
reviewer's model when triage's builds, the critic and the plan check to
`implement.L`, the old-bug hunt to `review`, the orchestrator to Opus. The
security pass must not use a reviewer's model, so with Opus reviewing it
runs on Sonnet for an item Haiku built and on Haiku for one Sonnet built;
give it Fable (`routing.security`) if your account has it.

This is weaker than cross-vendor review: two models from one family tend
to share blind spots, and one model reviewing its own build shares them
all, fresh session or not. The critic that also reads high-risk diffs
uses `routing.critic` (default: `implement.L`) even when that is the
builder's model, in a fresh session too.

## Codex-only

Every step can run on Codex instead, with no Claude credential: on an
OpenAI API key, or on a ChatGPT plan (`FACTORY_CODEX_AUTH=chatgpt`, signed
in with `./factory codex-login`). As with Claude, the reviewer and the
verifier may run a builder's model, each in a fresh session, but a
different model catches more. Codex models compare by their exact name, so
`gpt-6.1-sol` and `gpt-6-luna` are two models. With two, one builds and the
other reviews. The names below are
placeholders, as in this repo's own `factory.yaml`: use the ones your sign-in
offers (`/model` in the `codex` app, signed in the same way, lists them; so
does OpenAI's Codex models page). Each needs a `pricing` entry, and
`escalation.implement` must list the builder's models:

```yaml
routing:
  orchestrate:       {harness: codex, model: gpt-6.1-sol}
  triage:            {harness: codex, model: gpt-6-luna}
  implement.XS:      {harness: codex, model: gpt-6.1-sol, effort: low}
  implement.S:       {harness: codex, model: gpt-6.1-sol, effort: medium}
  implement.M:       {harness: codex, model: gpt-6.1-sol, effort: high}
  implement.L:       {harness: codex, model: gpt-6.1-sol, effort: high}
  review:            {harness: codex, model: gpt-6-luna, effort: medium}
  review.high_risk:  {harness: codex, model: gpt-6-luna, effort: high}
  verify:            {harness: codex, model: gpt-6-luna}
  security:          {harness: codex, model: gpt-6.1-sol, effort: high}
escalation:
  implement: [gpt-6.1-sol]      # Codex models: a retry stays on the builder's
pricing:                        # every Codex model needs one, even on a plan
  gpt-6.1-sol: {input_per_mtok: 2, cached_input_per_mtok: 0.10, output_per_mtok: 10}
  gpt-6-luna:  {input_per_mtok: 0.10, cached_input_per_mtok: 0.01, output_per_mtok: 0.50}
```

Use the models your plan offers. If it has a third, review with it and let
the builder escalate through the other two.

- **The builder.** With the docker runner a Codex builder runs with
  `--sandbox danger-full-access`, as a Claude builder runs with
  `bypassPermissions`. It installs dependencies, reaches the job's services
  (a database over the services network) and writes its verdict outside the
  checkout, and the boundary is the job's container and the egress
  allowlist (see [Isolation](security.md#isolation)). With the local runner
  it keeps Codex's `workspace-write` sandbox, with the out directory added
  for the verdict and no network, so a check that installs or reaches a
  service fails there. Read-only steps keep Codex's read-only sandbox.
- **The orchestrator** gets the daemon's MCP tools as Codex `mcp_servers`,
  with their calls approved up front (`codex exec` can't ask, and refuses
  a call that needs approval). Each tool checks its own call, as for
  Claude. A tool call Codex couldn't make fails the run.
- **No session resume.** A fix round on Codex starts a fresh session; only
  Claude continues the last one.
- **Lessons** go to `AGENTS.md`, which Codex reads, and the readiness check
  stops asking for a `CLAUDE.md` that loads it.
- **On a ChatGPT plan** Codex runs one job at a time (two runs can't share
  one sign-in), so with every step on Codex the factory builds, reviews
  and triages one job after another; `budgets.concurrent_jobs` doesn't
  raise that. Jobs cost $0 here, so the dollar budgets, the cost estimate
  and `confirm_over` never hold anything back; the plan's 5-hour and weekly
  limits do (`budgets.pause_at_usage`). Codex has no per-run dollar cap,
  so on an API key a run stops at its timeout, not at its budget, and the
  budget counts after it ends.

This gives up what Claude-only does: two models of one vendor share more
blind spots than two vendors' do. And with two models the security pass
runs on the builder's, reading the change with its own prompt.

## How review checks itself

Three things keep the reviewer honest and the fix rounds short. All of
them are on by default; the `review` block changes them:

```yaml
review:
  verify: true              # a verifier rules on each blocking finding
  paired_high_risk: false   # high risk: reviewer and critic side by side
  incremental: true         # after round one, review what changed
  context_bytes: 20000      # whole diff as context up to this size
  rethink: true             # one fresh attempt before the round cap parks
```

| Key | Default | What it does |
| --- | --- | --- |
| `verify` | `true` | Each blocking finding goes to the verifier, `routing.verify`, which confirms or refutes it against the code, or rules it a note: true, but no threat to the merge. Only a confirmed finding blocks; a refuted or noted one stays on the PR as a note, and a noted one keeps the PR from merging itself. A critical or security finding can't be made a note. If the verifier can't run (the daily budget can't cover `budgets.verify`, its harness is held or signed out, it fails, or it skips a finding), the finding blocks. `false` blocks on the reviewer's word, as before. |
| `paired_high_risk` | `false` | A high-risk item's reviewer and critic read the change at the same time, instead of the critic after the reviewer passes it. The round blocks on any finding either raises that the verifier confirms. It costs a critic run on every high-risk round, but saves a round when the critic would have blocked. When the critic can't start (its harness is held or signed out, busy with another job it can't share a sign-in with, or the daily budget can't cover its `budgets.review` cap), the round isn't paired and the critic follows the reviewer as usual. The paired critic doesn't count toward `concurrent_jobs`: each high-risk review in progress can run one agent more than that allows. |
| `incremental` | `true` | From round two, a reviewer gets the diff since the commit it last reviewed, with its open findings and the builder's answers, rather than the whole diff again. |
| `rethink` | `true` | An item the reviewer blocks past `escalation.max_review_rounds` gets one fresh build from the base branch on the next size's implement route, with the unresolved findings and the note that the approach may be wrong, before it parks. Once per triage; with `budgets.confirm_over` set, only when the item's estimate (its p75, or the approved amount) covers it. See [Rethink before parking](quality.md#rethink-before-parking). `false` parks at the cap, as before. |
| `context_bytes` | `20000` | A re-review also gets the whole diff, for context, when it's at most this many bytes; a bigger one is listed by file, which the reviewer can read in its checkout. |

`routing.verify` should be cheap: it reads a few claims and the code
around them. Left out, it's triage's harness and model, unless a builder
can run on those (as Haiku can here, for S builds), and then the
reviewer's. Set, it's used even when a builder can run on it: like the
reviewer, the verifier starts a fresh session and never resumes the
build's. Each verifier run is a job of its own, kind `verify`, with its own cost and
log; so is a paired critic, kind `parallel_critic`. Both run inside the
review that starts them and never wait in the queue. Cancelling one
cancels its review.

See [Review findings](quality.md#review-findings) for what happens to each
finding.

## The security pass

A change to code where a mistake is an exploit gets a review of its own,
on `routing.security` with `prompts/security.md`, beside the reviewer.
Its critical findings block whatever the reviewer decides (see
[Security pass](quality.md#security-pass) for when it runs and what
happens to its findings). It is on by default; the `security` block
changes it:

```yaml
security:
  enabled: true
  paths: [...]                     # left out: the sensitive list
  extra_paths: [internal/billing/] # added to paths
  content: true                    # risky added code starts a pass too
```

| Key | Default | What it does |
| --- | --- | --- |
| `enabled` | `true` | `false` turns the pass off: no change gets one. |
| `paths` | the [sensitive list](#sensitive-code) | Globs of files whose change starts a pass. Setting it replaces the sensitive list for the pass; prefer setting `sensitive`. |
| `extra_paths` | none | Globs added to `paths`, or to the sensitive list. |
| `content` | `true` | Code a change adds that runs a command, builds SQL from strings, evaluates code, deserializes an unsafe format, writes raw HTML or turns off TLS checks starts a pass too, in any file but tests and docs. |

Left out, `paths` is every area of the [sensitive list](#sensitive-code),
which is where to say what is sensitive: `paths` and `extra_paths` still
work, but set, `paths` replaces that list for the pass alone (REVIEW.md's
`skip` still never covers what either names). Globs match as the
sensitive list's do; a trailing `/` now means a directory only (see
[Sensitive code](#sensitive-code)). Tests
and docs (`.md`, `.rst`, `.adoc`) never start a pass. `paths: []` leaves
only `content` and the feature map to start one.

A change to a `high` feature in the [feature map](quality.md#feature-map)
gets a pass whatever these say.

`routing.security` should be strong, and differ from the reviewer: two
reads by the same model share its blind spots. The daemon refuses one on
the same harness and model as `review`, `review.medium_risk` or
`review.high_risk`, or a config where the route it would fall back to
is a reviewer's. Left out, it is the first of `routing.critic` (else the
strongest Claude tier) and `implement.L`, `M`, `S` and `XS` that is
neither a reviewer's model nor one that built the item, so the pass
never reads code its own model wrote when another will do. Each pass is capped at `budgets.review`
and `timeouts.review`. Like any routing entry, `routing.security` can
name skills and instructions of its own (see
[Skills and instructions for each step](#skills-and-instructions-for-each-step)).

Each pass is a job of its own, kind `security`, that runs inside its
review like the verifier. When its harness is paused, signed out or busy
with a job that runs one at a time, the review goes back to the queue
without using an attempt and isn't claimed again until that harness is
free (up to five times). When it can't start for another reason (such as
the daily budget), fails, or ends without a valid verdict, the review
fails: it is retried once, then the item parks. The daemon won't start without
`prompts/security.md` while the pass is on.

A repo's own `REVIEW.md`, read from its base branch, also steers review:
its text goes to the reviewer, the critic and the verifier, and its front
matter can skip paths and cap minor findings. It lives in the repo, not in
`factory.yaml`; see [REVIEW.md](quality.md#reviewmd).
