import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

/** Every repo set up, so Home shows its usual flow and nothing waits for setup. */
async function allSetUp(page: Page) {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const r of body.repos) r.state = "ready";
    body.banner = "";
    await route.fulfill({ response: res, json: body });
  });
  await page.route(/\/api\/v1\/board$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const col of body.columns) for (const c of col.cards) c.setup = c.setup_note = "";
    await route.fulfill({ response: res, json: body });
  });
}

/** A service only a person can add to factory.yaml: Home leads with setup and lists the rest under it. */
async function personsPartLeft(page: Page) {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const r = body.repos[0];
    r.checking = false;
    r.done = 0;
    r.steps[0].state = "failing";
    r.steps[0].blockers = [
      { key: "", level: "blocker", area: "services", title: "`test` can't reach a service it needs", detail: "Add postgres under services in factory.yaml.",
        issue: 0, issue_url: "", status: "none", split_done: 0, split_total: 0, item_id: null, item_stage: "" },
    ];
    await route.fulfill({ response: res, json: body });
  });
}

test("a check that couldn't finish says which command stopped it and its error", async ({ page }) => {
  await page.route(/\/api\/v1\/readiness$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    const r = body.repos[0];
    Object.assign(r, { state: "incomplete", checking: false, done: 0 });
    r.steps[0].state = "failing";
    r.steps[0].blockers = [
      { key: "", level: "blocker", area: "checks", title: "Couldn't run `test`",
        detail: "This is a problem with the factory's setup.\n\n```\nCreated database\n\u001b[0mERROR FooTest#test_1\nMinitest::UnexpectedError:   ActiveRecord::DatabaseConnectionError: There is an issue connecting with your hostname: db.\n```",
        issue: 0, issue_url: "", status: "none", split_done: 0, split_total: 0, item_id: null, item_stage: "" },
    ];
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/");
  const card = page.getByTestId("setup-lead");
  await expect(card).toContainText("Couldn't run test: fix that, then check again");
  await expect(card).toContainText("It failed with: ActiveRecord::DatabaseConnectionError: There is an issue connecting with your hostname: db.");
  await expect(card.getByRole("link", { name: "See the whole error" })).toBeVisible();
});

test("while a person's part of setup is left, Home leads with it and ranks the rest by what blocks it", async ({ page }, info) => {
  await personsPartLeft(page);
  await page.goto("/app/");
  const lead = page.getByRole("region", { name: "Setup", exact: true });
  await expect(lead.getByRole("heading", { level: 1 })).toHaveText("Finish setting up api");
  const card = lead.getByTestId("setup-lead");
  await expect(card).toContainText("Setup · needs you · new tasks wait until it's done");
  await expect(card).toContainText("Test can't reach a service it needs");
  await expect(card.getByRole("button", { name: "Check again" })).toBeVisible();
  await expect(card.getByRole("link", { name: "See every step" })).toHaveAttribute("href", /\/app\/setup$/);
  // One lead: no setup banner repeating it, and the decisions are a list under it.
  await expect(page.getByRole("group", { name: "Notices" }).getByRole("region", { name: "Setup", exact: true })).toHaveCount(0);
  const needs = page.getByRole("region", { name: "Needs you" });
  await expect(needs.getByRole("heading", { level: 2 })).toHaveText("8 things need you");
  await expect(needs.getByTestId("decision")).toHaveCount(0);
  const rows = needs.getByRole("listitem");
  await expect(rows.first()).toContainText("Parser drops the last token");
  await expect(rows.last()).toContainText("Waits for setup");
  await expect(needs).toContainText("it starts by itself then");
  await shot(page, info, "home-setup");
  if (phone(info)) {
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  }
});

test("Home shows what needs you one decision at a time, reviews first, with the rest next", async ({ page }, info) => {
  await allSetUp(page);
  await page.goto("/app/");
  const needs = page.getByRole("region", { name: "Needs you" });
  await expect(needs.getByRole("heading", { level: 1 })).toHaveText("8 things need you");
  const top = needs.getByTestId("decision");
  await expect(top).toHaveCount(1);
  await expect(top).toContainText("Review · web#101");
  await expect(top).toContainText("Parser drops the last token without a trailing newline");
  await expect(top.getByRole("link", { name: /Review the change/ })).toHaveAttribute("href", /\/pull\/88$/);
  await expect(needs.getByRole("list", { name: "After this" })).toContainText("Next: review Show order totals on the checkout page");
  // Later puts it at the back: the next one comes up.
  await top.getByRole("button", { name: "Later" }).click();
  await expect(top).toContainText("Show order totals on the checkout page");
  await expect(top.getByRole("link", { name: /1\. Review api#91/ })).toBeVisible();
  // The line, with you as its last station.
  const line = page.getByRole("region", { name: "The line" });
  await expect(line.getByRole("listitem", { name: "You: 8" })).toBeVisible();
  await line.getByRole("link", { name: "See the whole line" }).click();
  await expect(page).toHaveURL(/\/app\/line$/);
  await expect(page.getByTestId("board")).toBeVisible();
  await page.goBack();
  await shot(page, info, "home");
  if (phone(info)) {
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  }
});

test("Home says what went live since you looked, until you dismiss it", async ({ page }) => {
  let dismissed = false;
  await page.route(/\/api\/v1\/home$/, async (route) => {
    const went_live = dismissed ? [] : [{ id: 1, title: "Fix flaky tests", ref: "api#106" }, { id: 2, title: "Start a feature map", ref: "web#7" }];
    await route.fulfill({ json: { seen_at: "2026-10-20T08:00:00Z", went_live } });
  });
  await page.route(/\/api\/v1\/home\/seen$/, async (route) => {
    dismissed = true;
    await route.fulfill({ json: { seen_at: new Date().toISOString(), went_live: [] } });
  });
  await page.goto("/app/");
  const since = page.getByRole("region", { name: "Since you looked" });
  await expect(since).toContainText("2 changes went live");
  await expect(since).toContainText("Fix flaky tests, Start a feature map");
  await since.getByRole("button", { name: "Dismiss" }).click();
  await expect(since).toHaveCount(0);
});

test("work the factory found and started itself says so, on Home and the line", async ({ page }) => {
  await allSetUp(page);
  await page.route(/\/api\/v1\/board$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const col of body.columns) for (const c of col.cards) if (c.title.startsWith("Parser drops")) c.self_started = "dependabot";
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/");
  await expect(page.getByTestId("decision")).toContainText("Found by the factory · Dependabot");
  await page.goto("/app/line");
  const card = page.getByRole("region", { name: "Needs you" }).getByTestId("card").filter({ hasText: "Parser drops" }).filter({ visible: true });
  await expect(card.getByTestId("found-by")).toHaveText("Found by the factory · Dependabot");
});

test("Home's Ask box opens Ask", async ({ page }) => {
  await page.goto("/app/");
  await page.getByRole("button", { name: "Ask, or hand over a task…" }).click();
  await expect(page.getByTestId("ask").filter({ visible: true }).getByLabel("Message")).toBeVisible();
});

test("Home numbers what needs you in order: what other work waits for first, what waits for the base branch last, saying why", async ({ page }) => {
  await personsPartLeft(page);
  await page.route(/\/api\/v1\/board$/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const col of body.columns)
      for (const c of col.cards) {
        c.setup = "";
        if (c.title.startsWith("Show order totals")) {
          c.setup = "base";
          c.setup_note = "Waits for main: it failed `lint`, which fails on main too, so no change passes until that's fixed there. Retry it then.";
        }
      }
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/app/");
  const needs = page.getByRole("region", { name: "Needs you" });
  await expect(needs).toContainText(
    "In this order: changes to review, then what other work waits for, then the rest, then what waits for setup or the base branch.",
  );
  const rows = needs.getByRole("listitem");
  await expect(rows.first()).toContainText("1.");
  await expect(rows.first()).toContainText("Parser drops the last token");
  // A review that waits for the base branch drops to the back, and says why.
  await expect(rows.last()).toContainText("Show order totals");
  await expect(rows.last()).toContainText("Waits for the base branch");
  await expect(rows.last()).toContainText("it failed lint, which fails on main too");
  // 104 holds up the Redis move: right after the review, ahead of plans and costs, and it says why.
  await expect(rows.nth(1)).toContainText("2.");
  await expect(rows.nth(1)).toContainText("Support YAML anchors");
  await expect(rows.nth(1)).toContainText("Do first: api#108 waits for it");
});
