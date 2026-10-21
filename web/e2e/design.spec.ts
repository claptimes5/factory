import { expect, test } from "@playwright/test";

test("the style guide shows every part of the design system, and fits a phone", async ({ page }, info) => {
  await page.goto("/app/design");
  await expect(page.getByRole("heading", { name: "Design" })).toBeVisible();
  for (const name of ["Color", "Type", "Buttons", "Badges", "Forms", "Alerts", "Progress", "Tabs and accordion", "Overlays"]) {
    await expect(page.getByRole("region", { name })).toBeVisible();
  }
  // A field ties its help and error to its control.
  await expect(page.getByLabel("GitHub token")).toHaveAttribute("aria-invalid", "true");
  await expect(page.getByLabel("Repository")).toHaveAttribute("aria-describedby", "design-repo-help");
  // A segmented control always keeps one choice.
  const gitlab = page.getByRole("radio", { name: "GitLab" });
  await gitlab.click();
  await expect(gitlab).toHaveAttribute("aria-checked", "true");
  await gitlab.click();
  await expect(gitlab).toHaveAttribute("aria-checked", "true");
  // A sheet opens and closes.
  await page.getByRole("button", { name: "Open a sheet" }).click();
  await expect(page.getByRole("dialog", { name: "Ask the factory" })).toBeVisible();
  await page.getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.getByRole("button", { name: "Show a toast" }).click();
  await expect(page.getByText("Saved the token. The factory can reach acme/web.")).toBeVisible();
  await info.attach("design", { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
  if (info.project.name === "phone") {
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  }
});
