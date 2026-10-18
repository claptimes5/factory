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
  await expect(needs.getByTestId("card").filter({ visible: true })).toHaveCount(3);
  if (phone(info)) {
    await noSidewaysScroll(page);
    await expect(page.getByRole("navigation", { name: "Tabs" })).toBeVisible();
    await page.getByRole("button", { name: /Triage/ }).click();
    await expect(board.getByRole("region", { name: "Triage" })).toContainText("Flaky TestCacheEviction");
  } else {
    await expect(board.getByRole("region", { name: "Blocked" })).toContainText("Waits on #102");
  }
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
  await expect(page.getByTestId("transcript")).toContainText("Writing a failing test first.");
  await expect(page.getByTestId("transcript")).toContainText("→ Bash");
  if (phone(info)) await page.getByRole("tab", { name: "Terminal" }).click();
  await page.getByRole("button", { name: "Connect terminal" }).click();
  const term = page.getByTestId("terminal");
  await expect(page.getByText("· connected")).toBeVisible();
  await term.click();
  await page.keyboard.type("echo hello-$((6*7))\n");
  await expect(term).toContainText("hello-42");
  await shot(page, info, "session");
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
