# Plan: workspaces, the factory's work scoped to a project

Status: building. The owner chose the four decisions in "Decided" below,
and the recommendation for each question that was open (see "Settled
later").

## Where things stand

`workspaces:` in factory.yaml means one thing today: repos that change
together. An issue in any member becomes one change across all of them,
with one branch, a PR in each repo and one review
([configuration.md](../configuration.md#workspaces-one-change-across-repos)).
It has no screen, and it scopes nothing else.

Everything else is either per repo or factory-wide:

| | Today |
|---|---|
| Dashboard | A repo filter in the sidebar, "All repos" or one, kept per browser |
| Orchestrator | One for the factory. It sees every repo, and `create_issue` without a `repo` goes to the parent epic's repo, else **the first repo in factory.yaml** (`pickRepo`, orchestrator.go) |
| Ask (chat) | Sees every repo; an issue drafted without a repo goes to the first one |
| Trust | Per repo: each has its own `auto_merge` and `self_start`, and its own record |
| Budgets | One daily cap for builds, one for the orchestrator |
| Queue, credentials, job images, models | Factory-wide |

So with an open-source repo and a private one on one factory, nothing
stops the orchestrator or Ask filing an issue about one in the other, and
the dashboard shows both mixed unless you filter by hand.

## What a workspace becomes

A workspace is a project: the repos that belong together, and the unit
the factory scopes its work to. Every repo is in exactly one workspace. A
workspace can hold one repo or several.

- **Seeing:** the dashboard shows one workspace at a time.
- **Deciding:** each workspace has its own orchestrator, which sees and
  acts on its repos only.
- **Asking:** Ask works in the workspace on screen, and can only file
  issues in its repos.
- **Trusting:** a workspace has one trust level, earned from the record
  of all its repos.
- **Spending:** under the factory's daily cap, a workspace can have a
  smaller cap of its own.

Whether a workspace's repos change as one, today's meaning, becomes a
setting inside it: `change_together`.

## Decided

1. **The name.** "Workspace" is the scoping group. Today's behavior is
   its `change_together: true`, off by default.
2. **The orchestrator is per workspace.** Each workspace's orchestrator
   runs on its own schedule and budget, sees only its repos, and can only
   create issues there. In a workspace of several repos, `create_issue`
   must name the repo: nothing defaults to "the first one".
3. **Trust is per workspace.** One level for the workspace, applied to
   all its repos, earned from the workspace's record. A repo can be set
   lower, never higher.
4. **Budgets.** The factory's daily cap stays. A workspace may set a
   smaller daily cap of its own, so one project can't spend the day.

## Config

```yaml
workspaces:
  factory:
    repos: [claptimes5/factory]
  shop:
    repos: [you/api, you/web]       # in the order their PRs merge
    change_together: true           # one change across both, as today
    daily_total: 30                 # dollars; under budgets.daily_total
    orchestrator: {enabled: true, interval: 60m, daily_budget: 5}
    auto_merge: {up_to: low, max_size: S}   # the workspace's trust level
```

- `repos:` still lists every repo and its settings (gates, image,
  services). A workspace names repos; it doesn't hold their settings.
- **The old form still loads.** `shop: [you/api, you/web]` reads as
  `{repos: [you/api, you/web], change_together: true}`, so today's
  workspaces behave exactly as before.
- **A repo in no workspace** gets a workspace of its own, named for it.
  With no `workspaces:` at all, each repo is its own workspace. For a
  factory with one repo nothing changes. For one with several, each
  repo's orchestrator, trust and dashboard view are then separate (see
  "Still open", 1).
- **The orchestrator block at the top level** holds the defaults every
  workspace's orchestrator takes. Its `daily_budget` stays a cap on all of
  them together.
- **Trust:** a workspace's `auto_merge` and `self_start` are its level. A
  repo's own `auto_merge` may only be stricter. Today's per-repo settings
  become the workspace's when the workspace has one repo. With several
  repos of different levels, the workspace takes the lowest of them, and
  the Trust screen says so.
- **Checks at load:** every workspace has at least one repo, and no repo
  is in two. A repo listed in a workspace but not in `repos:` is an error.

## Scoping, area by area

| Area | Scoped to the workspace |
|---|---|
| Dashboard | A workspace switcher at the top of the sidebar (and the phone's top bar) replaces the repo filter. Home, The line, History, Spend, Setup, Trust and the Inbox show the chosen workspace. Within a workspace of several repos, the repo filter stays as a second level. The choice is kept per browser and in the URL (`?ws=factory`), so a link opens the right one. |
| Orchestrator | One per workspace. Its prompt names only its repos; its read tools return only its items; `create_issue`, `set_priority` and the rest refuse an item or repo outside it. It runs on the workspace's interval, one at a time across the factory as now, and its spend counts against the workspace's orchestrator budget and the factory's. The daily summary is one per workspace. |
| Ask | Works in the workspace on screen: its prompt and checkouts are that workspace's repos, its tools are fenced like the orchestrator's, and the Create issue buttons only offer those repos. A conversation keeps the workspace it started in. |
| Trust | The Trust screen shows each workspace's level, record and suggestion. Raising or lowering it writes the workspace's `auto_merge` and `self_start`. The auto-merge breaker trips per workspace. |
| Self-started work | Main health, sources, the hunt and grooming already run per repo. What they file is self-started at the workspace's level, within a per-workspace daily cap. |
| Budgets | Builds and reviews count against the workspace of their item's repo. With the workspace's `daily_total` spent, its queued jobs wait until tomorrow; other workspaces go on. The factory cap still stops everything. |
| Notifications | Each one names its workspace when there's more than one. Quiet hours stay factory-wide. |
| Get started | The Repositories step groups repos by workspace. Adding a repo asks which workspace it joins, or makes a new one named for it. A workspace can be renamed, and a repo moved, there or in Settings. |

**Not scoped:** the queue and how many jobs run at once, credentials, job
images, models and routing, the machine checks. These are the machine's,
not a project's. Routing per workspace can come later if it's wanted.

## Attributing work to a workspace

Most of the factory already knows a job's repo through its item, and so
its workspace. The rest needs a workspace of its own:

- **Orchestrator and chat jobs** have no item. Their input records the
  workspace they ran for, and spend, the feed and History read it there.
- **Events with no item** (a repo's readiness check, main health) carry
  their repo already; their workspace follows from it.
- **Moving a repo between workspaces** moves its history with it: nothing
  stores the workspace on an item, so the record follows the config. An
  orchestrator or chat job keeps the workspace it ran for.

## Phases

0. **Safety, at once.** With more than one repo configured, the
   orchestrator's `create_issue` and Ask's drafts must name the repo, and
   are refused without one. Nothing else changes. Small, and worth
   shipping before the rest.
1. **The model.** Config parsing and checks, the old form, a workspace
   for every repo, `Config.WorkspaceOf(repo)`, and `change_together` in
   place of today's membership checks (`inWorkspace` and `members` in the
   pipeline). Docs. No behavior changes for anyone yet.
2. **The orchestrator per workspace.** Scheduling, budgets, prompts,
   fenced tools, the summary, and the job input that records the
   workspace.
3. **Trust per workspace.** The level, the record, the breaker, the Trust
   screen, the stricter-repo rule, and self-start caps.
4. **The dashboard.** The switcher, scoping each screen's API by
   `?workspace=`, Ask in its workspace, notifications naming it.
5. **Get started and Settings.** Grouping repos, choosing a workspace
   when adding a repo, renaming and moving.
6. **Budgets per workspace.** `daily_total` per workspace and the queue
   holding a spent workspace's jobs.

Each phase ships on its own, with its tests and docs, in this order:
phases 2 to 6 each build on phase 1, and phase 4 is where most of the
dashboard work is.

## Settled later

1. **A factory with several repos and no `workspaces:`.** Each repo
   becomes its own workspace: the safest default.
2. **Nothing hides in another workspace.** Built as a line on Home and the
   Inbox, "In shop, 2 things need you. Switch to it", rather than an "All
   workspaces" view: every screen, Ask included, always shows one
   workspace. The factory-wide notices (the disk, setup) show everywhere;
   a repo's (its main, its breaker) in its workspace. Spend stays the
   factory's, with the workspace's own budget in the sidebar.
3. **Who may add or change a workspace from the browser:** the same rule
   as saving settings, a password or the factory's own machine.
