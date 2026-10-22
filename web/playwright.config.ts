import { defineConfig, devices } from "@playwright/test";

// Runs against the devserver: the real Go handlers over a seeded database.
const port = 8099;

export default defineConfig({
  testDir: "e2e",
  timeout: 30_000,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: { baseURL: `http://127.0.0.1:${port}`, trace: "retain-on-failure" },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } } },
    { name: "phone", use: { ...devices["Pixel 7"] } },
  ],
  webServer: {
    command: `go run ../internal/web/devserver -listen 127.0.0.1:${port}`,
    url: `http://127.0.0.1:${port}/api/v1/overview`,
    timeout: 120_000,
    // npm run e2e starts it first (see scripts/e2e.mjs), so it's this run's own.
    reuseExistingServer: !process.env.CI || !!process.env.FACTORY_E2E_SERVER,
  },
});
