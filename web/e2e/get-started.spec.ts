import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

const step = (page: Page, key: string) => page.locator(`[data-testid="wizard-step"][data-key="${key}"]`);

/** Opens a step's card, unless it is the one already open. */
async function openStep(page: Page, key: string) {
  const head = step(page, key).locator("button[aria-expanded]");
  if ((await head.getAttribute("aria-expanded")) !== "true") await head.click();
  await expect(head).toHaveAttribute("aria-expanded", "true");
  return step(page, key);
}

// When a check fails, say what the page and the API showed.
test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  const api = await page.request.get("/api/v1/setup");
  console.log(`page ${page.url()}:\n${await page.locator("main").innerText({ timeout: 1000 }).catch((e) => String(e))}`);
  console.log(`GET /api/v1/setup = ${api.status()} ${(await api.text()).slice(0, 2000)}`);
});

test("the get-started page lists each step and opens the first that needs attention", async ({ page }, info) => {
  await page.goto("/app/get-started");
  await expect(page.getByRole("heading", { name: "Get started" })).toBeVisible();
  const steps = page.getByTestId("wizard-step");
  await expect(steps).toHaveCount(9);
  await expect(step(page, "machine")).toHaveAttribute("data-state", "done");
  await expect(step(page, "notify")).toHaveAttribute("data-state", "optional");
  await expect(step(page, "claude")).toContainText("API key saved");
  // Exactly one card is open: the first that needs something.
  await expect(page.locator('[data-testid="wizard-step"] button[aria-expanded="true"]')).toHaveCount(1);
  const open = page.locator('[data-testid="wizard-step"]:has(button[aria-expanded="true"])');
  await expect(open).toHaveAttribute("data-state", "attention");

  // This machine: each check, with the fix for what isn't right.
  await openStep(page, "machine");
  await expect(step(page, "machine").getByTestId("machine-finding")).toHaveCount(3);
  await expect(step(page, "machine")).toContainText("sudo apt-get install ttyd");
  await step(page, "machine").getByRole("button", { name: "Re-check" }).click();
  await expect(step(page, "machine").getByRole("status")).toContainText("Checked this machine again.");

  // A saved secret comes back only masked.
  await openStep(page, "claude");
  await expect(step(page, "claude").getByLabel("API key or subscription token")).toHaveAttribute("placeholder", /Saved: sk-a…wxyz/);
  await shot(page, info, "get-started");
  if (phone(info)) await noSidewaysScroll(page);
});

test("a key that doesn't look like Claude's is refused unless you say to use it", async ({ page }) => {
  await page.goto("/app/get-started");
  const claude = step(page, "claude");
  await openStep(page, "claude");
  await claude.getByLabel("API key or subscription token").fill("not-a-key");
  await claude.getByRole("button", { name: "Save", exact: true }).click();
  await expect(claude.getByRole("alert")).toContainText("doesn't look like an Anthropic key");
  await expect(claude.getByRole("button", { name: "Use it anyway" })).toBeVisible();
});

test("a ChatGPT plan signs in from the page: it shows the link and code, then that it's signed in", async ({ page }, info) => {
  await page.goto("/app/get-started");
  const codex = step(page, "codex");
  await openStep(page, "codex");
  await expect(codex.getByRole("radio", { name: /A ChatGPT plan/ })).toBeChecked();
  await expect(codex).not.toContainText("./factory");
  const signIn = codex.getByRole("button", { name: "Sign in", exact: true });
  await expect(signIn).toBeVisible();
  if (phone(info)) await noSidewaysScroll(page);
  // Signing in changes the shared sample setup, so one project does it.
  if (phone(info)) return;
  // A sign-in can be called off.
  await signIn.click();
  await expect(codex.getByTestId("device-code")).toHaveText("DEVS-12345");
  await codex.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(codex.getByText("Sign-in cancelled.").first()).toBeVisible();
  await expect(codex.getByTestId("device-code")).toHaveCount(0);
  await signIn.click();
  await expect(codex.getByTestId("device-code")).toHaveText("DEVS-12345");
  await expect(codex.getByRole("link", { name: /auth\.openai\.com\/codex\/device/ })).toHaveAttribute("href", "https://auth.openai.com/codex/device");
  await shot(page, info, "get-started-codex-sign-in");
  await expect(codex.getByText("Signed in. Codex uses your ChatGPT plan.")).toBeVisible({ timeout: 10_000 });
  await expect(codex.getByTestId("device-code")).toHaveCount(0);
});

