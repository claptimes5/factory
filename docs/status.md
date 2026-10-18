# Status

## What's built

These milestones from [SPEC.md](../SPEC.md) are in:

| Milestone | What it added |
| --- | --- |
| M1 | Store, claim, config |
| M2 | Runner and adapters |
| M3 | The pipeline |
| M4 | Control room and notifications |
| M5 | The orchestrator |
| M6 | Hidden scenarios, metrics, `factory eval`, session resume, safe upgrade |
| M7 | Dashboard v2 |

Side-by-side skill comparison, services, workspaces across repos,
[keeping main green](using.md#keeping-main-green),
[issues from Sentry, Dependabot and TODOs](using.md#issues-from-sentry-dependabot-and-todos),
[screenshots on the PR](using.md#screenshots-on-the-pr),
[cost estimates](using.md#cost-estimates),
[backlog grooming](using.md#backlog-grooming) and
[auto-merge's guard rails](using.md#auto-merge-guard-rails) are in too.

## Where this differs from SPEC.md

- **The daemon pushes, not the builder** (see [Isolation](security.md#isolation)).
- **Checkouts are fresh per job on the host** (under `data/jobs/<id>`),
  mounted into the container and deleted when the job ends.
- **Auto-merge is opt-in:** SPEC.md keeps every merge human. With
  [`auto_merge`](configuration.md#auto-merge) set, small low-risk PRs merge
  themselves once CI passes against the current base branch, and a
  breaker turns it off for a repo whose factory PRs get reverted or break
  main too often.
- **Unparking is explicit:** a `factory:retry` label, not a removed label.
- **A malformed review retries once, then parks**, so a broken reviewer
  never consumes a fix round or passes code.
- **On restart, live job containers are killed and requeued** without using
  a retry. Reattaching to them comes with container reattach (M2 recovery).
  Spend from a job killed mid-run is not recorded (the CLIs report cost only
  at the end), so the daily cap can undercount after a crash.
- **Test-proves-fix counts a compile failure as a failing test.** A new test
  that calls a function the change adds fails to build on the old code,
  which is the expected TDD shape.
- **Stuck items are reconciled:** an item in an active stage with no job for
  two minutes resumes from its last job's outcome.
- **No separate spec stage yet.** Triage parks unclear issues with a question.
- **Reviewer value is measured as review escapes** (passed PRs you changed or
  closed), since blocked versions never reach you and so can't be judged,
  and as **reviewer precision**: the share of its blocking findings that
  a verifier didn't refute and the builder fixed (see
  [Review findings](quality.md#review-findings)).
- **The orchestrator is off by default** in code (on in this repo's
  `factory.yaml`), since it spends on its own schedule.
- **Approve opens the PR on GitHub.** The dashboard has no GitHub login of
  yours, and the factory never approves its own PRs.
- **Browser notifications need an open tab.** There is no Web Push service;
  your notify channel (ntfy, Slack, Discord) covers a closed browser.
- **The terminal is a plain `sh` in the container**, through `docker exec`
  on the host. It connects only when you press Connect, and a shell left
  behind ends with the job's container.
- **A drained queue only triggers the orchestrator for stuck work**, once per
  distinct set of stuck items, so items waiting on your PR review don't cost
  a run every half hour.
