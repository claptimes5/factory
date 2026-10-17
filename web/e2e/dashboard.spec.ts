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

test("board shows items by stage with live status and badges", async ({ page }, info) => {
  await page.goto("/app/");
  const board = page.getByTestId("board");
  await expect(board.getByRole("region", { name: "Needs review" })).toContainText("Parser drops the last token");
  await expect(board.getByRole("region", { name: "Parked" })).toContainText("needs you");
  await expect(board.getByRole("region", { name: "Implementing" }).getByTitle("A job is running")).toBeVisible();
  await expect(board.getByRole("region", { name: "Needs review" })).toContainText("$2.07");
  await page.getByLabel("Filter items").fill("#104");
  await expect(page.getByTestId("card")).toHaveCount(1);
  await page.getByLabel("Filter items").fill("");
  await shot(page, info, "board");
  if (phone(info)) {
    await noSidewaysScroll(page);
    await expect(page.getByRole("navigation", { name: "Tabs" })).toBeVisible();
  }
});

test("PR panel shows CI, gates and findings linked to diff lines", async ({ page }, info) => {
  await page.goto("/app/");
  await page.getByTestId("card").filter({ hasText: "Parser drops" }).click();
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
  await page.getByTestId("card").filter({ hasText: "webhook client" }).click();
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