test("a machine check about a job image has a Build button, not a command", async ({ page }) => {
  // The sample machine has every image the check looks for; this adds one that isn't built.
  await page.route(/\/api\/v1\/setup(\?.*)?$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const finding = { status: "fail", what: "Job image factory-job-node:latest (for acme/web) isn't built", fix: "", action: "build_images" };
    await route.fulfill({ response: res, json: { ...body, machine: [...body.machine, finding] } });
  });
  await page.goto("/app/get-started");
  const machine = await openStep(page, "machine");
  const row = machine.getByTestId("machine-finding").filter({ hasText: "isn't built" });
  await expect(row.getByRole("button", { name: /^(Build|Building…)$/ })).toBeVisible();
  await expect(row).not.toContainText("./factory");
});

test("the GitHub app is made in the browser, then installed and checked", async ({ page }, info) => {
  // It changes the shared sample setup, so one project checks it.
  test.skip(phone(info), "desktop only: it changes the sample setup");
  await page.goto("/app/get-started");
  const gh = step(page, "github");
  await expect(gh).toHaveAttribute("data-state", "attention");
  await expect(gh.getByRole("button", { name: "Create an app for me" })).toBeVisible();
  // The page posts the manifest to GitHub (the devserver's stand-in), which sends the browser back.
  await gh.getByRole("button", { name: "Create an app for me" }).click();
  await expect(page.getByRole("status").first()).toContainText("Created the app factory-api-ab12");
  await expect(page).toHaveURL(/\/app\/get-started$/);
  const install = gh.getByRole("region", { name: "Install the app" });
  await expect(install.getByRole("link", { name: /apps\/factory-api-ab12\/installations\/new/ })).toBeVisible();
  await expect(install).toContainText('Pick acme, choose "Only select repositories"');
  await install.getByRole("button", { name: "Check", exact: true }).click();
  await expect(gh.getByTestId("access")).toHaveCount(2);
  await expect(gh.getByTestId("access").first()).toContainText("the factory can reach it");
  // Reached, but one repo lacks two of the factory's labels.
  await expect(gh).toHaveAttribute("data-state", "attention");
  await expect(gh).toContainText("Labels missing on acme/web");
  await expect(gh.getByTestId("access").nth(1)).toContainText("Missing labels: factory:parked, factory:retry");

  await gh.getByRole("button", { name: "Create missing labels" }).click();
  await expect(gh).toHaveAttribute("data-state", "done");
  await expect(gh.getByText("acme/web factory:parked")).toBeVisible();
  await expect(gh.getByTestId("label").filter({ hasText: "acme/web factory:parked" })).toContainText("created");
  await expect(gh.getByTestId("label").first()).toContainText("already there");
  await expect(gh.getByRole("button", { name: "Create missing labels" })).toBeDisabled();
  await expect(step(page, "finish")).toHaveAttribute("data-state", "done");
  await shot(page, info, "get-started-github");
});

test("a used or unknown app link says to start again", async ({ page }) => {
  await page.goto("/api/v1/setup/github/callback?code=x&state=not-a-state");
  await expect(page.getByRole("alert").first()).toContainText("start again");
  await expect(page).toHaveURL(/\/app\/get-started$/);
});

test("job images build in the background with the log in view", async ({ page }, info) => {
  test.skip(phone(info), "desktop only: it changes the sample setup");
  await page.goto("/app/get-started");
  const images = step(page, "images");
  await openStep(page, "images");
  await expect(images.getByTestId("job-image")).toHaveCount(2);
  await images.getByRole("button", { name: "Build", exact: true }).click();
  await expect(images.getByTestId("build-log")).toContainText("Building factory-job-node:latest");
  await expect(images.getByTestId("build-log")).toContainText("Successfully tagged", { timeout: 10_000 });
  await expect(images).toHaveAttribute("data-state", "done", { timeout: 10_000 });
  await expect(images.getByText("The job images are built.")).toBeVisible();
});

