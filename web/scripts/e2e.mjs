// Runs Playwright against a devserver started first. Before it starts its
// own, Playwright connects to the port to see whether one is running, and
// where a closed port hangs instead of refusing (WSL2's mirrored
// networking) that check waits over two minutes. A running server answers
// it at once, so this starts one, unless one already answers, and stops
// it after.
//
// It times the run too, and a whole-suite run that takes far longer than
// usual fails, so a stall like that one shows up in CI instead of only on
// the machine it happens on.
import { spawn } from "node:child_process";

// The whole suite took about 100s in CI and 70-90s on a laptop (Oct 2026,
// 146 tests). A run past this budget is a regression to find, not noise:
// the stall above added 140s. Raise it, with the new numbers here, when
// the suite grows for real.
const budgetSeconds = 180;
const secs = (since) => Math.round((performance.now() - since) / 1000);

const port = 8099; // playwright.config.ts's
const url = `http://127.0.0.1:${port}/api/v1/overview`;
const up = () => fetch(url, { signal: AbortSignal.timeout(1000) }).then((r) => r.ok, () => false);

const started = performance.now();
let server;
if (!(await up())) {
  // Its own process group, so stopping it stops the binary go run starts.
  server = spawn("go", ["run", "../internal/web/devserver", "-listen", `127.0.0.1:${port}`], { stdio: "inherit", detached: true });
  const deadline = Date.now() + 120_000;
  while (!(await up())) {
    if (server.exitCode !== null || Date.now() > deadline) {
      console.error("e2e: the devserver didn't start");
      process.exit(1);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
}
const stop = () => server && process.kill(-server.pid);
const args = process.argv.slice(2);
const ran = performance.now();
const pw = spawn("npx", ["playwright", "test", ...args], { stdio: "inherit", env: { ...process.env, FACTORY_E2E_SERVER: "1" } });
pw.on("exit", (code) => {
  stop();
  const took = secs(ran);
  // Only the whole suite has a usual time to hold it to.
  const whole = args.length === 0;
  console.log(`e2e: devserver up in ${Math.round((ran - started) / 1000)}s; Playwright took ${took}s${whole ? ` (budget ${budgetSeconds}s)` : ""}`);
  if (whole && code === 0 && took > budgetSeconds) {
    console.error(
      `e2e: the suite took ${took}s, over its ${budgetSeconds}s budget. Something made it slower: a check that hangs before the tests, ` +
        `a test that waits out timeouts, a much bigger suite. Find it before raising budgetSeconds in web/scripts/e2e.mjs.`,
    );
    process.exit(1);
  }
  process.exit(code ?? 1);
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => pw.kill(sig));
