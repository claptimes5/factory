import { expect, test, type Page, type TestInfo } from "@playwright/test";

const phone = (info: TestInfo) => info.project.name === "phone";

async function shot(page: Page, info: TestInfo, name: string) {
  await info.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

// When a check fails, say what the page and the API showed.
test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  const id = new URL(page.url()).searchParams.get("chat") ?? "1";
  const api = await page.request.get(`/api/v1/chats/${id}`);
  console.log(`page ${page.url()}:\n${await page.locator("main").innerText({ timeout: 1000 }).catch((e) => String(e))}`);
  console.log(`GET /api/v1/chats/${id} = ${api.status()} ${(await api.text()).slice(0, 2000)}`);
});

test("the orchestrator tab opens on a chat that explains itself and shows what it drafted", async ({ page }, info) => {
  await page.goto("/app/orchestrator");
  await expect(page.getByRole("tab", { name: "Chat" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("Nothing is filed until you press")).toBeVisible();
  await expect(page.getByText("The orchestrator doesn't run on its own.")).toBeVisible();
  await expect(page.getByText("$1.84 of today's $10.00")).toBeVisible();
  await page.getByTestId("chat-list").getByRole("button", { name: /Split the CSV export/ }).click();
  await expect(page).toHaveURL(/chat=\d+/);
  const convo = page.getByTestId("conversation");
  await expect(convo.getByTestId("you")).toHaveCount(2);
  await expect(convo.getByTestId("reply").first()).toContainText("billing/export.go");
  const drafts = page.getByTestId("draft");
  await expect(drafts).toHaveCount(2);
  await expect(drafts.first()).toContainText("Write invoices as CSV");
  await expect(drafts.first()).toContainText("billing");
  await expect(drafts.first().getByRole("button", { name: "Create issue" })).toBeVisible();
  await expect(page.getByTestId("draft-dismissed")).toContainText("Export invoices as PDF");
  await shot(page, info, "chat");
  if (phone(info)) await noSidewaysScroll(page);

  await page.getByRole("tab", { name: "Decisions" }).click();
  await expect(page).toHaveURL(/tab=decisions/);
  await expect(page.getByTestId("feed")).toBeVisible();
});

test("sending a message starts a chat; its draft can be edited and created", async ({ page }, info) => {
  await page.goto("/app/orchestrator");
  const ask = `Add rate limits to the public API (${info.project.name})`;
  await page.getByLabel("Message").fill(ask);
  await page.getByLabel("Message").press("Enter");
  await expect(page).toHaveURL(/chat=\d+/);
  const convo = page.getByTestId("conversation");
  await expect(convo.getByTestId("you")).toContainText(ask);
  await expect(convo.getByTestId("reply")).toContainText("sample draft");
  await expect(page.getByLabel("Message")).toHaveValue("");
  await expect(page.getByTestId("chat-list")).toContainText(ask.slice(0, 20));

  const draft = page.getByTestId("draft");
  await expect(draft).toHaveCount(1);
  await draft.getByRole("button", { name: "Edit" }).click();
  await draft.getByLabel("Title").fill(`Rate-limit the public API (${info.project.name})`);
  await draft.getByRole("button", { name: "Create issue" }).click();
  await expect(draft).toContainText("Created");
  await expect(draft).toContainText(`Rate-limit the public API (${info.project.name})`);
  await expect(draft).toContainText(/Issue #3\d\d/);
  await expect(draft.getByRole("link", { name: "Follow it on the board" })).toHaveAttribute("href", "/app/items/1");
  await expect(draft.getByRole("button", { name: "Create issue" })).toHaveCount(0);
  await shot(page, info, "chat-created");
  // However many chats there are, the list scrolls on its own.
  if (phone(info)) await noSidewaysScroll(page);
});

test("a draft the forge refuses says why and stays a draft", async ({ page }, info) => {
  await page.goto("/app/orchestrator");
  await page.getByLabel("Message").fill(`Please refuse this one (${info.project.name})`);
  await page.getByRole("button", { name: "Send" }).click();
  const draft = page.getByTestId("draft");
  await expect(draft).toHaveCount(1);
  await draft.getByRole("button", { name: "Create issue" }).click();
  await expect(draft.getByRole("alert")).toContainText("GitHub refused the issue");
  await expect(draft.getByRole("button", { name: "Create issue" })).toBeVisible();
  await draft.getByRole("button", { name: "Dismiss" }).click();
  await expect(page.getByTestId("draft-dismissed")).toHaveCount(1);
});

test("a refused message says why and keeps what you wrote", async ({ page }) => {
  await page.goto("/app/orchestrator");
  await page.getByLabel("Message").fill("are you busy?");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByRole("alert")).toContainText("another chat reply is still being written");
  await expect(page.getByLabel("Message")).toHaveValue("are you busy?");
});

test("a failed reply says why and can be tried again", async ({ page }, info) => {
  // It changes the shared sample chat, so one project checks it.
  test.skip(phone(info), "desktop only: it changes the sample chat");
  await page.goto("/app/orchestrator");
  await page.getByTestId("chat-list").getByRole("button", { name: /login fix fail twice/ }).click();
  const failed = page.getByTestId("reply-failed");
  await expect(failed).toContainText("The reply failed: claude exited with code 1: overloaded.");
  await expect(failed.getByRole("link", { name: "See what happened" })).toHaveAttribute("href", /\/app\/jobs\/\d+$/);
  await failed.getByRole("button", { name: "Try again" }).click();
  await expect(page.getByTestId("reply").last()).toContainText("your last message");
  await expect(failed).toHaveCount(0);
});