test("in setup mode every screen goes to get-started, with no dashboard around it", async ({ page }, info) => {
  // The devserver runs in normal mode; this makes the page see setup mode.
  await page.route(/\/api\/v1\/setup(\?.*)?$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    await route.fulfill({ response: res, json: { ...body, mode: "setup" } });
  });
  await page.goto("/app/settings");
  await expect(page).toHaveURL(/\/app\/get-started$/);
  await expect(page.getByText("The factory needs a few things before it can start.")).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Main" })).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "Tabs" })).toHaveCount(0);
  await expect(step(page, "finish")).toContainText("Finish");
  if (phone(info)) await noSidewaysScroll(page);
});

test("after Finish, a factory that restarts but still can't start says why instead of waiting on", async ({ page }) => {
  // The devserver runs in normal mode; this plays setup mode and a restart into it.
  let finished = false;
  await page.route(/\/api\/v1\/setup(\?.*)?$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const after = finished ? { started_at: "a new process", setup_reason: "no GitHub credentials" } : {};
    await route.fulfill({ response: res, json: { ...body, mode: "setup", can_finish: true, ...after } });
  });
  await page.route(/\/api\/v1\/setup\/finish$/, async (route) => {
    const res = await route.fetch({ url: page.url().replace(/\/app\/.*/, "/api/v1/setup") , method: "GET", headers: {} });
    finished = true;
    await route.fulfill({ json: { message: "Starting the factory. This page opens the dashboard when it's up.", wizard: { ...(await res.json()), mode: "setup", can_finish: true } } });
  });
  await page.route(/\/api\/v1\/overview$/, (route) => route.fulfill({ status: 503, json: { error: "the factory isn't set up yet" } }));
  await page.goto("/app/get-started");
  const finish = await openStep(page, "finish");
  await finish.getByRole("button", { name: "Finish and start the factory" }).click();
  await expect(finish.getByRole("alert")).toContainText("The factory restarted but still can't start: no GitHub credentials.", { timeout: 15_000 });
});

test("GitHub's answer shows once, then leaves the address", async ({ page }) => {
  await page.goto("/app/get-started?github=" + encodeURIComponent("Created the app factory-x."));
  await expect(page.getByRole("status").first()).toContainText("Created the app factory-x.");
  await expect(page).toHaveURL(/\/app\/get-started$/);
});

test("settings and the setup screen link to get-started", async ({ page }) => {
  await page.goto("/app/settings");
  await page.getByRole("link", { name: "get-started page" }).click();
  await expect(page).toHaveURL(/\/app\/get-started$/);
  await page.goto("/app/setup");
  await expect(page.getByRole("link", { name: "get-started page" })).toHaveAttribute("href", "/app/get-started");
});

test("the checks step shows each repo's suggested checks, flags what its AGENTS.md disagrees with, and saves", async ({ page }, info) => {
  await page.goto("/app/get-started");
  const checks = await openStep(page, "checks");
  const api = checks.getByTestId("repo-checks").filter({ hasText: "acme/api" });
  await expect(api).toContainText("Looks like Ruby on Rails.");
  await expect(api.getByLabel("lint")).toHaveValue("bundle exec rubocop");
  if (phone(info)) return; // saving changes the shared devserver: once is enough
  await expect(checks).toHaveAttribute("data-state", "attention");
  await expect(api.getByLabel("test")).toHaveValue("bundle exec rspec");
  await expect(api).toContainText("AGENTS.md suggest");
  await api.getByRole("button", { name: "Use it" }).click();
  await expect(api.getByLabel("test")).toHaveValue("bin/rails db:prepare test");
  await api.getByRole("button", { name: "Save" }).click();
  await expect(api.getByRole("status")).toContainText("Saved acme/api's checks");
  const web = checks.getByTestId("repo-checks").filter({ hasText: "acme/web" });
  await expect(web).toContainText("Suggested, not saved yet.");
  await web.getByRole("button", { name: "Use these checks" }).click();
  await expect(checks).toHaveAttribute("data-state", "done");
});
