import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

test("every screen sits in one frame: the sidebar on a desk, tabs on a phone", async ({ page }, info) => {
  await page.goto("/app/");
  if (phone(info)) {
    const tabs = page.getByRole("navigation", { name: "Tabs" });
    await expect(tabs).toBeVisible();
    for (const name of [/^Home$/, /^Line$/, /^History$/, /^Settings$/]) await expect(tabs.getByRole("link", { name })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Main" })).toBeHidden();
  } else {
    const main = page.getByRole("navigation", { name: "Main" });
    await expect(main).toBeVisible();
    for (const name of [/^Home$/, /^The line$/, /^History$/, /^Spend$/, /^Settings$/]) await expect(main.getByRole("link", { name })).toBeVisible();
    await main.getByRole("link", { name: "Spend" }).click();
    await expect(page).toHaveURL(/\/app\/metrics$/);
    await expect(page.getByRole("navigation", { name: "Tabs" })).toBeHidden();
  }
  await shot(page, info, "frame");
});

test("warnings for every screen share one notice area: the most serious shows, the rest wait behind more", async ({ page }) => {
  await page.goto("/app/");
  const area = page.getByRole("group", { name: "Notices" });
  await expect(area.getByRole("region")).toHaveCount(1);
  await expect(area.getByRole("region", { name: "Main health" })).toContainText("api main is flaky");
  await area.getByRole("button", { name: "2 more notices" }).click();
  await expect(area.getByRole("region")).toHaveCount(3);
  await expect(area.getByRole("region", { name: "Auto-merge breaker" })).toContainText("Auto-merge is off");
  await expect(area.getByRole("region", { name: "Setup", exact: true })).toContainText("Setup isn't finished.");
  await area.getByRole("button", { name: "Show fewer" }).click();
  await expect(area.getByRole("region")).toHaveCount(1);
});

test("Ask the factory is a sheet over the screen, with suggestions to start from", async ({ page }, info) => {
  await page.goto("/app/");
  if (phone(info)) await page.getByRole("navigation", { name: "Tabs" }).getByRole("button", { name: "Ask" }).click();
  else await page.getByRole("button", { name: "Ask the factory" }).click();
  const sheet = page.getByRole("dialog", { name: "Ask the factory" });
  await expect(sheet).toBeVisible();
  await sheet.getByRole("button", { name: "What's stuck, and why?" }).click();
  await expect(sheet.getByLabel("Message")).toHaveValue("What's stuck, and why?");
  await shot(page, info, "ask");
  // Following a link out of it closes it.
  await sheet.getByRole("link", { name: "Open the full chat" }).click();
  await expect(page).toHaveURL(/\/app\/orchestrator$/);
  await expect(sheet).toHaveCount(0);
});

test("on a wide screen, Ask stays beside every screen until you close it", async ({ page }, info) => {
  test.skip(phone(info), "a wide screen only");
  await page.setViewportSize({ width: 1600, height: 900 });
  await page.goto("/app/");
  const toggle = page.getByRole("button", { name: "Ask the factory" });
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-pressed", "true");
  const panel = page.getByRole("complementary", { name: "Ask the factory" });
  await expect(panel.getByRole("button", { name: "What's running right now?" })).toBeVisible();
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Spend" }).click();
  await expect(panel).toBeVisible();
  await page.reload();
  await expect(panel).toBeVisible();
  await toggle.click();
  await expect(panel).toHaveCount(0);
});
