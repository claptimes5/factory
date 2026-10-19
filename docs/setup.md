# Setup

This guide covers installing the factory, the dashboard's get-started page
and what `factory setup` does, running
it as a service, upgrading, and doing every setup step by hand. For the
short version, see the [Quickstart](../README.md#quickstart).

You need a Linux machine (or [WSL2 on Windows](#windows-wsl2)) and admin
rights on the GitHub repo you want the factory to work on.

Starting a new project? Give [new-project.md](new-project.md) to whoever
sets up the repo. It lists what a repo needs before the factory can work on
it.

## Dependencies

`sh scripts/install.sh` checks these for you (see [Install and run](#install-and-run)).

| What | Version | Why |
| --- | --- | --- |
| Go | 1.26+ | Builds the `factory` binary (it fetches the 1.27 toolchain itself). |
| Docker | 28+ for the egress allowlist; any recent one with `mode: open` | Runs every job in a container. |
| git | any recent | Worktrees and pushes. |
| Node.js | 22.12+ | Only to rebuild the dashboard; the built app is committed. |
| A Claude API key or subscription token | | The builder and Claude reviewers. |
| An OpenAI API key or a ChatGPT plan | | Only if a reviewer uses Codex. |

You don't install Claude Code, Codex or Playwright yourself. They're pinned
in the job image (`deploy/job-image/Dockerfile`), which setup builds. The
binary has no other runtime dependencies: SQLite is built in, so
`CGO_ENABLED=0` works.

Docker 28 or newer is needed for the [egress allowlist](security.md#network-the-egress-allowlist)
and for [services](configuration.md#services-databases-and-the-like).

## Install and run

```sh
git clone https://github.com/claptimes5/factory && cd factory
sh scripts/install.sh
./factory serve
```

Then open http://127.0.0.1:8080/app/get-started and follow the steps (see
[Setting up in the browser](#setting-up-in-the-browser)). On a server, or
in a script, run `./factory setup` in a terminal instead: it asks the same
things (see [What setup asks for](#what-setup-asks-for)).

`scripts/install.sh` checks Go, git and Docker (and Node with `--web`), and
for anything missing or too old prints the command that fixes it on your
system: apt, dnf, pacman, Docker Desktop's WSL integration, or brew. It
installs nothing and never uses sudo itself. When Go and git are there it
builds `./factory`. To build by hand instead:

```sh
CGO_ENABLED=0 go build -o factory ./cmd/factory
```

Once setup is done the dashboard is at http://127.0.0.1:8080/app/. If setup's
readiness check found blockers, the dashboard's **Setup** screen shows what
each repo still needs and follows the factory's work on it; once it says
the repo is ready, label an issue `factory:ready`.

One thing setup can't do for you: **protect `main`**. In the repo's
settings, require a pull request with your review and the `test` and `web`
checks. `.github/CODEOWNERS` makes you the owner of `.github/`,
`factory.yaml`, `prompts/` and the lint config.

## Setting up in the browser

Until the factory can reach your repos, `./factory serve` doesn't stop with
an error: it starts the dashboard with only its get-started page, prints

```
Open http://127.0.0.1:8080/app/get-started to finish setup.
```

and every other screen leads there. The page is a list of steps, each
marked done, needing attention, or optional, with the next one open:

1. **This machine:** the same checks as `./factory doctor` runs on the
   machine, each problem with its fix, and a Re-check button.
2. **Repositories:** which repos the factory works on, saved in
   `factory.yaml`.
3. **GitHub:** "Create an app for me" sends you to GitHub with a new app
   filled in, with only the permissions the factory needs. Click "Create
   GitHub App" and GitHub brings you back; the app's key is saved next to
   `factory.env`. Then install it from the link the page shows and press
   Check. You can use an app you already made (its ID and the path of its
   key on the factory's machine) or, for a quick try, a personal access
   token. The page then checks it can reach each repo, says when a repo's
   default branch differs from the one the factory uses, and shows which of
   the factory's labels each repo is missing, with a button that creates
   just those.
4. **GitLab** (with GitLab repos): links to make a legacy or fine-grained
   token, and the same access and label checks.
5. **Claude** and **OpenAI (Codex)**, when `factory.yaml` uses them: the
   same choices as `factory setup`. For a ChatGPT plan, **Sign in** runs
   Codex's device-code sign-in on the factory's machine and shows the link
   and code to enter, with a Cancel button. A code nobody enters within
   20 minutes ends the sign-in. The page says when you're
   signed in. Only someone who may save settings sees the code.
6. **Notifications** and **Sentry** (both optional), with a button that
   sends a test notification.
7. **Job images:** which are built, and a Build button that builds the
   missing ones while you watch the log. A build `factory serve` started
   on its own shows here too, and the two never run at once.
8. **Finish**, once the factory can reach every repo, and not while an
   image builds or a Codex sign-in runs. If something won't
   work yet (no model key, say), it says what. Finish restarts the factory
   with the new settings and the page opens the dashboard when it's up.

The page reads the settings as the factory will run with them: a
variable exported where the factory starts wins over the one saved in
`factory.env`, as at startup. That includes a `GITHUB_TOKEN` exported for
other tools such as `gh`: unset it where the factory starts if the
factory shouldn't use it. `factory setup` in the terminal offers what is
saved instead, takes an exported value only for a key the file lacks,
and never takes an exported `GITHUB_TOKEN`. Both name each saved key an
export overrides.

Saved keys and tokens never come back to the browser: the page shows only
whether each is set and its first and last few characters. Answers go in
the same files, the same way, as `factory setup` writes them. Saving needs
the dashboard's password, or the browser on the factory's machine itself
(an SSH tunnel counts); see [security](security.md).

The page stays at `/app/get-started` once the factory runs, linked from
the Settings and Setup screens, so you can change a key or a repo later or
see whether setup is still complete: each time it opens it checks again
that the factory reaches each repo, that the app is still installed and
that the labels are there. Finish then restarts the factory to apply what
changed, once running jobs finish.

## Job images

Jobs run in Docker images built from `deploy/job-image/Dockerfile`. When
`factory serve` starts, it builds any of them that are missing in the
background, one at a time. Jobs that need an image wait until it is
built, then start. A build that takes over an hour is stopped, and a
restart asked for meanwhile waits for the build and says so. If a build fails, the factory logs it and sends it to
your phone, and those jobs fail as they would without the image. You can
also build them from the dashboard's get-started page, or with
`./factory images` (`--rebuild` rebuilds them all).

Every image the factory builds has the label `io.factory.built=1`. A
rebuild leaves the old copy without a tag, so the factory removes those
old copies after each build, when `serve` starts, and about once an
hour. It only removes unused images with that label, never your own.
An image a repo names with `image` (and no `image_from`) is yours to
build: the factory only tells you when it is missing.

## Checking your setup

`./factory doctor` checks everything the factory needs, any time
something seems off: Docker works for you and is new enough for what
`factory.yaml` uses, the job images are built, there's disk to spare, the
Claude, Codex and GitHub credentials your config needs are in
`factory.env`, and the dashboard's port is free. It sends nothing to
GitHub or the model providers. Each problem comes with its fix:

```
$ ./factory doctor
Checking this machine, factory.yaml and factory.env:
  ✓ git 2.43.0
  ✓ Docker 27.5.1
  ✗ Docker 27.5.1 is too old for the egress allowlist, which needs 28 or later
      upgrade Docker: curl -fsSL https://get.docker.com | sh   (Docker's own packages; a distro's may be older than 28)
      or set runner.egress.mode: open in factory.yaml, which lets jobs reach the whole internet
      (docs/security.md#network-the-egress-allowlist)
  ✗ Job image factory-job:latest (for you/app) isn't built
      ./factory images
  ✓ Disk space: 112 GB free in the data directory (/home/you/factory/data), 112 GB free in Docker's storage (/var/lib/docker)
  ✓ Claude: subscription token
  ✓ Codex: OpenAI API key
  ✓ GitHub: app 123456, key /home/you/factory/factory-app.pem
  ! ttyd isn't installed (optional: browser terminals into running jobs)
      sudo apt install ttyd
  ✓ Dashboard port 127.0.0.1:8080 is free

Fix the ✗ lines and run ./factory doctor again. More in docs/setup.md#checking-your-setup.
```

It exits non-zero while a ✗ line remains. `factory setup` starts with the
same checks of the machine, and `factory serve` runs them as it starts
(all but the disk, which it watches while running, and the port, which it
then holds): each failure is logged and sent to your phone, and the
daemon starts anyway.

## What setup asks for

`factory setup` is the terminal version of the get-started page, for a
server or a script. It walks you through everything else and checks each
answer before moving on:

1. **Repositories:** which repos the factory works on, one or several. It
   updates `factory.yaml`. For each repo it suggests checks and a job image
   for its stack (see [Each repo's toolchain and checks](configuration.md#each-repos-toolchain-and-checks)).
2. **Docker:** checks that Docker works for you and is new enough for
   `factory.yaml` (printing the fix if not), and offers to build the job
   image the first time. This takes a few minutes.
3. **GitHub:** creates a GitHub App for the factory. It gives you a link
   that opens GitHub with the app already filled in, with only the
   permissions the factory needs, then asks you to install it on the repo.
   It saves the app's key and finds the IDs itself, so you copy nothing by
   hand. It then confirms it can reach the repo and offers to create the
   factory's labels, including grooming's (`factory:duplicate?`,
   `factory:stale?`, `factory:split?`, `factory:split`, `factory:close`).
   On a server you reach over SSH, setup prints the `ssh -L` command that
   lets your own browser open the link.
4. **Claude:** only if a route in `factory.yaml` uses Claude: an API key, or
   a subscription token from `claude setup-token`. To run without Claude, see
   [Codex-only](configuration.md#codex-only).
5. **OpenAI:** only if `factory.yaml` uses Codex. It asks "API key or
   ChatGPT plan?" and signs Codex in with an API key, a ChatGPT plan, or a
   Business or Enterprise access token (see
   [Codex on a ChatGPT plan](#codex-on-a-chatgpt-plan)). To skip it, see
   [Claude-only review](configuration.md#claude-only-review).

[`docs/examples`](examples/) has complete configs for a GitLab workspace (a
Rails API and a Vite app) on Claude alone and on Codex alone.
6. **Phone notifications** (optional): an ntfy, Slack or Discord URL.
7. **Sentry** (optional, only with `sources.sentry` on): an auth token
   with the `project:read` and `event:read` scopes, saved as
   `SENTRY_AUTH_TOKEN`. Enter skips it.

For GitLab repos it also asks for `GITLAB_TOKEN` and checks it against each
project (see [GitLab repos](configuration.md#gitlab-repos)). Setup ends by
measuring [how ready each repo is](quality.md#how-ready-a-repo-is). One
of its tips is a `REVIEW.md` for the reviewers (see
[REVIEW.md](quality.md#reviewmd)), which only you can write.

Your answers go in `factory.env` next to `factory.yaml`, readable only by
you, and every `factory` command loads it. Run `./factory setup` again any
time to change something; it shows the saved answers as defaults.

## Codex on a ChatGPT plan

Codex can run on your ChatGPT plan (Plus, Pro, Business or Enterprise)
instead of an OpenAI API key. Its jobs then use the plan's usage limits,
not dollars. Choose it in `./factory setup`, or run:

```sh
./factory codex-login
```

This signs Codex in with a device code: it runs `codex login --device-auth`
in the job image, and you open the link it shows and enter the code. Run it
as the user the daemon runs as, so the daemon can read the file it saves. If
Codex on this machine is already signed in, it offers to copy that sign-in
instead. If you copy it, run `codex login` here again afterward, so this
machine and the factory don't share one sign-in. If Codex says device code
sign-in is off, turn it on in ChatGPT's security settings (or ask your
workspace admin).

The sign-in is saved in `data/codex/auth.json`, readable only by you, and
`factory.env` gets `FACTORY_CODEX_AUTH=chatgpt`. Any `OPENAI_API_KEY`,
`CODEX_API_KEY` or `CODEX_ACCESS_TOKEN` there is commented out, and it says
which. Restart the factory after switching. Each Codex job gets its own copy of the sign-in. Codex refreshes
it about every 8 days, and the factory saves the new one back.

Know the trade-offs before you choose it:

- **OpenAI recommends API keys for automation.** The plan works, but it's
  meant for a person at a terminal.
- **Don't use it on public repos.** Anyone can put text in front of Codex
  there, and the job holds your ChatGPT sign-in.
- **Codex runs one job at a time.** Two jobs refreshing one sign-in at once
  would break it, so the others wait in the queue. Claude jobs aren't
  affected. This holds across processes too: a `factory eval` or `factory
  compare` with Codex waits while the daemon runs a Codex job, and the
  other way round.
- **If the sign-in stops working,** for example because it was used on
  another machine, the job goes back in the queue without using a retry,
  Codex jobs wait, and you're notified once. Sign in again on the
  dashboard's get-started page, or with `./factory codex-login`; the
  running factory picks it up. A job is queued again this way
  at most twice; after that it fails as usual.

`./factory status` shows the sign-in and when it was last refreshed. It
goes by its own settings (`factory.env` and its environment), which are the
daemon's unless the daemon's environment sets other values. The
plan's 5-hour and weekly usage show there, under the dashboard's header
and on its **Spend** page, and Codex jobs wait when one is nearly used up (see
[Subscription usage limits](configuration.md#budgets)).

On ChatGPT Business or Enterprise you can use a Codex access token
instead: choose it in setup, or put `CODEX_ACCESS_TOKEN` in `factory.env`
([how to make one](https://learn.chatgpt.com/codex/enterprise/access-tokens)).
Jobs then run side by side. When the token expires, Codex jobs wait until
you put in a new one and restart the factory.

## Running it as a service

To keep the factory running across reboots:

1. Install it from a checkout in `/opt/factory`.
2. Run it as a `factory` user that can use Docker.
3. Run setup as that user. It gets its own settings and database there.

```sh
sudo useradd --system --create-home --groups docker factory
sudo git clone https://github.com/claptimes5/factory /opt/factory
sudo chown -R factory /opt/factory
cd /opt/factory
sudo -u factory env CGO_ENABLED=0 "$(command -v go)" build -o factory ./cmd/factory
sudo -u factory ./factory setup
sudo cp deploy/factory.service /etc/systemd/system/
sudo systemctl enable --now factory
journalctl -u factory -f     # follow its log
```

The service reads `/opt/factory/factory.env`. If you set up an older
version with `/etc/factory/env`, delete that file: values in it override
the ones setup saves.

## Upgrading

Build the new binary somewhere else and hand it to `deploy/upgrade.sh`:

```sh
cd /opt/factory && sudo -u factory git pull
sudo -u factory env CGO_ENABLED=0 "$(command -v go)" build -o /tmp/factory.new ./cmd/factory
sudo deploy/upgrade.sh /tmp/factory.new
```

`sudo deploy/upgrade.sh NEW_BINARY` upgrades safely:

1. It stops the daemon.
2. It backs up the database.
3. It swaps the binary, keeping the old one as `factory.old`.
4. It migrates the database.
5. It starts the daemon and watches the next job.

If the migration fails, the daemon stops, or that next job fails before
doing any work, it restores the old binary (and the database backup when
the migration failed). It uses `factory last-job`, `factory wait-job` and
`factory db-path` to do this.

## Manual setup

`factory setup` is the easy way. If you'd rather do each step yourself:

1. **Build the job images:** `./factory images`, or let `factory serve`
   build them as it starts. The default one is
   `docker build -t factory-job:latest --label io.factory.built=1 deploy/job-image`.

2. **Create a GitHub App.** The factory acts on GitHub as this app, so its
   access is limited to one repo and a few permissions you choose
   ([GitHub's guide](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app)).

   1. Open [New GitHub App](https://github.com/settings/apps/new). For a repo
      owned by an organization, use
      `https://github.com/organizations/<org>/settings/apps/new` instead.
   2. **GitHub App name:** anything unique, such as `factory-<your-login>`.
      **Homepage URL:** this repo's URL.
   3. **Webhook:** untick **Active**. The factory polls GitHub, so it needs no
      public URL.
   4. **Repository permissions:** set these and leave everything else
      at *No access*. Metadata switches to *Read-only* by itself.

      | Permission | Access | Used for |
      | --- | --- | --- |
      | Contents | Read and write | pushing `factory/*` branches |
      | Issues | Read and write | reading issues, labels and comments |
      | Pull requests | Read and write | opening and updating draft PRs |
      | Checks | Read-only | the CI panel in the dashboard |
      | Dependabot alerts | Read-only | [`sources.dependabot`](configuration.md#sources), if you turn it on |
      | Actions | Read-only | [main health](configuration.md#main-health), to tell workflows apart when two have a job of the same name |

      Leave **Workflows** at *No access*, so the factory can never edit its
      own CI. An app made without *Checks* still works, but the dashboard's
      CI panel stays empty until you add it (then accept the new permission
      on the installation page).
   5. **Where can this GitHub App be installed?** *Only on this account.*
      Click **Create GitHub App**.
   6. On the app's page, copy the **App ID** shown under *About*.
   7. Scroll to **Private keys**, click **Generate a private key**, and keep
      the downloaded `.pem` where only the daemon can read it
      ([about keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)):

      ```sh
      sudo install -D -m 600 -o "$USER" ~/Downloads/<the-downloaded-file>.pem /etc/factory/app.pem
      ```
   8. In the app's left sidebar, click **Install App**, then **Install** next
      to your account. Choose **Only select repositories**, pick the repo the
      factory works on, and click **Install**
      ([about installing](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)).
   9. You land on `https://github.com/settings/installations/<number>` (for
      an organization, `https://github.com/organizations/<org>/settings/installations/<number>`).
      That number is the **installation ID**. You can find it again later under
      [Settings → Applications](https://github.com/settings/installations) →
      *Configure*.

   With several repos, install the app on every one. Repos under different
   accounts need the app to be public (see [Several repositories](configuration.md#several-repositories)).

3. **Save the settings** in `factory.env` next to `factory.yaml`, as plain
   `NAME=value` lines with no comments after a value, and `chmod 600` it.

   ```sh
   FACTORY_GITHUB_APP_ID=123456
   FACTORY_GITHUB_INSTALLATION_ID=7890123
   FACTORY_GITHUB_APP_KEY=/etc/factory/app.pem
   ANTHROPIC_API_KEY=sk-ant-...
   OPENAI_API_KEY=sk-...
   ```

   Every `factory` command loads it, and a variable already set in the
   environment wins. The model keys reach each job container as env vars
   and are never written into a job's files. Use `-env PATH` or
   `FACTORY_ENV_FILE` to keep the file elsewhere.

   | Variable | What it's for |
   | --- | --- |
   | `FACTORY_GITHUB_APP_ID`, `FACTORY_GITHUB_INSTALLATION_ID`, `FACTORY_GITHUB_APP_KEY` | The GitHub App: its ID, the installation ID, and the path to its `.pem` key |
   | `GITHUB_TOKEN` | A personal access token instead of the app, for a quick local try |
   | `GITLAB_TOKEN` | GitLab repos (see [GitLab repos](configuration.md#gitlab-repos)) |
   | `ANTHROPIC_API_KEY`, or `CLAUDE_CODE_OAUTH_TOKEN` in its place | Claude |
   | `OPENAI_API_KEY`, or `CODEX_API_KEY` in its place | Codex |
   | `FACTORY_CODEX_AUTH=chatgpt` | Codex on a ChatGPT plan, signed in with `factory codex-login`; API keys are then ignored |
   | `CODEX_ACCESS_TOKEN` | Codex on a ChatGPT Business or Enterprise plan; wins over the two above |
   | `FACTORY_NOTIFY_URL` | Phone notifications (see [Notifications](using.md#notifications)) |
   | `FACTORY_WEB_PASSWORD` | A password for the control room (see [Control room login](security.md#control-room-login)) |
   | `SENTRY_AUTH_TOKEN` | Reading Sentry for [`sources.sentry`](configuration.md#sources) |

   Codex runs on an [OpenAI API key](https://platform.openai.com/api-keys),
   billed per use, or on a ChatGPT plan (see
   [Codex on a ChatGPT plan](#codex-on-a-chatgpt-plan)). Without either,
   every job routed to Codex fails (review jobs, in the default
   `factory.yaml`) and its item parks. To run on Claude alone, see
   [Claude-only review](configuration.md#claude-only-review); on Codex
   alone, [Codex-only](configuration.md#codex-only), and leave out the
   Claude key.

4. **Create the labels** `factory:ready`, `factory:running`, `factory:parked`,
   `factory:retry` and `factory:awaiting-you`.
   `scripts/setup-labels.sh <owner>/<repo>` does it with `gh`.

5. **Set a spend alert** on both API accounts. The daemon also enforces its
   own [budgets](configuration.md#budgets).

## Windows (WSL2)

1. Run the daemon inside a WSL2 distro with Docker available there (Docker
   Desktop's WSL integration, or Docker Engine in WSL).
2. Keep the repo and `data/` under the Linux home rather than `/mnt/c`.
3. Open `http://localhost:8080/app/` in a Windows browser: WSL2 forwards
   localhost. If that forwarding is unreliable, set
   `networkingMode=mirrored` in `.wslconfig`.

The `factory` CLI runs inside WSL. WSL counts as a Linux host for the
egress allowlist. The disk space check also watches the Windows drive under
WSL's virtual disks (see [Disk space](configuration.md#disk-space)).
