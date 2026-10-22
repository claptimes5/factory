// Runs Playwright against a devserver started first. Before it starts its
// own, Playwright connects to the port to see whether one is running, and
// where a closed port hangs instead of refusing (WSL2's mirrored
// networking) that check waits over two minutes. A running server answers
// it at once, so this starts one, unless one already answers, and stops
// it after.
import { spawn } from "node:child_process";

const port = 8099; // playwright.config.ts's
const url = `http://127.0.0.1:${port}/api/v1/overview`;
const up = () => fetch(url, { signal: AbortSignal.timeout(1000) }).then((r) => r.ok, () => false);

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
const pw = spawn("npx", ["playwright", "test", ...process.argv.slice(2)], { stdio: "inherit", env: { ...process.env, FACTORY_E2E_SERVER: "1" } });
pw.on("exit", (code) => {
  stop();
  process.exit(code ?? 1);
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => pw.kill(sig));
