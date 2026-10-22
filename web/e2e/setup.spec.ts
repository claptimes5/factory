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

test("once a person's part is done, Setup and every screen say the factory is fixing the rest, as work on the line", async ({ page }, info) => {
  // On every screen, Home included: a person's part is done, so Home is
  // Home again, and the banner says what new tasks wait for.
  await page.goto("/app/line");
  const banner = await notice(page, "Setup");
  await expect(banner).toContainText("The factory is fixing 2 things that fail on api's main today; new tasks wait until they're merged.");
  await expect(banner.getByRole("link", { name: "See the work" })).toHaveAttribute("href", /\/app\/line$/);

  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  await expect(api).toContainText("Not ready yet");
  await expect(api).toContainText("1 of 1 setup steps done");
  const steps = api.getByTestId("setup-step");
  await expect(steps).toHaveCount(1);
  await expect(steps.first()).toContainText("The factory can install the repo and run its checks");
  await expect(steps.first().getByLabel("Done")).toBeVisible();

  // One next step: the person's part is done, and it says what's left.
  const next = api.getByTestId("next-step");
  await expect(next).toContainText("Your part is done. The factory is fixing 2 things first");
  await expect(next.getByRole("link", { name: "See them on the line" })).toHaveAttribute("href", /\/app\/line$/);
  await expect(api).not.toContainText("./factory");
  await expect(api.getByRole("button", { name: /File|Accept/ })).toHaveCount(0);

  // The factory's work: each blocker as an issue, with what it's doing about it.
  const work = api.getByRole("region", { name: "The factory's work" });
  await expect(work).toContainText("The factory is fixing these first (2)");
  const rows = work.getByTestId("readiness-finding");
  await expect(rows.nth(0)).toContainText("lint fails on main");
  await expect(rows.nth(0)).toContainText("Being built");
  await rows.nth(0).getByRole("button", { name: /Details/ }).click();
  await expect(rows.nth(0)).toContainText("ineffectual assignment");
  await expect(rows.nth(1)).toContainText("Tests are flaky");
  await expect(rows.nth(1)).toContainText("Waiting its turn");
  await expect(rows.nth(1).getByRole("link", { name: /#106/ })).toHaveAttribute("href", /acme\/api\/issues\/106$/);

  // What isn't needed to start is recommended, filed by the factory.
  const recommended = api.getByRole("region", { name: "Recommended" });
  await expect(recommended).toContainText("Recommended (2)");
  await expect(recommended).toContainText("Filed for later: the factory starts on it once its setup fixes are merged.");
  await expect(recommended).toContainText("No issue yet: the factory files one on its next check.");

  const web = page.getByRole("region", { name: "Setup of acme/web" });
  await expect(web.getByTestId("next-step")).toContainText("Ready: add the factory:ready label to issues to start");
  await shot(page, info, "setup");
  if (phone(info)) await noSidewaysScroll(page);

  // Check again runs in the background and the page follows it.
  await api.getByRole("button", { name: "Check again" }).click();
  await expect(api.getByRole("status").first()).toContainText(/[Cc]hecking/);
  await expect(api.getByRole("button", { name: "Check again" })).toBeVisible({ timeout: 15_000 });
  await expect(api).toContainText(/Checked \d+s ago/);
  // What it ran, and how each went, stays to look at.
  await api.getByText("What the last run did").click();
  await expect(api.getByRole("list", { name: "What it has done" })).toContainText("Passed bin/rails test in 1s");

  // The work opens from its row.
  await rows.nth(1).getByRole("link", { name: "See the work" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Flaky TestCacheEviction on CI");
});

/** acme/api with a blocker only a person can fix, in its run step. */
async function serviceMissing(page: Page) {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const r = body.repos[0];
    r.checking = false;
    r.done = 0;
    r.steps[0].state = "failing";
    r.steps[0].blockers = [
      {
        key: "",
        level: "blocker",
        area: "services",
        title: "`test` can't reach a service it needs",
        detail: "Add postgres under services in factory.yaml.",
        issue: 0,
        issue_url: "",
        status: "none",
        split_done: 0,
        split_total: 0,
        item_id: null,
        item_stage: "",
      },
    ];
    await route.fulfill({ response: res, json: body });
  });
}

test("what only a person can fix comes first, on its step and on Home", async ({ page }) => {
  await serviceMissing(page);
  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  const step = api.getByTestId("setup-step").first();
  await expect(step.getByTestId("next-step")).toContainText("Next: test can't reach a service it needs");
  await expect(step.getByTestId("next-step")).toContainText("Needs you");
  await expect(api.getByTestId("next-pointer")).toContainText("in step 1");
  await page.goto("/app/");
  await expect(page.getByTestId("setup-lead")).toContainText("Test can't reach a service it needs");
});

test("the factory's fix waiting for review is the next step, on its row", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const lint = body.repos[0].work[0];
    body.repos[0].checking = false;
    lint.status = "needs_you";
    lint.item_stage = "awaiting_human";
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/setup");
  const api = page.getByRole("region", { name: "Setup of acme/api" });
  const lint = api.getByRole("region", { name: "The factory's work" }).getByTestId("readiness-finding").first();
  const next = lint.getByTestId("next-step");
  await expect(next).toContainText("Next: review the factory's fix: lint fails on main");
  await expect(next).toContainText("Needs you");
  await expect(next.getByRole("link", { name: "Review it" })).toHaveAttribute("href", /\/app\/items\/\d+$/);
  // The row says nothing else that competes.
  await expect(lint).not.toContainText("#113");
  await expect(api.getByTestId("next-pointer")).toContainText("in the factory's work");
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
  await expect(note).toContainText("Claude runs on your Claude subscription, so its jobs cost no dollars here");
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

test("a setup run in progress shows the same on every screen, with its time counting", async ({ page }) => {
  await page.route("**/api/v1/readiness", async (route) => {
    const r = await route.fetch();
    const body = await r.json();
    body.repos[0].checking = true;
    body.repos[0].action = "check";
    body.repos[0].checking_since = new Date(Date.now() - 90_000).toISOString();
    body.repos[0].checking_why = "Started by itself: main moved from 1a2b3c4 to 5d6e7f8.";
    await route.fulfill({ response: r, json: body });
  });
  await page.goto("/app/line");
  const banner = await notice(page, "Setup");
  await expect(banner).toContainText(/Checking api's main, started 1m ago\. Why check again: main moved from 1a2b3c4 to 5d6e7f8\./);
  await expect(banner.getByRole("link", { name: "Watch it" })).toHaveAttribute("href", /\/app\/setup$/);
  // The line shows it as work under way, as it would a job.
  await expect(page.getByTestId("setup-run").filter({ visible: true })).toContainText("Checking api's main");

  await page.goto("/app/setup");
  const next = page.getByRole("region", { name: "Setup of acme/api" }).getByTestId("next-step");
  await expect(next).toContainText("Working on it: checking api's main");
  await expect(next).toContainText("Started 1m ago");
  await expect(next).toContainText("Why check again: main moved from 1a2b3c4 to 5d6e7f8.");
});

test("while a repo's job image builds, every screen says so and why, and its check says it waits for it", async ({ page }) => {
  await page.route("**/api/v1/readiness", async (route) => {
    const r = await route.fetch();
    const body = await r.json();
    Object.assign(body.repos[0], {
      checking: true,
      check_waits: "its job image to build",
      checking_since: new Date(Date.now() - 120_000).toISOString(),
      checking_why: "Started by itself: main moved from 1a2b3c4 to 5d6e7f8.",
      image: { why: "factory.yaml changed what it's built from", since: new Date(Date.now() - 90_000).toISOString(), step: "[test 3/5] COPY --from=build /app /app" },
    });
    await route.fulfill({ response: r, json: body });
  });
  await page.goto("/app/line");
  const banner = await notice(page, "Setup");
  await expect(banner).toContainText("Building api's job image, started 1m ago. Because factory.yaml changed what it's built from. Its jobs wait for it.");
  await expect(banner).toContainText("Then it checks main in it. Why check again: main moved from 1a2b3c4 to 5d6e7f8.");
  await expect(banner).toContainText("Now at [test 3/5] COPY --from=build /app /app");
  await expect(page.getByTestId("setup-run").filter({ visible: true })).toContainText("Building api's job image");
  await page.goto("/app/setup");
  await expect(page.getByRole("region", { name: "Setup of acme/api" }).getByRole("status").first()).toContainText("Building its image");
});

test("a token that can't open pull requests comes first, on Setup and Home, before any work", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const r = body.repos[0];
    r.checking = false;
    r.total += 1;
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

test("on subscription plans only, no dollars show: plan usage instead", async ({ page }, info) => {
  await page.route(/\/api\/v1\/overview$/, async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, json: { ...(await res.json()), plans_only: true } });
  });
  await page.goto("/app/metrics");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Usage and metrics");
  await expect(page.getByText("Cost per merged PR")).toHaveCount(0);
  await expect(page.getByText("Daily spend (30 days)")).toHaveCount(0);
  await expect(page.getByText("Spend by model (all time)")).toHaveCount(0);
  if (!phone(info)) {
    const main = page.getByRole("navigation", { name: "Main" });
    await expect(main.getByRole("link", { name: "Usage" })).toBeVisible();
    await expect(page.getByTitle("Spent today (UTC) of the daily cap")).toHaveCount(0);
    await expect(page.getByRole("group", { name: "Plan usage" }).filter({ visible: true }).first()).toContainText("5-hour: 42% used");
  }
  await page.goto("/app/items/1");
  await expect(page.getByRole("columnheader", { name: "Cost" })).toHaveCount(0);
});
