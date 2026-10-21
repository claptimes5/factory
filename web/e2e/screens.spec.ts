import { expect, test } from "@playwright/test";

test("History has the activity, what's done and the orchestrator's decisions; /live still leads there", async ({ page }) => {
  await page.goto("/app/live");
  await expect(page).toHaveURL(/\/app\/history$/);
  const history = page.getByTestId("history");
  await expect(history.getByRole("tab", { name: "Activity" })).toHaveAttribute("aria-selected", "true");
  await history.getByRole("tab", { name: "Done" }).click();
  await expect(page).toHaveURL(/tab=done$/);
  await expect(history.getByRole("list", { name: "Done" }).getByRole("link").first()).toBeVisible();
  await history.getByRole("tab", { name: "Orchestrator's decisions" }).click();
  await expect(page.getByText("What the orchestrator did on its own, and why.")).toBeVisible();
});

test("an item shows its way along the line as stamps", async ({ page }) => {
  await page.goto("/app/line");
  const card = page.getByRole("region", { name: "Needs you" }).getByTestId("card").filter({ hasText: "Parser drops the last token" }).filter({ visible: true });
  await card.getByRole("link", { name: /Parser drops the last token/ }).click();
  const stamps = page.getByTestId("stamps");
  await expect(stamps.getByRole("listitem", { name: "Plan: done" })).toBeVisible();
  await expect(stamps.getByRole("listitem", { name: "Review: done" })).toBeVisible();
  await expect(stamps.getByRole("listitem", { name: "You: now" })).toBeVisible();
  await expect(stamps.getByRole("listitem", { name: "Live: ahead" })).toBeVisible();
});

test("on a phone, Settings leads to the screens the tabs leave out", async ({ page }) => {
  await page.goto("/app/settings");
  const more = page.getByRole("navigation", { name: "More settings" });
  await more.getByRole("link", { name: /Spend/ }).click();
  await expect(page).toHaveURL(/\/app\/metrics$/);
});
