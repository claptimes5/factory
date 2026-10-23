import { expect, test, type Page } from "@playwright/test";

// The sample factory has one workspace, acme; these add a second, site,
// with a repo of its own and nothing in it yet.
async function twoWorkspaces(page: Page) {
  await page.route(/\/api\/v1\/overview$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.workspaces = [...body.workspaces, { name: "site", repos: ["acme/site"], daily_cap_usd: 0, spent_today_usd: 0 }];
    await route.fulfill({ json: body });
  });
}

const switcher = (page: Page, phone: boolean) => page.locator(phone ? "#workspace-top" : "#workspace-side");

test("one workspace has no switcher", async ({ page }) => {
  await page.goto("/app/");
  await expect(page.getByTestId("home")).toBeVisible();
  await expect(page.getByLabel("Workspace", { exact: true })).toHaveCount(0);
});

test("the switcher scopes every screen to one workspace, and Home and the Inbox say what waits in the others", async ({ page }, info) => {
  const phone = info.project.name === "phone";
  await twoWorkspaces(page);
  await page.goto("/app/");
  const pick = switcher(page, phone);
  await expect(pick).toHaveValue("acme");
  await expect(page.getByRole("region", { name: "Needs you" })).toBeVisible();
  await expect(page.getByRole("region", { name: "In other workspaces" })).toHaveCount(0);

  await pick.selectOption("site");
  await expect(page.getByRole("heading", { name: "All humming along." })).toBeVisible();
  const elsewhere = page.getByRole("region", { name: "In other workspaces" });
  await expect(elsewhere).toContainText(/In acme, \d+ things need you\./);

  // The line is site's too: none of acme's cards.
  await page.goto("/app/line");
  await expect(page.getByTestId("card").filter({ hasText: "Parser drops the last token" })).toHaveCount(0);

  // The Inbox keeps what's about the whole factory, and counts acme's.
  await page.goto("/app/notifications");
  await expect(page.locator('[data-testid="inbox"] a[href*="/items/"]')).toHaveCount(0);
  await expect(page.getByLabel("In other workspaces")).toContainText(/In acme, \d+ more need you\./);
  await page.getByRole("button", { name: "Switch to it" }).first().click();
  await expect(switcher(page, phone)).toHaveValue("acme");
  await expect(page.getByTestId("inbox")).toBeVisible();
});

test("a link with ?ws= opens that workspace, and an item brings its own on screen", async ({ page }, info) => {
  const phone = info.project.name === "phone";
  await twoWorkspaces(page);
  await page.goto("/app/line?ws=site");
  await expect(switcher(page, phone)).toHaveValue("site");
  // An acme item opened from a link switches to acme.
  await page.goto("/app/line?ws=acme");
  const card = page.getByTestId("card").filter({ hasText: "Parser drops the last token" }).filter({ visible: true });
  const href = await card.getByRole("link", { name: /Parser drops the last token/ }).getAttribute("href");
  await page.goto(`/app/notifications?ws=site`);
  await expect(switcher(page, phone)).toHaveValue("site");
  await page.goto(href!.startsWith("/app") ? href! : `/app${href}`);
  await expect(switcher(page, phone)).toHaveValue("acme");
});
