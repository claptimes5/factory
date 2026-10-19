import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

test("while a repo isn't ready, a banner leads to its setup steps and the work on each blocker", async ({ page }, info) => {
  await page.goto("/app/");
  const banner = page.getByRole("region", { name: "Setup", exact: true });
  await expect(banner).toContainText("Setup isn't finished.");
  await expect(banner).toContainText("api: 1 of 3 setup steps done");
  await banner.getByRole("link", { name: "See what's missing" }).click();
  await expect(page).toHaveURL(/\/app\/setup$/);
  // The banner leaves once you're on the page it points at.
  await expect(page.getByRole("region", { name: "Setup", exact: true })).toHaveCount(0);

  const api = page.getByRole("region", { name: "Setup of acme/api" });
  await expect(api).toContainText("Not ready yet");
  await expect(api).toContainText("1 of 3 setup steps done");
  await expect(api.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "1");
  const steps = api.getByTestId("setup-step");
  await expect(steps).toHaveCount(3);
  await expect(steps.nth(0)).toContainText("Dependencies install from a clean checkout");
  await expect(steps.nth(0).getByLabel("Done")).toBeVisible();

  // Each blocker links its issue and says what the factory is doing about it.
  const lint = steps.nth(1).getByTestId("readiness-finding");
  await expect(lint).toContainText("lint fails on main");
  await expect(lint).toContainText("Being built");
  await expect(lint.getByRole("link", { name: /#113/ })).toHaveAttribute("href", /acme\/api\/issues\/113$/);
  await lint.getByRole("button", { name: /Details/ }).click();
  await expect(lint).toContainText("ineffectual assignment");
  const flaky = steps.nth(2).getByTestId("readiness-finding");
  await expect(flaky).toContainText("Tests are flaky");
  await expect(flaky).toContainText("Waiting its turn");
  // Filing is a button, never a command to run.
  await expect(api).not.toContainText("./factory");
  await expect(api.getByRole("button", { name: "File issues" })).toBeVisible();
  await expect(api.getByRole("button", { name: "File and start work" })).toBeVisible();

  // A gap that isn't needed to start waits under Worth doing later.
  await api.getByText("Worth doing later (2)").click();
  await expect(api).toContainText("Filed, not queued");
  await expect(api).toContainText("No issue yet");

  const web = page.getByRole("region", { name: "Setup of acme/web" });
  await expect(web).toContainText("3 of 3 setup steps done");
  await expect(web).toContainText("Ready: add the factory:ready label to issues to start.");
  await expect(web).toContainText("1 issue the check filed for later isn't labeled yet");
  await shot(page, info, "setup");
  if (phone(info)) await noSidewaysScroll(page);

  // Check again runs in the background and the page follows it.
  await api.getByRole("button", { name: "Check again" }).click();
  // The other project may be running one too: one run at a time.
  await expect(api.getByRole("status").first()).toContainText(/[Cc]hecking/);
  await expect(api.getByRole("button", { name: "Check again" })).toBeVisible({ timeout: 15_000 });
  await expect(api).toContainText(/Checked \d+s ago/);

  // File and start work checks again, then files; the page follows it. The
  // devserver changes nothing, so the other project sees the same report.
  if (!phone(info)) {
    // Retried, since the phone project's own check can hold the one slot.
    await expect(async () => {
      await api.getByRole("button", { name: "File and start work" }).click({ timeout: 5_000 });
      await expect(api.getByRole("status").first()).toContainText(/[Cc]hecking/);
      await expect(api.getByText("Opened 1 issue(s) on acme/api.")).toBeVisible({ timeout: 8_000 });
    }).toPass({ timeout: 40_000 });
  }

  // A blocker's work opens from its row.
  await lint.getByRole("link", { name: "See the work" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Make the lint check pass on main");
});

test("plan usage shows as percentages, and dollar costs say they're API-price estimates", async ({ page }, info) => {
  await page.goto("/app/");
  const usage = page.getByRole("group", { name: "Plan usage" });
  await expect(usage).toContainText("5-hour: 42% used, resets");
  await expect(usage).toContainText("Weekly: 18% used");
  await expect(usage.getByTestId("plan-usage").filter({ hasText: "Codex" })).toContainText("no reading yet");
  await expect(usage.getByRole("meter", { name: "Claude 5-hour usage" })).toHaveAttribute("aria-valuenow", "42");
  if (phone(info)) await noSidewaysScroll(page);

  await page.goto("/app/items/1");
  const note = page.getByTestId("plan-cost-note");
  await expect(note).toContainText("Claude runs on your Claude subscription: its dollar figures are an estimate at API prices");
  await expect(note).toContainText("Codex runs on your ChatGPT plan, so its jobs cost no dollars here");
  await expect(note).toContainText("No reading yet: Codex reports usage");
  await shot(page, info, "plan-usage");
  if (phone(info)) await noSidewaysScroll(page);
});
