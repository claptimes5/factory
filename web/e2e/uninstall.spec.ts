import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

test("Remove the factory lists what goes first, and removes it only once confirmed", async ({ page }, info) => {
  await page.goto("/app/settings");
  const region = page.getByRole("region", { name: "Remove the factory" });
  await expect(region).toContainText("./factory uninstall");
  await region.getByRole("button", { name: "Show what would be removed" }).click();

  // The dry run: labels with their open issues and PRs, images, data, and what's left to you.
  await expect(region).toContainText("Deleting a label takes it off every issue and PR that has it.");
  await expect(region.getByTestId("uninstall-labels").first()).toContainText("factory:parked 3 open");
  await expect(region.getByTestId("uninstall-images")).toContainText("factory-job:latest 2.31GB");
  await expect(region.getByTestId("uninstall-images")).toContainText("(an old copy)");
  const data = region.getByRole("checkbox", { name: /Also remove the job data/ });
  await expect(data).not.toBeChecked();
  await expect(region).toContainText("The database (items, jobs, spend)");
  await expect(region.getByRole("link", { name: /settings\/apps\/acme-factory/ }).first()).toHaveAttribute("href", "https://github.com/settings/apps/acme-factory");
  await expect(region).toContainText("delete /srv/factory/factory.yaml and /srv/factory/factory.env yourself");
  await shot(page, info, "uninstall-plan");
  if (phone(info)) await noSidewaysScroll(page);

  // Cancel removes nothing.
  await region.getByRole("button", { name: "Remove…" }).click();
  const dialog = page.getByRole("dialog", { name: "Remove the factory?" });
  await expect(dialog).toContainText("Delete 5 labels from acme/api, acme/web");
  await expect(dialog).toContainText("Each comes off every issue and PR that has it (10 open now)");
  await expect(dialog).toContainText("Remove 2 job images");
  await expect(dialog).not.toContainText("Remove the job data");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  await expect(region.getByTestId("uninstall-result")).toHaveCount(0);

  // With the data too, confirmed: each item says how it went.
  await data.check();
  await region.getByRole("button", { name: "Remove…" }).click();
  await expect(dialog).toContainText("Remove the job data");
  await dialog.getByRole("button", { name: "Remove", exact: true }).click();
  const result = region.getByTestId("uninstall-result");
  await expect(result).toContainText("1 of 8 couldn't be removed.");
  await expect(result).toContainText("label factory:parked on acme/api");
  await expect(result).toContainText("image a1b2c3d4e5f6: image is being used by stopped container");
  await expect(result).toContainText("data /srv/factory/data/jobs");
  await expect(result).toContainText("Stop the factory now");
  await shot(page, info, "uninstall-result");
});
