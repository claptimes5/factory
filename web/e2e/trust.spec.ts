import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

test("Trust shows each repo on the ladder toward full automation, with the levels not built yet marked", async ({ page }, info) => {
  await page.goto("/app/trust");
  const levels = page.getByTestId("level");
  await expect(levels).toHaveCount(7);
  // The sample's auto_merge (low risk, up to size S) is level 3.
  await expect(page.locator('[data-testid="level"][aria-current="step"]')).toContainText("Merge small, low-risk changes");
  await expect(levels.nth(5)).toContainText("Not built yet");
  await expect(levels.nth(6)).toContainText("Ship");
  await expect(page.getByRole("region", { name: "Guard rails" })).toContainText("Always asks you: migrations, new dependencies, auth code, deleted tests");
  await shot(page, info, "trust");
  // acme/web's breaker tripped: it stepped back.
  await page.getByRole("radio", { name: "web" }).click();
  await expect(page.getByText("Stepped back to level 1.")).toBeVisible();
  if (phone(info)) {
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  }
});

test("a level the record has earned is offered on Trust and on Home; moving there says when it applies", async ({ page }) => {
  // The record and the save are played here, so the sample config stays as it is.
  let level = 3;
  const trust = async (route: import("@playwright/test").Route) => {
    const res = await route.fetch({ url: page.url().replace(/\/app\/.*/, "/api/v1/trust"), method: "GET", headers: {} });
    const body = await res.json();
    body.pending = level !== 3;
    body.repos[0] = { ...body.repos[0], level, merged: 24, suggest: level === 3 ? 4 : 0, suggest_why: level === 3 ? "Level 4 would have merged 24 changes on its own in the last 30 days, and none was reverted or broke main." : "Not yet." };
    return body;
  };
  await page.route(/\/api\/v1\/trust$/, async (route) => {
    if (route.request().method() === "POST") {
      level = route.request().postDataJSON().level;
      return route.fulfill({ json: { message: `acme/api is set to level ${level}: merge low-risk changes of any normal size. It applies once the factory restarts.`, trust: await trust(route) } });
    }
    return route.fulfill({ json: await trust(route) });
  });
  await page.goto("/app/");
  await page.getByRole("link", { name: "Earned more trust" }).click();
  await expect(page).toHaveURL(/\/app\/trust\?repo=acme%2Fapi$/);
  const earned = page.getByRole("region", { name: "Earned" });
  await expect(earned).toContainText("Ready for level 4?");
  await earned.getByRole("button", { name: "Move to level 4" }).click();
  await expect(page.getByText("acme/api is set to level 4").first()).toBeVisible();
  await expect(page.getByText("Saved changes apply once the factory restarts.")).toBeVisible();
  await expect(page.locator('[data-testid="level"][aria-current="step"]')).toContainText("Merge low-risk changes of any normal size");
});
