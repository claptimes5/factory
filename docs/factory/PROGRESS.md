# Factory progress

Source of truth for where the factory build stands. Updated at the end of every step (see `PLAYBOOK.md`, Appendix A). Statuses: `not started`, `in progress`, `blocked`, `passed`, `skipped`. Put your name in Owner when you begin a step; do not start a step someone else owns.

**Lock level: 1** (since 2026-11-04; see PLAYBOOK.md, "Lock levels"). Rollout mode A.

Core path: 0 to 10 and 12. Autonomy track: 13 to 15, after the core path has carried real work. Optional: 5b, 7b, 11, 16.

| Step | Name | Status | Owner | Date | Gate command | Evidence |
|------|------|--------|-------|------|--------------|----------|
| 0 | Orient and baseline | passed | Avi | 2026-11-04 | commands in BASELINE.md; GitHub plan recorded | [BASELINE.md](BASELINE.md), top-up in log |
| 1 | One-command local verification | passed | Avi | 2026-11-04 | `scripts/verify.sh`; install cooldown checked with npm 11.19 | fresh clone at 0e2c81e, 2 runs; `web/.npmrc`, top-up in log |
| 2 | Lean CLAUDE.md | passed | Avi | 2026-11-03 | fresh-session task, verify from CLAUDE.md only | 3 fresh-agent runs, see log |
| 3 | Deterministic guardrails (hooks, permissions) | passed | Avi | 2026-11-04 | `python3 .claude/hooks/guard_test.py`; Level 1 live demos | Level 1 gate in log; Level 3 parts done early and parked |
| 4 | Required CI mirrors local verify | passed | Avi | 2026-11-04 | throwaway PR, clean PR, direct push | #124, #125, top-up in log |
| 5 | Characterization + critical-path tests | passed | Avi | 2026-11-04 | `docs/factory/seeds.py`; new tests 5x local and 5x CI | #123, see log |
| 5b | Property tests and fuzzing (optional) | skipped | Avi | 2026-11-04 | | Step 5's seeds caught 23 of 24 with example tests; revisit if a parser or matcher bug gets past them |
| 6 | Browser E2E + web quality gates | passed | Avi | 2026-11-04 | `docs/factory/ui-seeds.py`; 5 CI attempts on e277ee7; throwaway red run | #129, #131, see log |
| 7 | Agent self-verification protocol | passed | Avi | 2026-11-04 | two sample tasks through `verify-factory` and the `verifier` agent | see log |
| 7b | Formal verification of the riskiest property (optional) | not started | | | | trigger (a) may apply: the pipeline's retry and park states; decide after Step 7 |
| 8 | AI code review + security review | not started | | | | |
| 9 | Risk-tiered approval and ownership | not started | | | | |
| 10 | Safe ship and rollback | not started | | | | |
| 11 | First automations: chores (optional) | skipped | Avi | 2026-11-04 | | sole maintainer, no recurring chore worth automating yet |
| 12 | First feature through the full loop | not started | | | | |
| 13 | Graduate to poteto-mode (autonomy by exception) | not started | | | | |
| 14 | Measure and improve (factory as code) | not started | | | | |
| 15 | Make poteto-mode the default (optional until 14 is stable) | not started | | | | |
| 16 | Scale carefully (optional) | skipped | Avi | 2026-11-04 | | not before Steps 12 to 14 are solid; reopen then |

## Lock log

<!-- One line per time a lock got in the way, was worked around, or should have fired and didn't: what happened, which control, the fix. Move up a level only when every item is fixed or accepted. -->

