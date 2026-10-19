import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

/** Nothing on the page may be wider than the viewport (no sideways scroll on a phone). */
async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

test("the floor puts what needs you first, with live work and repo chips", async ({ page }, info) => {
  await page.goto("/app/");
  const board = page.getByTestId("board");
  const needs = board.getByRole("region", { name: "Needs you" });
  await expect(needs).toContainText("Parser drops the last token");
  await expect(needs).toContainText("5 of 5 gates");
  await expect(needs).toContainText("$2.07");
  await expect(needs).toContainText("Should anchors merge across files");
  await expect(needs.getByRole("link", { name: /Review PR #88/ })).toHaveAttribute("href", /\/pull\/88$/);
  // The running build says what it is doing, from its log.
  const building = phone(info) ? board.getByRole("region", { name: "On the floor" }) : board.getByRole("region", { name: "Building" });
  await expect(building).toContainText("editing webhook/retry.go");
  await expect(building.getByTitle("A job is running").first()).toBeVisible();
  if (!phone(info)) {
    await expect(board.getByRole("region", { name: "Reviewing" }).getByRole("img", { name: "Gates: 5 of 5 passed" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Live activity" })).toContainText("started building with opus");
  }
  await shot(page, info, "board");

  // One repo's chip narrows the floor to it.
  await page.getByRole("button", { name: "web", exact: true }).click();
  await expect(needs.getByTestId("card").filter({ visible: true })).toHaveCount(2);
  await expect(needs).not.toContainText("YAML anchors");
  await page.getByRole("button", { name: "All repos" }).click();
  await expect(needs.getByTestId("card").filter({ visible: true })).toHaveCount(6);
  if (phone(info)) {
    await noSidewaysScroll(page);
    await expect(page.getByRole("navigation", { name: "Tabs" })).toBeVisible();
    await page.getByRole("button", { name: /Triage/ }).click();
    await expect(board.getByRole("region", { name: "Triage" })).toContainText("Flaky TestCacheEviction");
  } else {
    await expect(board.getByRole("region", { name: "Blocked" })).toContainText("Waits on #102");
  }
});

test("an item over the cost limit waits for an OK on its estimate", async ({ page }, info) => {
  await page.goto("/app/");
  const needs = page.getByTestId("board").getByRole("region", { name: "Needs you" });
  const card = needs.getByTestId("card").filter({ hasText: "Rewrite the job scheduler" }).filter({ visible: true });
  await expect(card).toContainText("Cost check");
  await expect(card).toContainText("$9.40–$16.20");
  await card.getByRole("link", { name: "See the estimate" }).click();
  const est = page.getByTestId("estimate");
  await expect(est).toContainText("based on 6 past L items across all repos");
  await expect(est).toContainText("waiting for your OK");
  await est.getByRole("button", { name: "Approve estimate" }).click();
  await expect(est.getByRole("status")).toContainText("devserver: would approve item");
  await shot(page, info, "estimate");
  if (phone(info)) await noSidewaysScroll(page);
});

test("an item whose plan waits shows the plan, a Build it button and another plan's ask", async ({ page }, info) => {
  await page.goto("/app/");
  const needs = page.getByTestId("board").getByRole("region", { name: "Needs you" });
  const card = needs.getByTestId("card").filter({ hasText: "Split billing out of the monolith" }).filter({ visible: true });
  await expect(card).toContainText("Plan check");
  await expect(card).toContainText("build it or ask for another plan");
  await expect(card.getByRole("link", { name: /Open the issue/ })).toHaveAttribute("href", /acme\/api\/issues\/111$/);
  await card.getByRole("link", { name: "Read the plan" }).click();
  const plan = page.getByTestId("plan");
  await expect(plan).toContainText("Plan: waiting for your OK");
  await expect(plan).toContainText("Move billing/ into a new service");
  await expect(plan).toContainText("billing/service.go");
  await expect(plan).toContainText("counted twice");
  await expect(plan.getByRole("link", { name: /the issue/ })).toHaveAttribute("href", /acme\/api\/issues\/111$/);
  await plan.getByRole("button", { name: "Ask for another plan" }).click();
  const send = plan.getByRole("button", { name: "Send and plan again" });
  await expect(send).toBeDisabled();
  await plan.getByLabel(/What should change/).fill("Keep the tables in the monolith.");
  await send.click();
  await expect(plan.getByRole("status")).toContainText("devserver: would ask for another plan for item");
  await plan.getByRole("button", { name: "Build it" }).click();
  await expect(plan.getByRole("status")).toContainText("devserver: would approve item");
  await shot(page, info, "plan");
  if (phone(info)) await noSidewaysScroll(page);
});

test("a change across a workspace links a PR per repo, in merge order", async ({ page }, info) => {
  await page.goto("/app/");
  const card = page.getByRole("region", { name: "Needs you" }).getByTestId("card").filter({ hasText: "Show order totals" }).filter({ visible: true });
  await expect(card.getByRole("link", { name: /1\. Review api#91/ })).toHaveAttribute("href", /acme\/api\/pull\/91$/);
  await expect(card.getByRole("link", { name: /2\. Review web#92/ })).toHaveAttribute("href", /acme\/web\/pull\/92$/);
  await card.getByRole("link", { name: "Show order totals on the checkout page" }).click();
  const pr = page.getByTestId("pr-panel");
  await expect(pr).toContainText("2 linked PRs");
  await expect(pr.getByTestId("linked-prs")).toContainText("1.api#91");
  await expect(pr).toContainText("web: build");
  await shot(page, info, "workspace");
  if (phone(info)) await noSidewaysScroll(page);
});

test("the palette jumps to an item by number", async ({ page }, info) => {
  await page.goto("/app/");
  await expect(page.getByTestId("board")).toBeVisible();
  if (phone(info)) await page.getByRole("button", { name: "Search" }).click();
  else await page.keyboard.press("ControlOrMeta+k");
  const input = page.getByLabel("Jump to an item or screen");
  await input.fill("#104");
  await expect(page.getByRole("option")).toHaveCount(1);
  await input.press("Enter");
  await expect(page).toHaveURL(/\/app\/items\/4$/);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Support YAML anchors");
});

test("live activity reads like a log of the day", async ({ page }, info) => {
  await page.goto("/app/live");
  const feed = page.getByTestId("activity");
  await expect(feed).toContainText("PR #88 is ready for you");
  await expect(feed).toContainText("started building with opus");
  await expect(feed).toContainText("Orchestrator on api#103");
  await expect(page.getByText("merged this week")).toBeVisible();
  await shot(page, info, "live");
  if (phone(info)) await noSidewaysScroll(page);
});

test("PR panel shows CI, gates and findings linked to diff lines", async ({ page }, info) => {
  await page.goto("/app/");
  await page.getByTestId("card").filter({ hasText: "Parser drops" }).filter({ visible: true }).click();
  const pr = page.getByTestId("pr-panel");
  await expect(pr).toContainText("PR #88");
  await expect(pr).toContainText("review: pass");
  await expect(pr.getByRole("link", { name: "test", exact: true })).toBeVisible();
  await expect(pr).toContainText("proves_fix");
  await expect(pr.getByRole("link", { name: "Approve on GitHub" })).toHaveAttribute("href", /\/pull\/88\/files$/);
  await expect(page.getByText("Keep the final token")).toBeVisible();
  await shot(page, info, "item");
  if (phone(info)) await noSidewaysScroll(page);

  await pr.getByRole("link", { name: "parser/parser.go:42" }).click();
  await expect(page).toHaveURL(/\/diff#/);
  const inline = page.getByTestId("inline-finding").filter({ hasText: "bounds check" });
  await expect(inline).toBeVisible();
  await expect(page.getByTestId("diff-file")).toHaveCount(2);
  await shot(page, info, "diff");
  if (phone(info)) await noSidewaysScroll(page);
});

test("an item's page shows its build's screenshots, before and after", async ({ page }, info) => {
  await page.goto("/app/");
  await page.getByTestId("card").filter({ hasText: "Parser drops" }).filter({ visible: true }).click();
  const gallery = page.getByTestId("screenshots");
  await expect(gallery).toContainText("/settings");
  const imgs = gallery.getByRole("img");
  await expect(imgs).toHaveCount(4);
  await expect(gallery.getByRole("img", { name: "/settings at phone, with the change" })).toBeVisible();
  // Each image loads from the API, not a broken link.
  const loaded = await imgs.evaluateAll((els) => els.every((e) => (e as HTMLImageElement).complete && (e as HTMLImageElement).naturalWidth > 0));
  expect(loaded).toBe(true);
  await gallery.getByText("What couldn't be captured").click();
  await expect(gallery).toContainText("net::ERR_ABORTED");
  await shot(page, info, "screenshots");
  if (phone(info)) await noSidewaysScroll(page);
});

test("buttons go through the daemon and show its answer", async ({ page }) => {
  await page.goto("/app/items/1");
  await page.getByTestId("pr-panel").getByRole("button", { name: "Retry", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("devserver: would retry job");
});

test("session view streams the transcript and attaches a terminal", async ({ page }, info) => {
  await page.goto("/app/");
  await page.getByTestId("card").filter({ hasText: "webhook client" }).filter({ visible: true }).click();
  await page.getByRole("link", { name: /Watch the running implement job/ }).click();
  if (phone(info)) {
    await expect(page.getByRole("tab", { name: "Terminal" })).toBeVisible();
  }
  const transcript = page.getByTestId("transcript");
  await expect(transcript).toContainText("Writing a failing test first");
  // Narration is the thread; tool calls fold into a line each, failures in sight.
  await expect(transcript.getByTestId("session-header")).toContainText("claude-sonnet-4-5 · 7 tools");
  await expect(transcript).toContainText("Ran a command, read a file, searched once (1 failed)");
  await expect(transcript).toContainText("Ran go test ./parser (failed)");
  await expect(transcript.locator("strong", { hasText: "keep" })).toBeVisible();
  await expect(page.getByTestId("now")).toContainText("Now: Edited webhook/retry.go");
  // The output opens on request.
  await transcript.getByRole("button", { name: /Ran go test \.\/parser/ }).click();
  await expect(transcript).toContainText("--- FAIL: TestLastToken");
  // A message reaches the running agent at its next step.
  await page.getByLabel("Message the agent").fill("Also cover an empty input.");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByRole("status").filter({ hasText: "devserver: would send job" })).toBeVisible();
  if (phone(info)) await page.getByRole("tab", { name: "Terminal" }).click();
  await page.getByRole("button", { name: "Connect terminal" }).click();
  const term = page.getByTestId("terminal");
  await expect(page.getByText("· connected")).toBeVisible();
  await term.click();
  await page.keyboard.type("echo hello-$((6*7))\n");
  await expect(term).toContainText("hello-42");
  await shot(page, info, "session");
});

test("a build that ran out of turns waits for you, and Continue picks it up with more budget and a note", async ({ page }, info) => {
  await page.goto("/app/");
  const needs = page.getByTestId("board").getByRole("region", { name: "Needs you" });
  const card = needs.getByTestId("card").filter({ hasText: "Add SSO sign-in" }).filter({ visible: true });
  await expect(card).toContainText("Your call");
  await expect(card).toContainText("ran out of turns (80 of 80)");
  await expect(card.getByRole("button", { name: "Retry", exact: true })).toHaveAttribute("title", /from scratch.*claude\/sonnet/);
  await expect(card.getByRole("button", { name: "Tier up" })).toHaveAttribute("title", /claude\/opus/);
  await card.getByRole("button", { name: "Continue", exact: true }).click();
  await card.getByText("+50%").click();
  await card.getByLabel(/A note for the builder/).fill("Finish sign-out only; leave the settings page.");
  await card.getByRole("button", { name: "Continue from here" }).click();
  await expect(card.getByRole("status")).toContainText("devserver: would continue job");
  await expect(card.getByRole("status")).toContainText("50% more turns and budget");
  await expect(card.getByRole("status")).toContainText("Finish sign-out only");
  await shot(page, info, "stopped-card");

  // The item page says what happened and offers the same choices.
  await card.getByRole("link", { name: /Add SSO sign-in/ }).click();
  const next = page.getByTestId("next");
  await expect(next).toContainText("Continue (claude/sonnet): Picks up where it stopped");
  await expect(next).toContainText("Park: It's parked now");
  await expect(next).toContainText("factory:retry");

  // The job page: how it stopped, its note, the work it kept and the transcript's ending.
  await next.getByRole("link", { name: /job #/ }).click();
  const stop = page.getByTestId("stop");
  await expect(stop).toContainText("ran out of turns (80 of 80)");
  await expect(stop).toContainText("Left: the sign-out route");
  await stop.getByRole("button", { name: /See the work it did/ }).click();
  await expect(stop).toContainText("admin/sso.go");
  const transcript = page.getByTestId("transcript");
  await expect(page.getByTestId("outcome")).toContainText("Continue picks up from here");
  await expect(page.getByTestId("outcome")).toContainText("80 turns");
  await transcript.getByRole("button", { name: "Ran a command, edited 2 files" }).click();
  await transcript.getByRole("button", { name: /Ran go test \.\/admin/ }).click();
  await expect(transcript).toContainText("ok  admin 0.4s");
  await expect(page.getByRole("link", { name: "Raw log" })).toHaveAttribute("href", /\/log\.jsonl$/);
  await shot(page, info, "stopped-session");
  if (phone(info)) await noSidewaysScroll(page);
});

test("orchestrator feed filters by item", async ({ page }, info) => {
  await page.goto("/app/orchestrator");
  const feed = page.getByTestId("feed");
  await expect(feed.locator("li")).toHaveCount(4);
  await page.getByLabel("Item id").fill("3");
  await expect(feed.locator("li")).toHaveCount(1);
  await expect(feed).toContainText("one tier up");
  await shot(page, info, "orchestrator");
});

test("spend and metrics show charts", async ({ page }, info) => {
  await page.goto("/app/metrics");
  await expect(page.getByText("Cost per merged PR").first()).toBeVisible();
  await expect(page.getByRole("img").first()).toBeVisible();
  await expect(page.getByText("claude/opus")).toBeVisible();
  await shot(page, info, "metrics");
  if (phone(info)) await noSidewaysScroll(page);
});

test("inbox lists what needs you", async ({ page }, info) => {
  await page.goto("/app/notifications");
  const inbox = page.getByTestId("inbox");
  await expect(inbox).toContainText("#101 is ready for your review");
  await expect(inbox).toContainText("#104 needs your input");
  await expect(inbox).toContainText("anchors merge across files");
  // A stopped build's choices are right in the inbox.
  const stopped = inbox.getByRole("listitem").filter({ hasText: "#112" });
  await expect(stopped).toContainText("ran out of turns");
  await expect(stopped.getByRole("button", { name: "Continue", exact: true })).toBeVisible();
  await shot(page, info, "inbox");
  if (phone(info)) await noSidewaysScroll(page);
});

test("settings save to factory.yaml and restart once jobs finish", async ({ page }, info) => {
  await page.goto("/app/settings");
  const budgets = page.getByRole("region", { name: "Budgets" });
  await expect(page.getByRole("region", { name: "Models" })).toContainText("implement.L");
  await expect(budgets.getByLabel("Pause at usage (%)")).toHaveAttribute("placeholder", "95");
  const cap = budgets.getByLabel("Daily cap", { exact: true });

  // A value that would stop the factory is refused, and nothing is written.
  await cap.fill("0");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("alert")).toContainText("budgets.daily_total must be at least 0.01");

  await cap.fill(phone(info) ? "31" : "30");
  await page.getByRole("button", { name: "Save" }).click();
  // The devserver is shared: another run may have asked for the restart already.
  const notice = page.getByRole("status");
  await expect(notice).toContainText(/Changes apply when the factory restarts|Restart pending/);
  const restart = page.getByRole("button", { name: "Restart when jobs finish" });
  if (await restart.isVisible()) await restart.click();
  await expect(notice).toContainText("Restart pending, waiting for 1 running job");
  await expect(page.getByText("Gate commands, repos, the runner and the network are edited in factory.yaml.")).toBeVisible();
  await shot(page, info, "settings");
  if (phone(info)) await noSidewaysScroll(page);
});

test("a tuning proposal shows its changes, evidence and diff on the settings screen", async ({ page }, info) => {
  await page.goto("/app/settings");
  // The devserver is shared and its proposal stays pending, so this only looks.
  const card = page.getByRole("region", { name: "Tuning proposal" });
  await expect(card).toContainText("First-pass rate on S builds fell from 0.71 to 0.53");
  const changes = card.getByRole("list", { name: "Proposed changes" });
  await expect(changes.getByRole("listitem")).toHaveCount(2);
  await expect(changes).toContainText("budgets.concurrent_jobs");
  await expect(changes).toContainText("Jobs waited in the queue on 11 of 30 days");
  await expect(card.getByRole("region", { name: "Ideas" })).toContainText("tell triage to name the package");
  await expect(card.getByLabel("Diff")).toContainText("+  concurrent_jobs: 4");
  await expect(card.getByText("1 dropped change")).toBeVisible();
  await expect(card.getByRole("link", { name: /Draft PR/ })).toHaveAttribute("href", "https://github.com/acme/api/pull/120");
  await expect(card.getByRole("button", { name: "Apply" })).toBeEnabled();
  // Each change can be left out.
  const pick = card.getByRole("checkbox", { name: "Apply budgets.concurrent_jobs" });
  await expect(pick).toBeChecked();
  await pick.uncheck();
  await expect(card.getByRole("button", { name: "Apply 1 of 2" })).toBeEnabled();
  await pick.check();
  await expect(card.getByRole("button", { name: "Dismiss" })).toBeEnabled();
  await shot(page, info, "tuning");
  if (phone(info)) await noSidewaysScroll(page);
});

test("a flaky main shows above every screen, linked to its fix", async ({ page }, info) => {
  await page.goto("/app/metrics");
  const health = page.getByRole("region", { name: "Main health" });
  await expect(health).toContainText("api main is flaky: test #106");
  await expect(health.getByRole("link", { name: "#106" })).toHaveAttribute("href", /acme\/api\/issues\/106$/);
  await expect(health).not.toContainText("web main");
  if (phone(info)) await noSidewaysScroll(page);
});
