import { expect, test } from "@playwright/test";

// The same sample data behind a password: scripts/e2e.mjs starts this
// devserver with -password beside the open one the other specs use.
const password = "e2e sample password";
test.use({ baseURL: "http://127.0.0.1:8098" });

test("nothing on the dashboard works until you sign in, and you land where you were going", async ({ page }) => {
  const before = await page.request.get("/api/v1/overview");
  expect(before.status()).toBe(401);
  expect(await before.json()).toEqual({ error: "sign in first" });

  await page.goto("/app/settings");
  await expect(page).toHaveURL(/\/login\?next=%2Fapp%2Fsettings$/);
  await page.getByLabel("Password").fill("not the password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText("Seeded failure for the Step 6 gate")).toBeVisible();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/app\/settings$/);
  await expect(page.getByRole("navigation", { name: "More settings" })).toBeVisible();
  expect((await page.request.get("/api/v1/overview")).status()).toBe(200);
});

test("a session that ends while the dashboard is open sends you to sign in, then back", async ({ page, context }) => {
  await page.goto("/login?next=%2Fapp%2Fsettings");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "More settings" })).toBeVisible();

  // The next API call the page makes gets a 401; the dashboard goes to sign in.
  await context.clearCookies();
  await page.getByRole("navigation", { name: "More settings" }).getByRole("link", { name: /When to tell you/ }).click();
  await expect(page).toHaveURL(/\/login\?next=%2Fapp%2Fnotify$/);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/app\/notify$/);
});