| Date | What happened | Control | Fix | State |
|------|---------------|---------|-----|-------|
| 2026-11-04 | Every edit to a factory file needed a person to restart Claude Code with FACTORY_CONFIG=1, and the sandbox blocked Bash writes under .claude/ | guard.py harness block, Bash sandbox (both Level 3 controls, switched on at Step 3) | Back to Level 1: the guard drops the factory files, settings.json drops the sandbox | fixed |
| 2026-11-04 | Every commit from a desktop-app session was refused: the guard runs verify with Claude Code's PATH, where the Windows npm comes first | guard.py verify-before-commit | verify switches to asdf's Linux Node when the npm on the PATH is the Windows one | fixed |
| 2026-11-04 | A stray ~/.git folder (no HEAD) made the guard treat the home directory as a repo and refuse writes under ~/.claude | guard.py repo_root | A .git directory counts only with a HEAD (#125) | fixed |
| 2026-11-04 | The guard reads a heredoc's text as commands, so a script whose text mentions "> some/protected/path" is treated as writing there; a heredoc mentioning "git commit" after another command is refused as "run git commit as its own command" | guard.py segments | None yet; append with the Edit tool instead. Fix if it keeps biting | accepted |
| 2026-11-04 | `cp -r staging/. worktree/` wrote .claude/hooks/guard.py into a branch worktree while the guard still refused factory files: cp names a directory as its target, and the guard checks only the target it is given | guard.py check_writes | None needed at Level 1 (nothing there is protected now); at Level 3 the sandbox and autonomy.json Edit denies stop it | accepted |
| 2026-11-04 | In the sandbox, the job bridge's tests couldn't open Unix sockets | Bash sandbox | allowAllUnixSockets plus denyRead on the Docker and WSL sockets (#126). autonomy.json doesn't carry these yet | open for Level 3 |
| 2026-11-04 | In the sandbox, git push fails through the token mask (the stand-in isn't swapped inside git's Basic header); gh works | Bash sandbox credentials mask | Give pushes the bot identity's own repo-scoped token (Step 9) | open for Level 3 |
| 2026-11-04 | In the sandbox, git status lists the /dev/null mounts over denied paths as untracked files | Bash sandbox | Add files by name in sandboxed sessions | open for Level 3 |

## Log
<!-- Newest last. One entry per step using the Appendix A template. -->

### 2026-11-03
```
STEP 0: Orient and baseline   STATUS: passed
Gate command(s): go vet ./...; go build ./...; go test -race ./...;
  cd web && npm ci && npm run build && npm run e2e; scripts/check-web-built.sh
Result: all pass (vet 6s, build 2s, test -race 111s, npm ci 8s, web build 7s,
  e2e 166 passed / 4 skipped in 46s). golangci-lint not run: not installed locally.
Negative control: n/a (read-only step)
Flagged changes: none
Decisions made: protected paths = AGENTS.md "Take care with" + CODEOWNERS +
  deploy/ + .claude/ (confirmed); critical flows picked by the agent (maintainer
  delegated); no running instance, previews and rollback are local with Docker;
  develop docs/factory/ in public; pull the Docker test images in CI (Step 4).
Open questions for human: none
Next step proposed: 1
```

### 2026-11-03
```
STEP 1: One-command local verification   STATUS: passed
Gate command(s): git clone . <scratch> && scripts/verify.sh (twice); scripts/verify.sh --quick
Result: fresh clone at 0e2c81e: run 1 pass in 156s (cold Go test cache), run 2 pass in
  78s (warm); Playwright 166 passed, 4 skipped both times. --quick: pass in 61s cold.
Negative control (each planted in the scratch clone, verify red, reverted: yes):
  Go type error -> go vet; TS type error -> dashboard build; errcheck violation ->
  golangci-lint; flipped assertion in pathglob_test.go -> go test -race; Go import of a
  missing module -> go vet; web import of a missing file -> vite build; unformatted new
  Go file -> first MISSED (gofmt saw tracked files only), fixed in 0e2c81e, now red.
Flagged changes: none (no test, threshold or CI change). gofmt -w on cmd/factory/images.go
  (whitespace only; main had drifted because nothing checked gofmt).
Decisions made: script is scripts/verify.sh (repo has scripts/, no Makefile); it fails if
  golangci-lint isn't the version ci.yml pins; installed golangci-lint v2.14.0 locally
  (asdf reshim golang needed); no .nvmrc, package.json engines and CI's node 24 cover it.
Flaky: e2e/notify.spec.ts (phone) failed once in 4 full runs; quarantined visibly in
  BASELINE.md, fix in Step 6.
Open questions for human: none
Next step proposed: 2
```

### 2026-11-03
```
STEP 2: Lean CLAUDE.md   STATUS: passed
Gate command(s): a fresh agent in a fresh clone, told to learn verification only from
  CLAUDE.md (-> AGENTS.md): "add one sentence to Match's doc comment and prove the repo
  still verifies". Default PATH.
Result: run 3 (at 9eac808) found and ran scripts/verify.sh, was stopped by verify's
  Windows-npm check, followed it, verify passed (166 passed, 4 skipped). No questions
  for a human. Its one gap (where a Linux Node lives) is now printed by verify via asdf.
Negative control: run 1 used the old AGENTS.md (worktree from main, by mistake): it ran
  only the Go checks and skipped the dashboard, unsure what the full gate was. Run 2
  (new AGENTS.md) failed on the Windows npm that `which npm` hid. Both gaps fixed.
Flagged changes: none
Decisions made: CLAUDE.md stays an @AGENTS.md import (Factory's own builders read
  AGENTS.md); AGENTS.md now 82 lines. The npm trap became a check in verify, not prose.
  A truly separate `claude -p` session was blocked by the auto-mode safety check; the
  gate used subagents told to ignore inherited context.
Open questions for human: none
Next step proposed: 3
```

### 2026-11-03
```
STEP 3: Deterministic guardrails   STATUS: passed
Gate command(s): live, in this session, with .claude/settings.json installed:
  git push --force nowhere-remote HEAD; git add <planted errcheck file> && git commit;
  scripts/verify.sh --quick; Edit of .claude/hooks/guard.py.
  Plus python3 .claude/hooks/guard_test.py (16 tests, in verify).
Result: force push refused by the hook; the commit refused with golangci-lint's
  errcheck finding; verify --quick ran, no prompt (auto mode, so this doesn't prove
  the allowlist); the protected edit got "ask", which auto mode's classifier
  approved: no person saw it. The maintainer chose to keep "ask" (see decisions).
Negative control: 6 breaks of guard.py on a copy each turned its tests red. The live
  demo found two holes, both fixed with tests: (1) the hook found guard.py from
  $CLAUDE_PROJECT_DIR, the main checkout, so in a worktree it ran nothing; it now
  follows the event's cwd. (2) `printf > bad.go && git add && git commit` passed,
  since the hook runs before the whole command; a commit must now be its own
  command (cd and git add before it are fine).
Flagged changes: verify.sh gains the hooks' tests and a tool check that installs
  golangci-lint and Playwright's Chromium at the pinned versions (stricter, not looser).
Decisions made: one stdlib Python script (no jq on the second workstation); protected
  edits ask, and in auto mode the classifier answers (maintainer's choice over a
  refuse-and-unlock file); shell writes to protected paths are caught best effort
  (redirects, sed -i, cp, mv, rm, tee), with review and CODEOWNERS behind them; crashes
  refuse; settings deny reading factory.env, factory.yaml and *.pem; the agent may not
  write .claude/settings.json (auto mode refused twice), so the maintainer installs it,
  in the main checkout too, since Claude Code reads settings from there. Factory's
  builders run with --setting-sources user and never load these hooks.
Second workstation: golangci-lint was missing for asdf's Go 1.27.3 (now verify installs
  it); Node 22.13. Full verify: 339s cold, then 122s with one new desktop flake
  (get-started.spec.ts:299, now in BASELINE), then 132s, 166 passed, 4 skipped, then
  125s with the same flake again (165 passed, 1 failed).
Open questions for human: none
Next step proposed: 4
```

### 2026-11-04
```
STEP 4: Required CI mirrors local verify   STATUS: passed
Gate command(s): (a) throwaway PR #122 breaking an assertion in e2e/home.spec.ts, its
  description the empty template; (b) #121 (this step); (c) git push origin HEAD:main
  with an empty commit on origin/main.
Result: (a) web, verify and pr-body red, merge state BLOCKED; the failure was the planted
  "Checking nothing at all" (desktop and phone). Filling the description reran pr-body
  -> pass, still BLOCKED on verify. Closed, branch deleted.
  (b) all checks pass (lint 38s, test 1m56s, web 2m40s, verify, pr-body, cla), merge state
  CLEAN. (c) rejected: GH006 "Changes must be made through a pull request"; main unchanged.
Negative control: (a) is it. Also check-pr-body.py fails #120's description (no sections).
Flagged changes: TestServicesWithDocker (internal/runner, protected) fails instead of
  skipping when FACTORY_REQUIRE_DOCKER is set; CI sets it. Stricter. Its first CI run came
  back "(cached)" from a run that skipped; with the variable it ran (internal/runner 30s).
Decisions made: CI's jobs each run one --part of verify.sh (same speed as before) and a
  verify job aggregates them, with if: always() so a skipped part can't pass; the
  golangci-lint action only installs the binary. Required: verify, pr-body, cla; strict
  up to date (no merge queue for personal accounts); PRs required with 0 approvals (sole
  maintainer; Step 9); admins included; no force push or deletion. The pr-body check runs
  the PR's own script, like every test; review is the guard. Auto mode refused a merge
  attempt on the red PR, so (a) is shown by GitHub's merge state, not by a refused merge.
Open questions for human: none
Next step proposed: 5
```

### 2026-11-04
```
STEP 5: Characterization + critical-path tests   STATUS: passed
Gate command(s): python3 docs/factory/seeds.py <clone> (24 one-line bugs in the 5 critical
  flows' code, each package's tests run per seed); go test -race -count=5 on the new tests;
  CI's ci workflow on #123, run 1 plus 4 reruns.
Result: seeds before: 16 of 23 caught (one seed's text matched twice, so it never ran).
  After: 23 of 24, by flow 2/2, 5/6, 4/4, 7/7, 5/5 (table in BASELINE). The rerun first
  showed 22: my redirect test didn't catch its seed, since //host is refused by the "has a
  host" check anyway; ///host (no host when parsed, another site to a browser) does.
  New tests: -count=5 -race all ok locally; CI run 41765848116 attempts 1-5 all success.
  Full verify: 220s, 166 passed, 4 skipped.
Negative control: the seeds are the negative control. Also: a renamed JSON field fails the
  existing contract test, TestOpenAPIMatchesGo; with the dashboard tests' clock pinned to
  2020 every internal/web test passes (before the per-repo fix, one didn't).
Flagged changes: (*Store).Today replaces store.Today, and finishJob stamps finished_at
  from the store's Clock (nil in production, so the wall clock, as before). One test case
  I added ("copied into protected") was removed: it claimed to cover copies, but the diff
  guard's copy parsing never runs (git -M overrides diff.renames = copies), so that seed,
  G4, can't be caught. Recorded as unreachable rather than tested.
Decisions made: measure the existing net with seeds before writing tests, and write tests
  only for the misses (coverage was 58 to 100% per package; it was a guide, not the gate).
  Seeds run in a scratch clone, never this checkout. seeds.py is committed so later steps
  can rerun it. Characterization beyond the misses wasn't needed: the flows' code already
  had focused tests that caught the other 16.
Follow-up for the hooks (Step 3, protected): the guard's "commit as its own command" rule
  also applies in repos without scripts/verify.sh, where there's nothing to check; it
  should apply only where verify runs. Not changed here.
Open questions for human: none
Next step proposed: 6
```

### 2026-11-04
```
PLAYBOOK UPDATE: new PLAYBOOK.md copied in; steps already passed (0 to 5) topped up
against the new gates. Owner column and rows 5b, 7b, 15 (poteto-mode default) and
16 (scale, was 15) added. 5b, 11 and 16 skipped (reasons in the table); 7b waits
until Step 7. Steps 7 to 10's new actions apply when those steps run.

STEP 0 top-up   STATUS: passed
Result: BASELINE.md "CI and repo settings" records the GitHub plan: public repo on a
  personal account; branch protection, rulesets and CODEOWNERS enforcement available;
  merge queue (organization repos only) and push rulesets (private and internal repos
  only) not, with the compensating control for each. Baseline ran with no secrets.
Decisions made: the agent may get its own bot identity (Step 9); no monthly spend cap
  yet (maintainer's answers to new question (f)).
Open questions for human: the spend cap, before Step 13.

STEP 1 top-up   STATUS: passed
Gate command(s): npm ci && npm run build in web/ with the new web/.npmrc; in a scratch
  package with the same .npmrc, npx npm@11.19.1 install @types/node with
  --min-release-age 7 and 0.
Result: install and build pass with ignore-scripts=true (only fsevents, macOS-only,
  has an install script). The cooldown works: age 7 installed 26.6.5, age 0 installed
  26.6.6 (6 days old). The lockfile was already committed.
Negative control: the age 0 run above.
Known gap: npm before 11.10 ignores min-release-age with a warning. CI has 11.19; this
  workstation's Node 24.4.1 has 11.5.2 and the second workstation's Node 22 has npm 10.
  verify only runs npm ci, which installs from the lockfile, so the gap matters only
  when someone adds or updates a package locally.

STEP 3 top-up   STATUS: blocked
Gate command(s): python3 .claude/hooks/guard_test.py (20 tests); the new guard run
  by hand on this checkout's paths, with and without FACTORY_CONFIG=1.
Result: harness paths (.claude/, .github/, docs/factory/, AGENTS.md, CLAUDE.md,
  REVIEW.md, RISK.md) are refused unless the session was started with
  FACTORY_CONFIG=1, and ask with it; the other protected paths still ask. Measured:
  Edit of .claude/settings.json refused, with FACTORY_CONFIG=1 asks;
  `sed -i ... .github/workflows/ci.yml` refused; `bash -c 'echo x >> .claude/hooks/
  guard.py'` refused (the guard now checks the command inside bash -c and sh -c);
  `FACTORY_CONFIG=1 sed -i ... AGENTS.md` refused (a prefix is not the session's env).
  `python3 -c 'open(".claude/x","w")'` passes the guard, as expected of a speed bump.
  settings.json: Read and Edit denies for .env*, factory.env, factory.yaml, *.pem,
  *.key, ~/.ssh, ~/.aws, ~/.azure, gcloud, gh's hosts.yml, ~/.netrc, ~/.npmrc,
  ~/.docker/config.json, ~/.kube/config, ~/.git-credentials; the sandbox on with
  failIfUnavailable and allowUnsandboxedCommands false, writes to the Go, npm and
  Playwright caches, network to the Go and npm registries, GitHub and Playwright's
  CDN. .claude/autonomy.json adds strictAllowlist, Edit denies for the harness paths
  (merged into the sandbox's write denies, so they stop sed -i and python -c too) and
  CLAUDE_CODE_SUBPROCESS_ENV_SCRUB. AGENTS.md says hooks are a speed bump.
Negative control: three breaks of the new guard on a copy (FACTORY_CONFIG check off,
  env defaulted to 1, bash -c not followed) turned 15, 15 and 4 tests red.
Flagged changes: AGENTS.md moved from "edit freely" to harness in guard_test.py (it is
  what CLAUDE.md loads). Stricter.
Why blocked: the live half of the gate needs the maintainer.
  1. The sandbox can't start here: bubblewrap is installed, socat is not
     (sudo apt-get install socat). With failIfUnavailable, Claude Code won't start in
     this repo until it is.
  2. Copy .claude/settings.json into the main checkout (Claude Code reads it there),
     then restart, once with and once without FACTORY_CONFIG=1, and show: an Edit to
     .claude/ refused without it and asking with it; Read of a .env file denied;
     scripts/verify.sh --quick with no prompt; the bypasses `sed -i` on
     .github/workflows/ci.yml and `bash -c 'echo x > .claude/x'` and what stopped each;
     /sandbox shows the unsandboxed retry off. Also confirm verify passes inside the
     sandbox (the Docker test skips there; CI still runs it) and Playwright's Chromium
     starts.
Known gap: in interactive sessions a python -c or similar write to .github/ or
  docs/factory/ is stopped by nothing local (the sandbox protects only Claude Code's
  own files under .claude/); review and CODEOWNERS (Step 9) are the wall.

STEP 4 top-up   STATUS: in progress
Gate command(s): (a) throwaway PR #124 breaking an assertion in web/e2e/home.spec.ts;
  (b) this PR; (c) git push of a new commit on origin/main straight to main.
Result: (a) web and verify red on the planted "Tests: seeded failure" step, merge state
  BLOCKED; closed, branch deleted. (b) see this PR's checks. (c) rejected: GH006
  "Protected branch update failed"; main unchanged at ec1fc56.
  Workflow safety: no repo secrets or variables; ci.yml and pr.yml run PR code with
  contents: read and no secrets; cla.yml's pull_request_target never checks out PR
  code; no workflow runs an agent, so the CI agent limits wait for Step 8. Every
  action is now pinned to a commit SHA, and verify fails on an unpinned one.
  Push ruleset: not available for a public repo (BASELINE.md).
Negative control: (a), and an unpinned actions/checkout@v4 in a scratch worktree made
  verify --part lint fail at "workflow actions pinned".
Flagged changes: none loosened. verify gains the pinned-actions check.
Next: set Step 4 to passed once this PR's checks are green (a FACTORY_CONFIG=1
  session), then finish Step 3's live gate. Steps 8 and 9's gates haven't run yet,
  so there is nothing to re-run for them.
```

### 2026-11-04
```
STEP 4   STATUS: passed
Result: #125 merged as 6afa07e; main takes a PR only once its verify check passes.

STEP 3 live gate, part 1   STATUS: blocked
Result (a session started after #125, so Bash ran in the sandbox):
  Read of a .env and `cat ~/.config/gh/hosts.yml` denied; a python3 -c write to
  .claude/hooks/ and a touch outside the workspace stopped (read-only file system);
  a request to a host off the allowlist refused; the unsandboxed retry off; an edit
  to docs/factory/ refused by the guard without FACTORY_CONFIG; verify --part lint
  and --part web pass in the sandbox with no prompt (Playwright 166 passed). In a
  FACTORY_CONFIG=1 session the guard let edits to .claude/hooks/ and docs/factory/
  through.
  Expected gap seen: a python3 -c write to .github/ went through (Step 9's CODEOWNERS
  is the wall). The probe file is removed.
What the sandbox breaks:
  1. go test: eight tests in internal/mcp and internal/pipeline fail with "socket:
     operation not permitted", because the job bridge listens on a Unix socket. Fix:
     sandbox.network.allowAllUnixSockets true, with filesystem.denyRead on
     /var/run/docker.sock, /run/docker.sock and /run/WSL; then show those are
     unreachable. Until then verify --quick fails in the sandbox, so commits from it
     are refused.
  2. gh: hosts.yml is denied. Fix: credentials mask mode for hosts.yml with
     injectHosts api.github.com and github.com, plus network.tlsTerminate. Mask and
     tlsTerminate are honoured only from user settings or --settings, not
     .claude/settings.json. The maintainer keeps them out of user settings (they'd
     reach every project), so they go in repo files passed with --settings:
     .claude/github.json for interactive sessions, .claude/autonomy.json for
     autonomous ones. Try it with the project's Read deny on hosts.yml kept first;
     dropping that deny would hand the real token to sessions started without
     --settings.
  3. git pull/push: the SSH key is under the ~/.ssh deny. Fix: an HTTPS remote with
     gh as git's credential helper (gh auth setup-git), which rides on the mask.
Applied, approved by the maintainer outside auto mode (the classifier had stopped
  them as self-modification):
  1. settings.json gets allowAllUnixSockets and the three denyReads. In the sandbox
     both Docker sockets are /dev/null (connect refused) and /run/WSL is empty;
     go test -race on internal/mcp and internal/pipeline passes, and so does the
     full scripts/verify.sh (Playwright 166 passed, 4 skipped).
  2. .claude/github.json and .claude/autonomy.json carry the mask; not yet tried
     (needs a restart with --settings).
  3. The remote switch needs the maintainer: the sandbox guards .git/config.
Guard follow-up from Step 5 done: "commit as its own command" now applies only in a
  repo with scripts/verify.sh. New test fails on the old guard; 22 tests pass.
Note: inside the sandbox, git status lists the /dev/null mounts over denied paths
  (.bashrc, .mcp.json, .claude/skills, ...) as untracked. Add files by name there.
Next: switch the remote, restart with --settings .claude/github.json, show gh and
  git push work with hosts.yml masked, then set Step 3 to passed.

STEP 3 live gate, part 2   STATUS: blocked
Gate command(s): in a FACTORY_CONFIG=1 session with the sandbox on and the mask from
  .claude/github.json: gh api user; git ls-remote origin HEAD; git push of a branch;
  python3 .claude/hooks/guard_test.py.
Result: origin is now https://github.com/claptimes5/factory.git with
  `gh auth git-credential` as git's helper. gh works through the mask: gh api user
  answered claptimes5 from inside the sandbox, with the project's Read deny on
  hosts.yml kept. Guard tests: 22 pass.
  git push fails: "Invalid username or token. Password authentication is not
  supported for Git operations." ls-remote passed only because the repo is public
  and needs no auth. Likely cause: the helper gives git the stand-in, git sends it
  base64-encoded in a Basic header, and the proxy swaps the stand-in only where it
  appears in plain text (gh sends it that way, so gh works). Sending it in plain
  text with http.extraHeader "Authorization: Bearer ..." also got a 401: GitHub's
  git endpoint wants Basic.
Not shown: the stand-in itself. Printing even part of the token gh sees was refused
  by auto mode's classifier, which is the right call.
Options for the maintainer: (a) push from a terminal outside Claude Code and accept
  that sandboxed sessions can't push; autonomous runs would then need a different
  route before Step 13. (b) Find out whether the sandbox's mask can swap a stand-in
  inside a Basic header. (c) Give sandboxed sessions a separate fine-grained token
  limited to this repo, readable by git, instead of masking gh's.
Next: the maintainer picks an option; then show a push from inside the sandbox and
  set Step 3 to passed.
```

### 2026-11-04
```
LOCK LEVEL CHANGE: back to Level 1   (PLAYBOOK.md now has "Rollout modes" and "Lock levels")
Why: the factory-config route and the sandbox are Level 3 controls (Step 13), but they
  went on at Step 3. Each factory change then needed the maintainer to restart Claude
  Code, and the agent couldn't commit or push from its own session.
What changed: the guard no longer lists .claude/, .github/, docs/factory/, AGENTS.md,
  CLAUDE.md, REVIEW.md or RISK.md, and the FACTORY_CONFIG check is gone (it is in git
  history, 5d469af, for Level 3). Product protected paths still ask; force pushes,
  --no-verify and rm -rf outside the checkout are still refused; bash -c is still
  checked. settings.json keeps its secret denies and drops the sandbox block.
  .claude/autonomy.json and .claude/github.json stay as Level 3 drafts, unused until
  then. verify switches to asdf's Linux Node when the npm on the PATH is Windows'.
  Rollout mode A (one maintainer, own repo) recorded in BASELINE.md.
Gate command(s): python3 .claude/hooks/guard_test.py (21 tests); verify --quick with
  a fake Windows npm first on the PATH.
Result: 21 pass. verify --quick: "npm here was the Windows one ...; using asdf's
  .../nodejs/24.2.0 instead", then passed; the old verify stopped at the same point.
Negative control: .claude/ put back on the protected list turned 3 tests red.
Flagged changes: guard tests that expected factory files to be refused now expect
  them to be allowed. This is a deliberate loosening, asked for by the maintainer and
  matching the playbook's lock levels.
Next: once this merges and the main checkout pulls it, restart once (settings load at
  start) and re-run Step 3's Level 1 gate live: a product protected edit asks, a commit
  with a lint error is refused, a .env read is denied, verify runs with no prompt, a
  factory file edits freely, and a Bash bypass of a product path is recorded.
```

### 2026-11-04
```
STEP 3: Deterministic guardrails (Level 1 gate)   STATUS: passed
Gate command(s): live, in a session started after #127 (no sandbox): Read of .env;
  guard.py pre-edit on internal/web/auth.go and .claude/settings.json; pre-bash on
  `sed -i ... deploy/upgrade.sh` and `python3 -c "open('deploy/x','w')"`; a commit
  of a planted errcheck violation in a scratch worktree; scripts/verify.sh --quick;
  an Edit of this file.
Result: .env read denied by the permission settings. auth.go: ask. settings.json:
  allowed, no output (factory files are free at Level 1). sed -i on deploy/: ask.
  python3 -c write to deploy/: allowed; the guard can't see it, so at Level 1 only
  review and CI stop it (Step 4 now, CODEOWNERS at Step 9). The commit was refused
  with golangci-lint's "Error return value of os.Remove is not checked (errcheck)",
  and verify got there under Claude Code's own PATH. verify --quick passed in 81s
  with no prompt, after "npm here was the Windows one ...; using asdf's
  .../nodejs/24.2.0 instead" (auto mode, so no prompt doesn't prove the allowlist).
  This Edit to docs/factory/PROGRESS.md went through with no prompt and no hook stop.
Negative control: the planted lint error above; 3 red guard tests with .claude/ put
  back on the protected list (lock-level change entry).
Flagged changes: none in this entry.
Decisions made: the Level 3 work done early (FACTORY_CONFIG route, sandbox, mask,
  autonomy.json) is parked in git history and .claude/autonomy.json until Step 13.
Open questions for human: none
Next step proposed: 6 (5b is skipped)
```

### 2026-11-04
```
STEP 6: Browser E2E + web quality gates   STATUS: passed
Gate command(s): python3 docs/factory/ui-seeds.py <clone>; CI run 41915653371 on e277ee7,
  attempts 1-5; throwaway PR run 41915103770 with a planted failure; Playwright MCP
  opening http://127.0.0.1:8090/app/ and taking a screenshot.
Result: UI seeds caught 5 of 5 (U1 hidden button, U2 moved route, U3 renamed field,
  U4 API field renamed, U5 sign-in skipped; #129). Five attempts all green, web job
  2m27s median (2m19s to 2m39s). The red run's playwright-report artifact (25 MB)
  holds each failure's screenshot and trace. Playwright MCP 0.0.83 (#131) opened the
  devserver and saved a screenshot in this session (not run when #131 merged).
Negative control: the UI seeds and the throwaway red run.
Flagged changes: CI retries 1 -> 0 (stricter, #129).
Decisions made: no Lighthouse-style quality gates (optional for one person). The
  browser tool is the Playwright MCP server, headless, isolated, held to the
  devservers' origins.
Open questions for human: none
```

### 2026-11-04
```
STEP 7: Agent self-verification protocol   STATUS: passed
Gate command(s): sample task 1 (a copy change and a small behaviour change: the
  diff's header and Trust's practice run at zero and one) taken through the
  verify-factory skill, then the verifier agent; sample task 2 (Home's "After this"
  list shows three, not two) with a seeded defect, then a fresh verifier.
Result: task 1 (#132): verify passed (172 passed, 4 skipped); both new browser tests
  red with the source change stashed (4 of 4 runs); before and after screenshots of
  Trust's level 6 from Playwright MCP. Verifier: "VERDICT: pass", citing its own runs
  (52 passed on the branch, 4 failed on main with the branch's specs), a rebuild
  that left git status clean, and desktop and phone screenshots. It also found the
  same singular bug in History.tsx:103 and the devserver's log line, outside the task.
  Task 2: the seed left "and {queue.length - 3} more" while the list grew to three,
  and the author's test counted only the list. Verifier: "VERDICT: fail", "1 current +
  3 listed + '5 more' adds up to 9 out of 8", plus the exactly-4 case, with a repro.
  The branch was deleted unpushed.
Negative control: task 2 is it.
Flagged changes: the PR template's "Testing" heading is now "Testing and evidence";
  check-pr-body.py still matches it ("test"), and its tests pass.
Decisions made: no Stop hook ("not done until verify passes"): the guard already
  refuses a commit until verify --quick passes, and the skill asks for the full
  verify; add one if an agent claims done without it. No /simplify or /bug-hunt
  skills (no recurring need yet). pstack (Appendix D) not used. The verifier has
  Bash so it can run tests and the devserver; it is told not to write in the
  checkout, and both runs left the checkouts clean. Claude Code loads
  .claude/agents/ at session start, so this session ran the verifier as a
  general-purpose agent told to follow verifier.md; from the next session it is
  the "verifier" type.
Found on the way: npm run e2e reuses whatever devserver answers on 8099, so a
  "before" devserver left on 8099 made verify test main's build (4 false failures).
  The skill and verifier.md now say to use 8090 and stop it before e2e.
Open questions for human: 7b. Trigger (a) applies (the pipeline's retry and park
  states are a state machine). Run it now, or skip until a Step 8 review misses
  something there?
Next step proposed: 7b or 8
```
