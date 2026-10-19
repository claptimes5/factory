# Security

The builder runs arbitrary code, and the issues it reads may come from
anyone. This guide covers who can start work, how jobs are kept apart from
the daemon, how untrusted issue text is handled, what jobs can reach on the network, and how to put a login on
the control room.

## Who can start work

Anyone can open an issue on a public repo, but only someone who could push
to it can start work there. The factory checks who added `factory:ready`
(or `factory:retry`, `factory:go`, or the epic label):

- **On GitHub** they need write or admin access. **On GitLab** they need
  Developer or higher. The factory's own app or bot account also counts,
  so issues the orchestrator files start on their own. Other bots and
  workflows (`github-actions[bot]`) don't: an issue's author can set off a
  workflow, so a person has to add the label.
- **Anyone else** gets the label removed and a comment saying why. So
  outsiders file issues, and you start the ones you want by labeling them.
- **What you approve is the text as you labeled it.** If the title or text
  changed after the label went on, even before the factory saw it, the
  label comes off and the factory asks you to add it again. Once work has
  started, an edit parks the item before its next stage. A reporter can't
  swap the request after you said yes. To build the new text, add
  `factory:retry`.
- **The orchestrator reads only approved text.** It sees an issue's title
  and body only if that issue was approved and is unchanged since.
