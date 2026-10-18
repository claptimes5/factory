# Security

The builder runs arbitrary code, and the issues it reads may come from
anyone. This guide covers who can start work, how jobs are kept apart from
the daemon, what jobs can reach on the network, and how to put a login on
the control room.

## Who can start work

Anyone can open an issue on a public repo, but only someone who could push
to it can start work there. The factory checks who added `factory:ready`
(or `factory:retry`, or the epic label):

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
- **Reviewers only count their final message.** A verdict file anywhere is
  ignored, and Claude's read-only runs ignore the checkout's settings,
  hooks and MCP servers.
- **The diff guard protects files that steer agents** (`.claude/`,
  `CLAUDE.md`, `AGENTS.md`, `AGENTS.override.md`, `.codex/`, `.mcp.json`).
  The list is `gates.diff_guard.protected_paths`; this repo's also covers
  the factory's own controls (`.github/`, `factory.yaml`, `prompts/`,
  `skills/`, `.golangci.yml`, `CODEOWNERS`). For every GitLab repo it adds
  `.gitlab-ci.yml` and `.gitlab/`.
- **The review is pinned to the verified commit.** If the branch moved
  after the gates ran, the item parks.
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
