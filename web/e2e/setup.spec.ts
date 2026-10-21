import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

/** A notice by its name, opening the notice area's "more" when it's behind it. */
async function notice(page: Page, name: string) {
  const more = page.getByRole("button", { name: /more notices?$/ });
  await page.getByRole("group", { name: "Notices" }).waitFor();
  if (await more.isVisible()) await more.click();
  return page.getByRole("region", { name, exact: true });
}

test("while a repo isn't ready, a banner leads to its setup steps and the work on each blocker", async ({ page }, info) => {
  // On every screen but Home, which leads with setup itself.
  await page.goto("/app/line");
  const banner = await notice(page, "Setup");
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

  // Each blocker says what the factory is doing about it and links its
  // issue, unless the next step is on its row: then that's all it says.
  const lint = steps.nth(1).getByTestId("readiness-finding");
  await expect(lint).toContainText("lint fails on main");
  await lint.getByRole("button", { name: /Details/ }).click();
  await expect(lint).toContainText("ineffectual assignment");
  const flaky = steps.nth(2).getByTestId("readiness-finding");
  await expect(flaky).toContainText("Tests are flaky");
  await expect(flaky).toContainText("Waiting its turn");
  await expect(flaky.getByRole("link", { name: /#106/ })).toHaveAttribute("href", /acme\/api\/issues\/106$/);
  // One next step, and it's a button, never a command to run. Lint that
  // fails across the repo can't be fixed in pieces, so the baseline leads.
  const next = api.getByTestId("next-step");
  await expect(next).toContainText("Next: accept today's lint offenses");
  await expect(next).toContainText("can't fix that in pieces");
  await expect(api).not.toContainText("./factory");
  await expect(next.getByRole("button", { name: "Accept today's offenses" })).toBeVisible();
  await expect(api.getByRole("button", { name: "File and start work" })).toHaveCount(0);
  // It sits on the row it fixes; the top points there.
  await expect(lint.getByTestId("next-step")).toBeVisible();
  await expect(api.getByTestId("next-pointer")).toContainText("Next: accept today's lint offenses");
  await expect(api.getByTestId("next-pointer")).toContainText("in step 2");

  // What isn't needed to start is recommended, in plain view.
  const recommended = api.getByRole("region", { name: "Recommended" });
  await expect(recommended).toContainText("Recommended (2)");
  await expect(recommended).toContainText("No feature map");
  await expect(recommended).toContainText("Filed, not queued");
  await expect(recommended).toContainText("No issue yet");

  const web = page.getByRole("region", { name: "Setup of acme/web" });
  await expect(web).toContainText("3 of 3 setup steps done");
  await expect(web.getByTestId("next-step")).toContainText("Ready. Next: start the recommended work");
  await expect(web.getByTestId("next-step").getByRole("button", { name: "File and start work" })).toBeVisible();
  await shot(page, info, "setup");
  if (phone(info)) await noSidewaysScroll(page);

  // Check again runs in the background and the page follows it.
  await api.getByRole("button", { name: "Check again" }).click();
  // The other project may be running one too: one run at a time.
  await expect(api.getByRole("status").first()).toContainText(/[Cc]hecking/);
  await expect(api.getByRole("button", { name: "Check again" })).toBeVisible({ timeout: 15_000 });
  await expect(api).toContainText(/Checked \d+s ago/);
  // What it ran, and how each went, stays to look at.
  await api.getByText("What the last run did").click();
  await expect(api.getByRole("list", { name: "What it has done" })).toContainText("Passed bin/rails test in 1s");

  // File and start work checks again, then files; the page follows it. The
  // devserver changes nothing, so the other project sees the same report.
  if (!phone(info)) {
    // Retried, since the phone project's own check can hold the one slot.
    await expect(async () => {
      await web.getByRole("button", { name: "File and start work" }).click({ timeout: 5_000 });
      await expect(web.getByRole("status").first()).toContainText(/[Cc]hecking/);
      await expect(web.getByText("Opened 0 issue(s) on acme/web.")).toBeVisible({ timeout: 8_000 });
    }).toPass({ timeout: 40_000 });
  }

  // A blocker's work opens from its row.
  await flaky.getByRole("link", { name: "See the work" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Flaky TestCacheEviction on CI");
});

test("blockers the factory can fix lead with File and start work, which carries on to the recommended work", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.repos[0].can_baseline = false;
    // The other project's runs may hold the slot, or have made a baseline.
    body.repos[0].checking = false;
    body.repos[0].baseline_pr = "";
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/setup");
  const next = page.getByRole("region", { name: "Setup of acme/api" }).getByTestId("next-step");
  await expect(next).toContainText("Next: let the factory work through it");
  await expect(next).toContainText("starts on the 2 recommended items below by itself");
  await expect(next.getByRole("button", { name: "Only file the issues" })).toBeVisible();
  await expect(next.getByRole("button", { name: "File and start work" })).toBeVisible();
});

test("plan usage shows as percentages, and dollar costs say they're API-price estimates", async ({ page }, info) => {
  // On a desk, the sidebar shows it; on a phone, the Spend screen.
  await page.goto(phone(info) ? "/app/metrics" : "/app/");
  const usage = page.getByRole("group", { name: "Plan usage" }).filter({ visible: true });
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

test("without leave to file, the setup screen names no buttons it doesn't show", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    await route.fulfill({ response: res, json: { ...body, can_check: false, can_file: false, check_note: "Checking from here needs a dashboard password." } });
  });
  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  await expect(api).toContainText("No issue yet");
  await expect(api).not.toContainText("File issues");
  await expect(api).not.toContainText("Only file the issues");
  await expect(api).not.toContainText("File and start work");
  await expect(api.getByRole("button", { name: /File/ })).toHaveCount(0);
  await expect(api).toContainText("Checking from here needs a dashboard password.");
});

test("lint failing on main offers a baseline, and making it keeps its pull request on the page", async ({ page }) => {
  // The devserver keeps the pull request once either project makes it, so
  // until this one has, the page sees none.
  let made = false;
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    if (!made) for (const r of body.repos) r.baseline_pr = "";
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  const offer = api.getByTestId("next-step");
  await expect(offer).toContainText("Next: accept today's lint offenses");
  await expect(offer).toContainText(".rubocop_todo.yml");
  made = true;
  // Retried, since the other project's run can hold the one slot.
  await expect(async () => {
    await offer.getByRole("button", { name: "Accept today's offenses" }).click({ timeout: 5_000 });
    await expect(offer).toContainText("Next: open and merge the lint baseline's pull request", { timeout: 8_000 });
  }).toPass({ timeout: 40_000 });
  const pr = offer.getByRole("link", { name: "Open the pull request to merge it" });
  await expect(pr).toHaveAttribute("href", "https://github.com/acme/api/pull/120", { timeout: 10_000 });
  // Only a person can merge it, and the row says nothing else that competes.
  const lint = api.getByTestId("setup-step").nth(1).getByTestId("readiness-finding");
  await expect(lint).toContainText("Needs you");
  await expect(lint).not.toContainText("See the work");
  await expect(lint).not.toContainText("#113");
  await expect(lint.getByRole("button", { name: "Make it again" })).toHaveCount(0);
  await expect(offer).toContainText("The factory checks again by itself after you merge.");
});

test("a setup run in progress shows the same on every screen, with its time counting", async ({ page }) => {
  await page.route("**/api/v1/readiness", async (route) => {
    const r = await route.fetch();
    const body = await r.json();
    body.repos[0].checking = true;
    body.repos[0].action = "baseline";
    body.repos[0].checking_since = new Date(Date.now() - 90_000).toISOString();
    await route.fulfill({ response: r, json: body });
  });
  await page.goto("/app/line");
  const banner = await notice(page, "Setup");
  await expect(banner).toContainText(/Making the lint baseline for api, started 1m ago/);
  await expect(banner.getByRole("link", { name: "Watch it" })).toHaveAttribute("href", /\/app\/setup$/);
  // The line shows it as work under way, as it would a job.
  await expect(page.getByTestId("setup-run").filter({ visible: true })).toContainText("Making the lint baseline for api");

  // Home and Setup say the same: it's under way, and what happens next.
  await page.goto("/app/");
  const lead = page.getByTestId("setup-lead");
  await expect(lead).toContainText("working on it now");
  await expect(lead).toContainText("Making the lint baseline");
  await expect(lead).toContainText("opens a pull request for you to merge");
  await expect(lead.getByRole("button", { name: "Accept today's offenses" })).toHaveCount(0);
  await page.goto("/app/setup");
  const next = page.getByRole("region", { name: "Setup of acme/api" }).getByTestId("next-step");
  await expect(next).toContainText("Working on it: making the lint baseline");
  await expect(next).toContainText("Started 1m ago");
  // The lint row says so too, rather than what became of its issue.
  const lint = page.getByRole("region", { name: "Setup of acme/api" }).getByTestId("setup-step").nth(1).getByTestId("readiness-finding");
  await expect(lint.getByTestId("next-step")).toContainText("Working on it: making the lint baseline");
});

test("a token that can't open pull requests comes first, on Setup and Home, before any work", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const r = body.repos[0];
    r.checking = false;
    r.steps.unshift({
      key: "access",
      title: "The factory's token can do its job",
      why: "Every change ends with the factory opening a pull request.",
      state: "failing",
      passed: "",
      blockers: [
        {
          level: "blocker",
          area: "access",
          title: "The factory's token can't open pull requests",
          detail: "Give the token leave to:\n\n- open pull requests",
          issue: 0,
          issue_url: "",
          status: "none",
          split_done: 0,
          split_total: 0,
          item_id: null,
          item_stage: "",
        },
      ],
    });
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  const next = api.getByTestId("next-step");
  await expect(next).toContainText("Next: give the factory's token the permissions it needs");
  await expect(next).toContainText("Its token can't open pull requests");
  await expect(next.getByRole("button", { name: "Check again" })).toBeVisible();
  await expect(api.getByTestId("setup-step").first()).toContainText("1. The factory's token can do its job");
  await page.goto("/app/");
  await expect(page.getByTestId("setup-lead")).toContainText("Give the factory's token the permissions it needs");
});