- **Issues the factory files from Sentry, Dependabot or TODOs wait for
  you.** They never carry `factory:ready` and are never approved as the
  factory, so the same check applies to them as to an outsider's issue
  (see [Sources](#sources-tokens-and-untrusted-text)).
- **Grooming acts only on a maintainer's label.** `factory:close` and
  `factory:split` are checked the same way as `factory:ready`, and the
  sub-issues a split opens wait for `factory:ready` too (see
  [Grooming](#grooming-untrusted-issue-text)).

Approving a [cost estimate](using.md#cost-estimates) from the dashboard
needs the control room's password, or a browser on the factory's own
machine, as changing settings does: it lets the factory spend.

Items that were already in the factory when you upgrade take their text,
the next time it's read, as approved.

## Isolation

Nothing the builder writes is trusted:

- **It works in its own clone.** Afterwards the daemon reads that clone's
  files (never its `.git`) into a daemon-owned repository and commits them
  there. Hooks, fsmonitor commands, remotes or refs a builder plants have
  no effect on the daemon's git.
- **Checkouts are fresh per job** on the host, under `data/jobs/<id>`,
  mounted into the container and deleted when the job ends.
- **The daemon pushes, not the builder,** and pushes exactly the commit the
  gates verified. The token reaches git through `GIT_ASKPASS` for that one
  command; it is never on a command line, on disk, or in a container.
- **Gates run on an export** of that commit, a copy apart from the daemon's
  repository, so code under test cannot change what gets pushed. Checks
  may add files there (dependencies, build output). But a check that edits
  or deletes the commit's own files fails, so it can't get around the diff
  guard by rewriting a lint config or removing a test at run time.
- **The container is the builder's sandbox.** Claude builds with
  `bypassPermissions` and Codex with `--sandbox danger-full-access`: inside
  the job's container, with the egress allowlist, either may run any
  command. Read-only steps keep each CLI's own read-only mode.
- **Reviewers only count their final message.** A verdict file anywhere is
  ignored, and Claude's read-only runs ignore the checkout's settings,
  hooks and MCP servers.
- **The diff guard protects files that steer agents** (`.claude/`,
  `CLAUDE.md`, `AGENTS.md`, `AGENTS.override.md`, `.codex/`, `.mcp.json`).
  The list is `gates.diff_guard.protected_paths`; this repo's also covers
  the factory's own controls (`.github/`, `factory.yaml`, `prompts/`,
  `skills/`, `.golangci.yml`, `CODEOWNERS`). For every repo it adds
  `REVIEW.md` and `.github/REVIEW.md`, whatever the list says, since they
  steer the reviewers (and the review reads them from the base branch, not
  the change). For every GitLab repo it adds `.gitlab-ci.yml` and
  `.gitlab/`.
- **The review is pinned to the verified commit.** If the branch moved
  after the gates ran, the item parks.
- **The PR description quotes the checks, never the reverse.** By default
  (`pr.check_output: private`) the end of each passing check's output goes
  only on PRs in repos the forge reports as private, and in a workspace
  only on its own repo's PR. It's redacted whole before it's cut: known
  secret values (model keys, `env`, services' `env`, each line of a
  multi-line value, the values in the repo's `files`, read through links
  as they're copied), token shapes, and any line that sets a variable or
  names a token, secret, password or key. If the repo's `files` can't all
  be read, nothing is quoted. It sits in a code block it can't close. The
  repo's PR template is read from the base branch as layout only: just its
  headings are kept, cleaned, and no part of it reaches a prompt. See
  [The PR description](using.md#the-pr-description).
- **The model keys** reach each job container as env vars and are never
  written into a job's files. Agents can read `env`, so it is not for
  secrets. Claude jobs get `ANTHROPIC_API_KEY` or
  `CLAUDE_CODE_OAUTH_TOKEN`. Codex jobs get `OPENAI_API_KEY` and
  `CODEX_API_KEY`, or on a ChatGPT plan no API key: `CODEX_ACCESS_TOKEN`,
  or a copy of the plan's sign-in.
- **The ChatGPT sign-in** (`data/codex/auth.json`, mode 0600) is copied
  into each Codex job's own `CODEX_HOME`, under the job's output
  directory, never the checkout, so a builder can't commit it. When the
  run ends, the daemon saves back a copy Codex refreshed and deletes the
  job's copy. If the daemon stops mid-run, the copy stays on disk under
  `data/jobs/` until the daemon starts again, which saves it back the same
  way and deletes it. A refreshed copy is saved only if it's a sign-in to
  the same account and the factory's copy hasn't changed since the job
  started. The daemon reads only small regular files from that directory,
  never through a link, so a job can't point it at another file. The
  sign-in's tokens are taken out of the job's log. The agent in that job
  can read the sign-in, as it can read an API key, so don't use a ChatGPT
  plan on public repos.

## Sources: tokens and untrusted text

The [sources](using.md#issues-from-sentry-dependabot-and-todos) read text
other people control into issues the builder may later read:

- **Error messages are attacker-influenced.** Anyone who can make your app
  throw can choose what a Sentry error says, and so can a crafted request
  for its stack frames' file names. Advisory summaries and the lines around
  a TODO come from elsewhere too. The factory puts all of it in a fenced
  block, marked as data and not instructions, never in a heading or a
  link. Titles are cut to one line with no backticks or invisible
  characters, and `#12` and `@name` in them are spaced out, so a title
  can't mention anyone or close an issue once built. Notifications leave
  the title out, and no notification pings: Slack's `<!channel>` and links
  are escaped, and Discord resolves no mentions. Nothing it files is built
  until someone with write access reads it and adds `factory:ready`.
- **Only its own issues.** The factory takes an issue as one it filed
  only when its own app or account opened it, and closes it only while
  its text still reads as filed. An issue template or someone who can
  label can't plant one for it to adopt and close, and an issue someone
  edited is left open.
- **The Sentry token** (`SENTRY_AUTH_TOKEN` in `factory.env`) is read as
  soon as `factory.env` is loaded, by every command, then taken out of the
  process's environment, so nothing it runs inherits it: not git, not
  Docker, not a job or an eval, even on the local runner. Its
  `base_url` must be `https` (or `http` on this machine). It's sent only to `sources.sentry.base_url`, never logged, and
  never in an issue or a notification. Give it read scopes only
  (`project:read`, `event:read`).
- **Dependabot alerts** are read with the factory's GitHub credentials; the
  app's permission for them is read-only.
- **The TODO scan** is a shallow clone the daemon owns, read with
  `git grep` and `git show` and deleted after. Nothing in it runs.

## Auto-merge's guard rails

The [guard rails](using.md#auto-merge-guard-rails) read and write a few
more things:

- **Commit messages and PR text are only matched against.** Anyone who can
  push to the base branch, or open a PR that gets merged, writes the text
  the revert watch reads. It looks only for a reference to one of the
  factory's own merged PRs (its number, merge commit or title) and never
  quotes the text anywhere: notifications, `factory status` and the
  dashboard name the reverting commit by its SHA or the PR by its number,
  and the factory PR by its own title, cut to one line without control or
  invisible characters. The worst a planted "This reverts commit ..." can
  do is count a strike, which only turns auto-merge off. A bare title, or
  a `revert-N-` branch name, counts for less: a title only when one
  standing factory PR has it, a branch only when its PR also says it
  reverts something.
- **Bringing a PR up to date** is a merge the daemon makes itself, in a
  clone of its own with hooks and other config that runs programs turned
  off, as every git command it runs is. The clone is partial (no file
  contents until the merge needs them), so every git command in it may
  fetch from the remote and gets the token the same way clone and push
  do: through GIT_ASKPASS in that command's environment, never on a
  command line or on disk. It merges the base branch's commit
  into the verified commit, pushes exactly that commit, and refuses if the
  branch has moved. The merge commit adds only what's already on the base
  branch, and CI must pass on it before auto-merge is turned on for it.
  Anyone else's push to the branch while it waits hands the PR back to you.
- **Resetting the breaker** lets PRs merge without you again, so from the
  dashboard it needs the control room's password, or a request from the
  factory's own machine, as saving settings does. `factory automerge reset`
  runs on the host, with the database's access.

## Grooming: untrusted issue text

[Backlog grooming](using.md#backlog-grooming) puts the text of open
issues, which anyone can write on a public repo, into a model's prompt:

- **It's fenced and marked as data.** Titles, labels and bodies (cut to
  1500 characters) go inside a code fence none of them can close, with
  the prompt saying to ignore any instructions in them. The job is
  read-only, runs in an empty folder with no checkout and none of the
  repo's `env`, and its only output is its JSON answer.
- **Its answer can't act.** The daemon checks every issue number against
  what the job was shown and may flag, so a planted "close #1" can only
  propose closing an issue a human then has to confirm. What it posts is
  checked first: no links, @mentions, HTML, GitLab references or quick
  actions (a line starting with `/`), and `#N` only for issues it was
  shown. The model's words are quoted in the comment, below the factory's.
- **Nothing happens without write access.** The factory only comments and
  adds `?` labels. Closing and splitting wait for `factory:close` or
  `factory:split` from someone with write access, checked like an
  approval, and sub-issues wait for `factory:ready`. A confirmed close is
  refused if the issue changed since it was flagged, and a duplicate is
  never closed when the issue to keep is closed or proposed closed too.
- **Its marker can't be forged:** only the factory's own comments count
  when it looks for the marker that says an issue was flagged already.
- **It leaves the factory's own work alone:** issues being built, with an
  open PR, or with a factory label are never flagged.

## Previews: running the change's app

A [preview](using.md#screenshots-on-the-pr) runs the change's own code, so
it's held to the gates' rules:

- **It runs only after the gates pass**, on the same export, in the same
  job image and container limits, with the repo's `env` and services and
  the same egress allowlist. No model key, forge token or `factory.env`
  secret reaches it, so the app can't push or act on the repo.
- **The daemon uploads, not the app.** The forge token stays in the
  daemon, which posts the comment and commits the images itself.
- **Only the files the factory named come back**, and only as regular
  files (no links out of the export) that decode in full as PNGs (a
  truncated or corrupt one is refused) of at most 4000×4000 pixels and
  5 MB, at most `previews.max_screenshots` of them.
  The app shares the container and could write anything there; anything
  else is ignored.
- **Pages the builder names are model output**, so only plain paths are
  kept (one leading `/`, no spaces, quotes, backticks or angle brackets),
  and they're always opened on the app's own `127.0.0.1` address.
- **The app's log stays off the PR.** The app runs with the repo's
  `files`, so its log can quote a secret. The log and the browser's error
  messages go to the job's folder and the item's page in the dashboard;
  the PR comment says what went wrong in the factory's own words only.
- **Images are attacker-influenced.** A PR can make its pages show
  anything, text included. The reviewer is told the screenshots show what
  the change looks like, not that it works, and that text in them is page
  content, never instructions.
- **On GitHub, the images live on `factory/previews`** in the repo
  itself, so anyone who can read the repo can read them, as with the PR.
  A removed image stays reachable by its commit's link until GitHub's
  garbage collection.
  It has no workflow files, so it runs no CI. The factory checks that
  the branch is its own (one parentless "factory: PR screenshots"
  commit) and then force-updates it, but the two steps aren't atomic:
  GitHub has no compare-and-swap on refs, so a push that lands between
  them is overwritten. Only the factory should write to the branch. On
  GitLab they're project
  uploads, with GitLab's own access rules; removing them takes the
  Maintainer role and GitLab 17.2.
- **The dashboard serves only the images a build's manifest lists**, as
  `image/png` with `nosniff` and a `default-src 'none'` policy.

## Network: the egress allowlist

With the docker runner, jobs can reach only the hosts on an allowlist. A
builder that reads a prompt injection in an issue or a dependency can then
send what it holds only to those hosts. This narrows the ways out; it
doesn't close them. An injection that brings its own credentials could
still push to the attacker's GitHub repo or publish a package. Keep
secrets the jobs don't need out of `env`.

- **Jobs sit on a network with no way out** (`factory-egress`, internal,
  with its gateway off the host). They can't resolve outside names, reach
  an address, or reach the host's ports. The factory refuses to use an
  existing network of that name that isn't set up this way.
- **Their HTTP(S) goes through the egress proxy** (`factory-egress-proxy-…`).
  That proxy is this binary, run from an empty image in a locked-down
  container on that network and on the usual one.
- **The proxy forwards only to listed host names,** on ports 443 and 80.
  It never forwards to IP addresses, or to names that resolve to private,
  loopback or link-local addresses. That rules out the cloud metadata
  service and your LAN.
- **A TLS connection on port 443 must name (in its SNI) the host it asked
  for.** That stops a client from naming a different site there. It
  doesn't stop a client from sending another site's `Host` header inside
  the encrypted stream (domain fronting), or hiding the name with
  Encrypted Client Hello. So an allowed CDN name can still reach other
  sites behind that CDN. Other ports you allow (`host:8443`) get no SNI
  check.
- **The built-in list** covers the Claude and OpenAI APIs and their
  sign-in, GitHub, gitlab.com, and the Go, npm, PyPI, RubyGems, crates.io
  and Maven registries. Each repo's own self-hosted GitLab is added for
  you.
- **Tests and hidden scenarios that run with no network** still get none.
- **Jobs that run at the same time share the network,** so they can reach
  each other's open ports, as they could on Docker's default network.

To change the list:

```yaml
runner:
  egress:
    allow: [registry.example.com, "*.cdn.example.com", "git.example.com:8443"]
    private: [mirror.corp.example]   # on your LAN
    # no_defaults: true              # only the hosts above
    # mode: open                     # Docker's usual network: the whole internet
```

Changing the list starts a new proxy before the next job.

**How jobs find the proxy.** Jobs get the proxy in `HTTPS_PROXY` and the
other usual variables, which git, Go, npm, pip, cargo, Claude Code and
Codex all honor. They also get it in `JAVA_TOOL_OPTIONS` for the JVM.
Maven may still need a proxy in its `settings.xml`.

**When a job is refused.** A job that hits a host that isn't listed gets a
403 that names the setting to change, and `docker logs` on the proxy lists
every refusal.

**Requirements.** The allowlist needs Docker 28 or later on a Linux host
(WSL counts). On anything else, the factory won't start until you set
`mode: open`.

## Control room login

`factory serve` serves the control room and dashboard on `web.listen`
(default `127.0.0.1:8080`).

**Add a password** by setting `FACTORY_WEB_PASSWORD` in `factory.env`.

- It must be at least 12 characters.
- Sign-in lasts 30 days, and changing the password signs everyone out.
- A wrong password is limited to five tries a minute per address.

**A password is required** whenever `web.listen` reaches beyond this
machine and a tailnet, as `0.0.0.0` or a LAN address does. Without one,
the factory won't start.

**Changing settings from the dashboard** (the Settings screen, see
[Settings](using.md#settings)) needs the password, or `web.listen` on a
loopback address and a page opened as `localhost`. Through a tailnet
address or a proxy such as `tailscale serve` without a password, the
screen is read-only: someone who can reach your tailnet can look, but not change
budgets or models, or restart the factory. Nothing that runs commands or
opens the network (gates, repos, the runner, egress) can be changed there.

**Use HTTPS on a public address.** The control room serves plain HTTP, so
put it behind an HTTPS proxy (Caddy, or `tailscale serve`); the session
cookie is then marked secure. There, Attach gives you the `docker exec`
command rather than starting a terminal server, since that server has no
login. The terminal inside the page still works.

**Other host names.** The control room only answers to localhost names and
the host in `web.listen`. To reach it under another name (a Tailscale
name, say), list it in `web.hosts`.

**The terminal** is a plain `sh` in the job's container, through
`docker exec` on the host. It connects only when you press Connect, and a
shell left behind ends with the job's container.

The dashboard has no GitHub login of yours, so Approve opens the PR on
GitHub, and the factory never approves its own PRs.
