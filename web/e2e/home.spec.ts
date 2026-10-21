import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

test("Home shows what needs you one decision at a time, reviews first, with the rest next", async ({ page }, info) => {
  await page.goto("/app/");
  const needs = page.getByRole("region", { name: "Needs you" });
  await expect(needs.getByRole("heading", { level: 1 })).toHaveText("7 things need you");
  const top = needs.getByTestId("decision");
  await expect(top).toHaveCount(1);
  await expect(top).toContainText("Review · web#101");
  await expect(top).toContainText("Parser drops the last token without a trailing newline");
  await expect(top.getByRole("link", { name: /Review the change/ })).toHaveAttribute("href", /\/pull\/88$/);
  await expect(needs.getByRole("list", { name: "After this" })).toContainText("Next: review Show order totals on the checkout page");
  // Later puts it at the back: the next one comes up.
  await top.getByRole("button", { name: "Later" }).click();
  await expect(top).toContainText("Show order totals on the checkout page");
  await expect(top.getByRole("link", { name: /1\. Review api#91/ })).toBeVisible();
  // The line, with you as its last station.
  const line = page.getByRole("region", { name: "The line" });
  await expect(line.getByRole("listitem", { name: "You: 7" })).toBeVisible();
  await line.getByRole("link", { name: "See the whole line" }).click();
  await expect(page).toHaveURL(/\/app\/line$/);
  await expect(page.getByTestId("board")).toBeVisible();
  await page.goBack();
  await shot(page, info, "home");
  if (phone(info)) {
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  }
});

test("Home says what went live since you looked, until you dismiss it", async ({ page }) => {
  let dismissed = false;
  await page.route(/\/api\/v1\/home$/, async (route) => {
    const went_live = dismissed ? [] : [{ id: 1, title: "Fix flaky tests", ref: "api#106" }, { id: 2, title: "Start a feature map", ref: "web#7" }];
    await route.fulfill({ json: { seen_at: "2026-10-20T08:00:00Z", went_live } });
  });
  await page.route(/\/api\/v1\/home\/seen$/, async (route) => {
    dismissed = true;
    await route.fulfill({ json: { seen_at: new Date().toISOString(), went_live: [] } });
  });
  await page.goto("/app/");
  const since = page.getByRole("region", { name: "Since you looked" });
  await expect(since).toContainText("2 changes went live");
  await expect(since).toContainText("Fix flaky tests, Start a feature map");
  await since.getByRole("button", { name: "Dismiss" }).click();
  await expect(since).toHaveCount(0);
});

test("Home's Ask box opens Ask", async ({ page }) => {
  await page.goto("/app/");
  await page.getByRole("button", { name: "Ask, or hand over a task…" }).click();
  await expect(page.getByTestId("ask").filter({ visible: true }).getByLabel("Message")).toBeVisible();
});
