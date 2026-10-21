import { expect, test } from "@playwright/test";

test("When to tell you: what gets through, push on this device, the devices and quiet hours", async ({ page }) => {
  await page.route(/\/api\/v1\/push$/, (route) =>
    route.fulfill({
      json: {
        available: true,
        public_key: "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8",
        devices: [{ id: "a1b2", agent: "iPhone", created_at: new Date(Date.now() - 86_400_000).toISOString(), failed_at: "" }],
      },
    }),
  );
  let saved: { path: string[]; value: unknown }[] = [];
  await page.route(/\/api\/v1\/settings$/, async (route) => {
    const res = await route.fetch({ url: page.url().replace(/\/app\/.*/, "/api/v1/settings"), method: "GET", headers: {} });
    const body = await res.json();
    if (route.request().method() === "POST") {
      saved = route.request().postDataJSON().changes;
      return route.fulfill({ json: { ...body, changed: true } });
    }
    return route.fulfill({ json: body });
  });
  await page.goto("/app/settings");
  await page.getByRole("navigation", { name: "More settings" }).getByRole("link", { name: /When to tell you/ }).click();
  await expect(page).toHaveURL(/\/app\/notify$/);
  await expect(page.getByRole("region", { name: "What gets through" })).toContainText("Always, even in quiet hours");
  const devices = page.getByRole("region", { name: "Devices" });
  await expect(devices.getByRole("button", { name: "Turn on notifications" })).toBeVisible();
  await expect(devices.getByRole("list", { name: "Devices with notifications on" })).toContainText("iPhone");

  const quiet = page.getByRole("region", { name: "Quiet hours" });
  await quiet.getByLabel("From").fill("19:00");
  await quiet.getByLabel("Until").fill("08:00");
  await quiet.getByRole("switch").click();
  await quiet.getByRole("button", { name: "Save quiet hours" }).click();
  await expect(page.getByText("Saved. Quiet hours apply once the factory restarts.")).toBeVisible();
  expect(saved).toEqual([
    { path: ["notify", "quiet_hours", "from"], value: "19:00" },
    { path: ["notify", "quiet_hours", "until"], value: "08:00" },
    { path: ["notify", "quiet_hours", "weekends"], value: true },
    { path: ["notify", "quiet_hours", "timezone"], value: null },
  ]);
});

test("the dashboard installs as a web app: its manifest, icons and service worker are served", async ({ request }) => {
  const m = await request.get("/app/manifest.webmanifest");
  expect(m.headers()["content-type"]).toContain("application/manifest+json");
  const manifest = await m.json();
  expect(manifest.start_url).toBe("/app/");
  for (const icon of manifest.icons) expect((await request.get(icon.src)).ok()).toBe(true);
  const sw = await request.get("/app/sw.js");
  expect(sw.ok()).toBe(true);
  expect(sw.headers()["cache-control"]).toBe("no-cache");
  expect(await sw.text()).toContain('addEventListener("push"');
});
