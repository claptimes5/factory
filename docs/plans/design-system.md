# Plan: Factory's design system and the new home

Status: agreed direction, not started. Mockups:
[Factory design directions](https://claude.ai/artifact/Huz7XkuZKRMnwtdC2ZyYKV),
round 4 ("All together, with trust and quiet hours").

## What we're making

The dashboard grew a screen at a time, and it shows: three shared
components, inputs and alerts built by hand on each screen, seven one-off
font sizes, and banners that stack above every page. The goal is one
calm, minimal product that is as good on a phone as on a desk.

Principles, in order:

1. **What needs you comes first, one thing at a time.** The factory's job
   is to need people less. When it does need you, that is the first thing
   on the screen, with the buttons to settle it.
2. **Everything else is quiet.** Work that is moving fine is a glance (the
   line), not a feed. Updates are batched into a catch-up, never a stream
   you fall behind on.
3. **Ask, don't hunt.** A chat with the factory is on every screen, with
   suggestions for the common requests.
4. **Trust is visible and earned.** The factory shows how much it may do on
   its own, the evidence for more, and steps back by itself when it slips.
5. **Phone first.** Every screen is designed at 390 px wide first.

## Visual language

| Token | Light | Use |
|---|---|---|
| `background` | `#F7F7F5` | Page |
| `surface` | `#FFFFFF` | Cards, panels, inputs |
| `ink` | `#17171A` | Text; the decision card's background |
| `muted` | `#55555D` | Secondary text (passes 4.5:1 on both grounds) |
| `border` | `#E5E5E0` | Hairlines, card edges |
| `accent` | `#1D4ED8` | Work moving, links, the current trust level |
| `attention` | `#C2410C` text, `#FDBA74` on ink | Needs you |
| `success` | `#15803D` on `#ECFDF3` | Went live, passed |
| `danger` | `#B91C1C` | Broken, failed |

Dark mode gets the same roles with its own values, chosen in phase 0 and
checked for contrast.

- **Type:** Geist for everything, Geist Mono for repo names, numbers,
  times and money (`@fontsource-variable/geist`, `@fontsource-variable/geist-mono`,
  self-hosted like today's fonts). One scale: 12, 13, 14, 15, 18, 22, 28,
  named `caption`, `meta`, `body-sm`, `body`, `title-sm`, `title`,
  `display`. No other sizes.
- **Shape:** radius 12 for controls, 18 for cards, full for pills and
  chips. Spacing on a 4 px grid. Hairline borders, no drop shadows except
  the decision stack.
- **Touch:** every control at least 44 px tall on a phone.
- **Icons:** lucide, 2 px stroke, 16 or 20 px.

## Components

Built on shadcn/ui (Radix primitives, copied into `web/src/components/ui`,
styled with the tokens). Primitives: Button, Input, Textarea, Field
(label, help and error together), Select, Checkbox, Switch, RadioGroup,
SegmentedControl, Accordion, Dialog, Sheet (a bottom sheet on phones),
Tooltip, Toast, Alert, Progress, Tabs, Badge.

Factory's own patterns, on top of those:

| Pattern | What it is |
|---|---|
| `DecisionStack` | What needs you, one card at a time: what it is, why it needs you, and its actions; the rest listed as "Next" and "Then" |
| `LineStrip` / `Line` | The stations (Plan, Code, Test, Review, You) with the jobs at each; the strip on Home, the full view as its own screen |
| `JobRow` | A job in a list: title, repo, station, time |
| `StationStamps` | A job's history as stamps, in its detail view |
| `CatchUp` | "Since you looked": what went live, what needs you, what's still going |
| `AskPanel` | The chat with the factory, its suggestion chips and composer; a side panel on desktop, the foot of Home on a phone |
| `TrustChip` / `TrustLevels` | The current level; the ladder with its evidence |
| `NoticeArea` | One place for warnings that apply everywhere (main is red, auto-merge paused, setup unfinished), replacing the stacked banners |
| `Field`, `SecretField`, `CopyCommand`, `EmptyState`, `StatusDot`, `PageHeader` | Shared forms and page parts |

A style guide page in the app (`/app/design`, dev builds only) shows every
component in light, dark and phone widths. A lint rule refuses arbitrary
Tailwind values (`text-[13px]`, `rounded-[..]`) outside `components/ui`.

## Screens

| Phone tabs | Desktop sidebar | Replaces |
|---|---|---|
| Home | Home | Floor's top, Inbox |
| Line | The line | Floor (Board), Live |
| — | History | Done items, the activity feed |
| — | Trust | Auto-merge settings and reset, scattered today |
| — | Spend | Spend (Metrics) |
| Settings | Settings | Settings, Get started, Setup (readiness) |

On a phone, Ask is the foot of Home and a sheet from any screen. On
desktop it is a panel on the right of every screen. The orchestrator's
chat becomes Ask; its own decisions move to History.

## What the backend needs

Most of it exists. New work, by phase:

- **Decisions** (phase 2): one list of what needs you, in order: PRs
  awaiting review, plans waiting for `factory:go`, estimates over
  `confirm_over`, questions (`needs_input`), and setup blockers. Today
  they come from `/board`, `/notifications` and item state separately.
- **Since you looked** (phase 2): when each viewer last saw Home, kept by
  the server, so the catch-up is the same on the phone and the desk.
- **Trust** (phase 4): levels for each repo as names for the existing
  `auto_merge` settings (`up_to`, `max_size`), the breaker as "steps back",
  `risk.needs_person` as "always asks". Evidence from what `/metrics`
  already counts per repo (merged, reverted, broke main, first-pass rate)
  plus your edits before merging (learning already reads them). The
  factory suggests a level up; only a person raises it. Later, **practice
  runs**: what would have merged at the next level over the last 30 days,
  and how each turned out.
- **When to tell you** (phase 5): kinds of event and when each is sent
  (right away, in the catch-up, always), quiet hours, and where (this
  device, ntfy, Slack, Discord). Held messages go out as one catch-up.
- **Installed web app** (phase 5): a web app manifest, a service worker,
  Web Push (VAPID keys in `factory.env`), and the badge count. iPhones get
  push for web apps added to the home screen (iOS 16.4+), without action
  buttons: a tap opens the decision. Android shows the buttons.

## Order of work

Each phase ships on its own, as one or more pull requests, with
Playwright tests at desktop and phone widths and screenshots in the PR.

0. **Foundations.** Tokens (light and dark), fonts, the type scale,
   shadcn primitives, the style guide page, the lint rule. No screen
   changes yet.
1. **The frame.** Sidebar and phone tabs, `NoticeArea` in place of the
   banners, `AskPanel` on the existing chats API. Every screen gets the new
   frame with its old content.
2. **Home.** `DecisionStack`, `LineStrip`, `CatchUp`, Ask, with the
   decisions list and last-seen on the server. Home becomes `/`; the old
   Floor moves to `/line`.
3. **The rest, one screen per PR.** The line, item and job detail (with
   `StationStamps`, its terminal and diff), History, Spend, Settings, Get
   started and Setup.
4. **Trust.** The levels screen over the existing settings, evidence,
   suggestions on Home; then practice runs and the trust history.
5. **Notifications.** The installed web app, push, when-to-tell-you
   settings, quiet hours, the held catch-up.

## How we got here

The design was worked out in four rounds of mockups on the canvas linked
at the top (private to its owner until shared from its Share menu). What
was decided, and why, so the work can go on without that conversation:

1. **Three looks for the old Floor** (Quiet: cool neutrals and indigo;
   Workshop: warm paper, hairline lists, signal orange; Night shift: dark,
   mint, big touch cards). Liked the clean parts of each; all felt too
   generic. Decided: calm and minimal, not dense; the phone matters as much
   as the desk, since once set up you mostly check in from your phone,
   as with Claude and Claude Code; free to pick a new brand.
2. **Four ideas for Home.** *The line* (each job at its station, you as
   the last station): liked seeing what's in progress and, above all,
   exactly what's needed from you; otherwise a little dull. *The
   briefing* (a written note): refreshing, but too much to read; its chat
   box at the bottom should be on Home. *The thread* (the factory as a
   chat): liked the simple conversation, the suggestion chips and the
   "needs you" card, but a feed gets too long to keep up with. *The
   pulse* (a ring chart): least liked; big, and you still have to read
   the list below to understand it.
3. **What you need, at the moment you look.** Stamped job cards (worth
   keeping, as a job's detail). *Moments*: the screen changes with why
   you're looking: something needs you, all humming, catching up after
   time away. Catching up groups updates into "went live / needs your OK /
   still going", which answers the thread's overload: only your own
   conversation is a chat. A lock-screen view (see below). A combined
   Home.
4. **All together.** One decision at a time, the line strip, the catch-up
   line and Ask on Home; Trust; when to tell you, with quiet hours; Home
   on desktop with Ask as a side panel. Agreed: "this feels right".

Two points from the discussion that shape the backend:

- **Trust is central.** The aim of any factory is to need people less
  over time: trust should rise as the record and the evaluations improve,
  be visible, and drop on its own when something goes wrong. The levels,
  evidence, suggestions and practice runs above came from this; they are
  their own feature plan beyond the screens.
- **No native app yet.** A lock-screen widget needs one, so it's out.
  Notifications come from the installed web app (or ntfy, Slack,
  Discord) and open the decision they're about.

The first fix to the get-started page, which led to this, is a separate
branch: `claude/get-started-ux` ("Get started asks which repos, and every
step explains itself"). Its PR may still need opening.

## Open questions

- The mark: a new logo to go with the new look.
- Dark mode values (phase 0) and whether a phone follows the system.
- Whether three trust levels are enough, or medium-risk splits by size.
- "The line" or "Floor" as the name of the full view.
