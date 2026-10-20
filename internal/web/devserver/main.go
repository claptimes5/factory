// Command devserver serves the control room and dashboard over a throwaway
// database seeded with sample work, for `npm run dev` and the Playwright
// checks in web/e2e. It runs nothing: actions are recorded and refused.
//
//	go run ./internal/web/devserver -listen 127.0.0.1:8090
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/preview"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/web"
)

type actions struct{}

func (actions) Retry(_ context.Context, id int64, up bool) error {
	return fmt.Errorf("devserver: would retry job %d (tier up: %v)", id, up)
}
func (actions) Cancel(_ context.Context, id int64) error {
	return fmt.Errorf("devserver: would cancel job %d", id)
}
func (actions) Park(_ context.Context, id int64, reason string) error {
	return fmt.Errorf("devserver: would park item %d: %s", id, reason)
}
func (actions) ApproveEstimate(_ context.Context, id, _ int64) error {
	return fmt.Errorf("devserver: would approve item %d (its estimate or its plan)", id)
}
func (actions) Replan(_ context.Context, id, _ int64, ask string) error {
	return fmt.Errorf("devserver: would ask for another plan for item %d: %s", id, ask)
}
func (actions) Continue(_ context.Context, id int64, extra float64, note string) error {
	return fmt.Errorf("devserver: would continue job %d with %.0f%% more turns and budget (note: %q)", id, extra*100, note)
}
func (actions) Steer(_ context.Context, id int64, text string) error {
	return fmt.Errorf("devserver: would send job %d: %s", id, text)
}
func (actions) ResetAutoMerge(_ context.Context, repo, _ string) error {
	return fmt.Errorf("devserver: would reset auto-merge's breaker for %s", repo)
}

// uninstaller lists sample labels and images, and pretends to remove them.
type uninstaller struct{}

func (uninstaller) Plan(context.Context) web.APIUninstall {
	return web.APIUninstall{
		Labels: []web.APIUninstallLabel{
			{Repo: "acme/api", Name: "factory:ready", Open: 1000, OpenCapped: true}, {Repo: "acme/api", Name: "factory:parked", Open: 3}, {Repo: "acme/api", Name: "factory:risk-low", Open: 0},
			{Repo: "acme/web", Name: "factory:ready", Open: 1}, {Repo: "acme/web", Name: "factory:awaiting-you", Open: 2},
		},
		Repos: []web.APIUninstallRepo{},
		Kept: []web.APIUninstallKept{
			{What: "label ready on acme/web", Why: "your intake label, which the factory didn't make: yours to delete by hand"},
		},
		Images: []web.APIUninstallImage{
			{Ref: "factory-job:latest", ID: "3f2a9c1d8e7b", Size: "2.31GB"},
			{Ref: "a1b2c3d4e5f6", ID: "a1b2c3d4e5f6", Size: "2.29GB", Dangling: true},
		},
		Data: web.APIUninstallData{Path: "/srv/factory/data", Exists: true, Size: "418.2MB",
			What: "Job logs, kept checkouts of stopped builds, readiness reports and repo caches, in /srv/factory/data.",
			Note: "The database (items, jobs, spend), the Codex sign-in and the dashboard's key stay while the factory runs. Once it's stopped, delete /srv/factory/data, or run ./factory uninstall --data."},
		Manual: []web.APIUninstallStep{
			{Text: "Delete the GitHub App acme-factory (only its owner can): its settings page, Advanced, Delete GitHub App.", URL: "https://github.com/settings/apps/acme-factory"},
			{Text: "If acme owns the app, it's under the organization's settings instead.", URL: "https://github.com/organizations/acme/settings/apps/acme-factory"},
			{Text: "The factory doesn't delete its settings: delete /srv/factory/factory.yaml and /srv/factory/factory.env yourself once you're done (factory.env holds its keys)."},
		},
	}
}

func (u uninstaller) Remove(ctx context.Context, data bool) (web.APIUninstallResult, error) {
	p := u.Plan(ctx)
	res := web.APIUninstallResult{Done: []web.APIUninstallDone{}, Manual: append([]web.APIUninstallStep{{Text: "Stop the factory now. While it runs it puts labels back as it works, and it picks up no new issues without its factory:ready label."}}, p.Manual...)}
	for _, l := range p.Labels {
		res.Done = append(res.Done, web.APIUninstallDone{What: "label " + l.Name + " on " + l.Repo, OK: true})
	}
	for _, im := range p.Images {
		d := web.APIUninstallDone{What: "image " + im.Ref, OK: !im.Dangling}
		if im.Dangling {
			d.Error = "image is being used by stopped container 9c1f0e2a"
		}
		res.Done = append(res.Done, d)
	}
	if data {
		res.Done = append(res.Done, web.APIUninstallDone{What: "data /srv/factory/data/jobs", OK: true})
	}
	return res, nil
}

// restarter pretends one job is still running, so a restart stays pending.
type restarter struct{ pending atomic.Bool }

func (r *restarter) RequestRestart(context.Context) int { r.pending.Store(true); return 1 }
func (r *restarter) CancelRestart(context.Context) bool { return r.pending.Swap(false) }
func (r *restarter) RestartPending() bool               { return r.pending.Load() }
func (r *restarter) Running() int                       { return 1 }
func (r *restarter) RestartWaitsFor() string            { return "" }

// sampleConfig is the factory.yaml the Settings screen edits.
const sampleConfig = `# A sample factory.yaml for the devserver.
repos: [acme/api, acme/web]
runner: {kind: local}

routing:
  triage:       {harness: claude, model: haiku}
  implement.S:  {harness: claude, model: sonnet}   # escalates on a miss
  implement.M:  {harness: claude, model: sonnet}
  implement.L:  {harness: claude, model: opus, fast: true}
  review:       {harness: codex,  model: gpt-6.1-sol}

escalation:
  implement: [sonnet, opus]

budgets:
  per_job: {XS: 0.5, S: 2, M: 6, L: 15}
  triage: 0.5
  review: 3
  daily_total: 25             # dollars a day
  concurrent_jobs: 3
  confirm_over: 10            # ask before building anything estimated over $10

pricing:
  gpt-6.1-sol: {input_per_mtok: 2, output_per_mtok: 10}
`

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to serve on")
	flag.Parse()
	dir, err := os.MkdirTemp("", "factory-devserver-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "factory.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfgPath := filepath.Join(dir, "factory.yaml")
	if err := os.WriteFile(cfgPath, []byte(sampleConfig), 0o644); err != nil {
		log.Fatal(err)
	}
	jobs := filepath.Join(dir, "jobs")
	if err := seed(context.Background(), st, jobs); err != nil {
		log.Fatal(err)
	}
	if err := seedTuning(context.Background(), st); err != nil {
		log.Fatal(err)
	}
	if err := seedHealth(context.Background(), st); err != nil {
		log.Fatal(err)
	}
	reports := filepath.Join(dir, "readiness")
	if err := seedReadiness(reports); err != nil {
		log.Fatal(err)
	}
	if err := seedUsage(context.Background(), st); err != nil {
		log.Fatal(err)
	}
	if err := seedChats(context.Background(), st); err != nil {
		log.Fatal(err)
	}
	rs := &restarter{}
	s := &web.Server{
		Store: st, Actions: actions{}, Repos: []string{"acme/api", "acme/web"}, DailyCap: 25, AwaitCap: 5, JobsDir: jobs,
		ConfigPath: cfgPath, Listen: *listen, Restarter: rs, StartedConfig: web.ConfigVersion([]byte(sampleConfig)),
		Choices: sampleChoices(st),
		SteerUnsupported: func(j *store.Job) string {
			if j.Kind != "implement" || j.Harness != "claude" {
				return "Only a running Claude build takes messages."
			}
			return ""
		},
		ReadinessDir: reports, IntakeLabel: "factory:ready", CheckReadiness: recheck(reports), FileReadiness: fileIssues(reports),
		Plans: []string{"claude", "codex"}, PauseAtUsage: 95,
		Chat:         &chatter{st: st},
		Wizard:       newWizard(rs),
		Uninstaller:  uninstaller{},
		Orchestrator: web.OrchestratorInfo{Harness: "claude", Model: "opus", Every: "1h0m0s", RunBudget: 2, DailyBudget: 10},
		Breakers: func(context.Context) []web.APIBreaker {
			return []web.APIBreaker{
				{Repo: "acme/api", On: true, Strikes: 1, Threshold: 2, WindowHours: 168},
				{Repo: "acme/web", On: true, Tripped: true, TrippedAt: store.Now(), Strikes: 2, Threshold: 2, WindowHours: 168,
					Reason: "2 factory PRs in 7 days: acme/web#41 reverted by commit 1a2b3c4; acme/web#44 turned main red (test)"},
			}
		},
		Issue: func(_ context.Context, _ string, n int) (*github.Issue, error) {
			return &github.Issue{Number: n, Body: fmt.Sprintf("Issue #%d.\n\nThe parser drops the last token when the input has no trailing newline.\n\nExpected: every token is kept.\nVerify: `go test ./parser`.", n)}, nil
		},
		Checks: func(_ context.Context, _, sha string) ([]github.CheckRun, error) {
			return []github.CheckRun{
				{Name: "test", Status: "completed", Conclusion: "success", URL: "https://example.com/ci/" + sha},
				{Name: "lint", Status: "in_progress"},
			}, nil
		},
		// A local shell stands in for the job's container.
		Exec: func(ctx context.Context, id int64) (*exec.Cmd, error) {
			j, err := st.GetJob(ctx, id)
			if err != nil {
				return nil, err
			}
			if j.State != state.Running {
				return nil, errors.New("job is not running")
			}
			cmd := exec.CommandContext(ctx, "sh")
			cmd.Env = append(os.Environ(), "PS1=job$ ")
			cmd.Dir = dir
			return cmd, nil
		},
	}
	h, err := s.Handler()
	if err != nil {
		log.Fatal(err)
	}
	// GitHub's new-app form, faked here so the app flow runs offline.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fake-github/settings/apps/new", fakeGitHub)
	mux.Handle("/", h)
	log.Printf("dashboard on http://%s/app/", *listen)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
	}
}

func seed(ctx context.Context, st *store.Store, jobs string) error {
	path := map[state.Stage][]state.Stage{
		state.Triage:        {},
		state.Ready:         {state.Ready},
		state.Implementing:  {state.Ready, state.Implementing},
		state.Reviewing:     {state.Ready, state.Implementing, state.Reviewing},
		state.AwaitingHuman: {state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman},
		state.Parked:        {state.Parked},
		state.Blocked:       {state.Blocked},
		state.Done:          {state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done},
	}
	items := []struct {
		repo  string
		issue int
		title string
		stage state.Stage
		size  string
	}{
		{"acme/web", 101, "Parser drops the last token without a trailing newline", state.AwaitingHuman, "S"},
		{"acme/api", 102, "Add retry with backoff to the webhook client", state.Implementing, "M"},
		{"acme/api", 103, "Rate-limit the public search endpoint", state.Reviewing, "M"},
		{"acme/api", 104, "Support YAML anchors in config files", state.Parked, "L"},
		{"acme/web", 105, "Typo in the CLI help for --dry-run", state.Ready, "S"},
		{"acme/api", 106, "Flaky TestCacheEviction on CI", state.Triage, ""},
		{"acme/web", 107, "Export metrics in OpenMetrics format", state.Done, "M"},
		{"acme/api", 108, "Move the cache to Redis 7", state.Blocked, "M"},
		{"acme/web", 109, "Show order totals on the checkout page", state.AwaitingHuman, "M"},
		{"acme/api", 110, "Rewrite the job scheduler", state.Blocked, "L"},             // over the cost limit
		{"acme/api", 111, "Split billing out of the monolith", state.Blocked, "L"},     // its plan waits
		{"acme/api", 112, "Add SSO sign-in to the admin app", state.Parked, "L"},       // its build ran out of turns
		{"acme/api", 113, "Make the lint check pass on main", state.Implementing, "S"}, // from the readiness check
		{"acme/api", 115, "Paginate the audit log API", state.Parked, "M"},             // its build failed, then its retry was cancelled
	}
	ids := map[int]int64{}
	for i, x := range items {
		it, _, err := st.UpsertItem(ctx, x.repo, x.issue, x.title)
		if err != nil {
			return err
		}
		ids[x.issue] = it.ID
		size, risk, prio := x.size, "low", len(items)-i
		u := store.ItemUpdate{Complexity: &size, Risk: &risk}
		if err := st.UpdateItem(ctx, it.ID, u); err != nil {
			return err
		}
		if err := st.SetPriority(ctx, it.ID, prio); err != nil {
			return err
		}
		for _, to := range path[x.stage] {
			reason := "seeded"
			if to == state.Parked {
				reason = "Should anchors merge across files, or only within one? The issue does not say."
				if x.issue == 115 {
					reason = "Its retry was cancelled from the dashboard. Nothing runs until you choose what's next: retry it, or leave it parked."
				}
				if x.issue == 112 {
					reason = "Its build (sonnet) stopped: it ran out of turns (80 of 80) before it finished. Its work is kept on the factory's machine (nothing was pushed), so it doesn't restart on its own: running out again is likely without a change. " +
						"Continue picks up where it stopped, in the same checkout and session, with more turns or budget and a note from you (for example, to narrow the scope). " +
						"Retry starts it from scratch; Tier up starts it from scratch on the next model. Or leave it parked."
				}
			}
			if to == state.Blocked {
				reason = "Waits on #102, which changes the same client."
				if x.issue == 110 {
					reason = "Waiting for your OK on the cost: estimated $9.40–$16.20, over the $10.00 limit. To build it, press Approve estimate on this item's page, or add the factory:go label on the issue."
				}
				if x.issue == 111 {
					reason = "Waiting for your OK on the plan."
				}
			}
			if err := st.SetStage(ctx, it.ID, to, "daemon", reason); err != nil {
				return err
			}
		}
	}
	// 112's build ran out of turns: its work is kept and waits for you.
	if err := seedStopped(ctx, st, jobs, ids[112]); err != nil {
		return err
	}
	// 115's build failed its gates, and you cancelled its retry.
	if err := seedCancelledRetry(ctx, st, ids[115]); err != nil {
		return err
	}
	job := func(issue int, kind, model string, end state.JobState, cost float64, verdict string) (int64, error) {
		item := ids[issue]
		harness := "claude"
		if kind == "review" {
			harness = "codex"
		}
		id, err := st.EnqueueJob(ctx, store.Job{ItemID: &item, Kind: kind, Harness: harness, Model: model, BudgetUSD: 6})
		if err != nil {
			return 0, err
		}
		if end == state.Queued {
			return id, nil
		}
		if _, err := st.ClaimJob(ctx); err != nil {
			return 0, err
		}
		dir := filepath.Join(jobs, fmt.Sprint(id))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
		logPath := filepath.Join(dir, "log.jsonl")
		lines := sampleClaudeLog
		if harness == "codex" {
			lines = sampleCodexLog
		}
		lines = append([]string{}, lines...)
		if end == state.Running && kind == "implement" {
			lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":"Edit","input":{"file_path":"/work/webhook/retry.go","old_string":"return err","new_string":"return backoff(err)"}}]}}`)
		}
		if end != state.Running && harness == "claude" {
			lines = append(lines, `{"type":"result","subtype":"success","is_error":false,"num_turns":14,"total_cost_usd":1.42,"result":"Fixed and tested."}`)
		}
		if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return 0, err
		}
		if err := st.SetJobRuntime(ctx, id, "", logPath, ""); err != nil {
			return 0, err
		}
		if end == state.Running {
			return id, nil
		}
		return id, st.FinishJob(ctx, id, store.Result{State: end, CostUSD: cost, Verdict: verdict})
	}
	triage := `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"Keep the final token when the input has no trailing newline; add a regression test in parser_test.go."}`
	if _, err := job(101, "triage", "haiku", state.Succeeded, 0.04, triage); err != nil {
		return err
	}
	impl, err := job(101, "implement", "sonnet", state.Succeeded, 1.42, `{"status":"done","summary":"Fixed","tests_added":["TestLastToken"],"blocker":""}`)
	if err != nil {
		return err
	}
	if err := st.SetJobHead(ctx, impl, "3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a39"); err != nil {
		return err
	}
	if err := st.SetGateResults(ctx, impl, []store.GateResult{{Gate: "build", Status: "pass"}, {Gate: "test", Status: "pass"}, {Gate: "lint", Status: "pass"}, {Gate: "proves_fix", Status: "pass"}, {Gate: "diff_guard", Status: "pass"}}); err != nil {
		return err
	}
	review := `{"decision":"pass","findings":[{"severity":"minor","file":"parser/parser.go","line":42,"claim":"The bounds check repeats the loop condition.","evidence":"for i <= len(toks) already guards it"},{"severity":"major","file":"parser/parser_test.go","line":12,"claim":"The test only covers ASCII input.","evidence":"No case with a multi-byte final token"}],"challenged_assumptions":["Inputs are always UTF-8"]}`
	rev, err := job(101, "review", "gpt-6.1-sol", state.Succeeded, 0.61, review)
	if err != nil {
		return err
	}
	pr, branch := 88, "factory/issue-101"
	if err := st.UpdateItem(ctx, ids[101], store.ItemUpdate{PRNumber: &pr, Branch: &branch}); err != nil {
		return err
	}
	patch := `diff --git a/parser/parser.go b/parser/parser.go
index 1a2b3c4..5d6e7f8 100644
--- a/parser/parser.go
+++ b/parser/parser.go
@@ -38,9 +38,9 @@ func Tokens(in string) []string {
 	toks := split(in)
 	var out []string
-	for i := 0; i < len(toks)-1; i++ {
+	for i := 0; i < len(toks); i++ {
 		if toks[i] == "" {
 			continue
 		}
-		out = append(out, toks[i])
+		out = append(out, strings.TrimSpace(toks[i]))
 	}
 	return out
 }
diff --git a/parser/parser_test.go b/parser/parser_test.go
new file mode 100644
--- /dev/null
+++ b/parser/parser_test.go
@@ -0,0 +1,14 @@
+package parser
+
+import "testing"
+
+func TestLastToken(t *testing.T) {
+	got := Tokens("a b c")
+	if len(got) != 3 || got[2] != "c" {
+		t.Fatalf("got %q", got)
+	}
+}
+
+func TestEmpty(t *testing.T) {
+	if len(Tokens("")) != 0 {
+		t.Fatal()
+	}
+}
`
	if err := os.WriteFile(filepath.Join(jobs, fmt.Sprint(rev), "diff.patch"), []byte(patch), 0o644); err != nil {
		return err
	}
	if err := seedPreview(filepath.Join(jobs, fmt.Sprint(impl), preview.Dir)); err != nil {
		return err
	}
	if _, err := job(102, "implement", "sonnet", state.Running, 0, ""); err != nil {
		return err
	}
	if _, err := job(103, "implement", "sonnet", state.Failed, 2.1, ""); err != nil {
		return err
	}
	opus, err := job(103, "implement", "opus", state.Succeeded, 4.3, "")
	if err != nil {
		return err
	}
	if err := st.SetGateResults(ctx, opus, []store.GateResult{{Gate: "build", Status: "pass"}, {Gate: "test", Status: "pass"}, {Gate: "lint", Status: "pass"}, {Gate: "proves_fix", Status: "pass"}, {Gate: "diff_guard", Status: "pass"}}); err != nil {
		return err
	}
	if _, err := job(103, "review", "gpt-6.1-sol", state.Running, 0, ""); err != nil {
		return err
	}
	if _, err := job(107, "implement", "sonnet", state.Succeeded, 2.2, ""); err != nil {
		return err
	}
	if _, err := job(107, "review", "gpt-6.1-sol", state.Succeeded, 0.5, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`); err != nil {
		return err
	}
	// 109 spans the acme workspace: an API change and the page that shows it.
	ws, err := job(109, "implement", "sonnet", state.Succeeded, 3.1, `{"status":"done","summary":"Totals","tests_added":["TestOrderTotals"],"blocker":""}`)
	if err != nil {
		return err
	}
	if err := st.SetJobHead(ctx, ws, "8c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d"); err != nil {
		return err
	}
	if err := st.SetGateResults(ctx, ws, []store.GateResult{{Gate: "api: test", Status: "pass"}, {Gate: "api: diff_guard", Status: "pass"}, {Gate: "web: build", Status: "pass"}, {Gate: "web: test", Status: "pass"}, {Gate: "web: diff_guard", Status: "pass"}}); err != nil {
		return err
	}
	if _, err := job(109, "review", "gpt-6.1-sol", state.Succeeded, 0.8, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`); err != nil {
		return err
	}
	wsPR, wsBranch := 92, "factory/web-109"
	if err := st.UpdateItem(ctx, ids[109], store.ItemUpdate{PRNumber: &wsPR, Branch: &wsBranch}); err != nil {
		return err
	}
	if err := st.SetItemPRs(ctx, ids[109], []store.RepoPR{{Repo: "acme/api", Number: 91}, {Repo: "acme/web", Number: 92}}); err != nil {
		return err
	}
	// 110 is over the cost limit and waits for an OK; 101 and 107 were under it.
	for _, e := range []store.Estimate{
		{ItemID: ids[110], Size: "L", MedianUSD: 9.4, P75USD: 16.2, TotalMedianUSD: 9.4, TotalP75USD: 16.2, Basis: "all", Samples: 6, ThresholdUSD: 10, Status: store.EstimateWaiting,
			Detail: "based on 6 past L items across all repos (2 in acme/api, too few on their own): median $9.40, p75 $16.20"},
		{ItemID: ids[101], Size: "S", MedianUSD: 1.6, P75USD: 2.4, TotalMedianUSD: 1.6, TotalP75USD: 2.4, Basis: "repo", Samples: 9, ThresholdUSD: 10, Status: store.EstimateUnder,
			Detail: "based on 9 past S items in acme/web: median $1.60, p75 $2.40"},
		{ItemID: ids[107], Size: "M", MedianUSD: 3.1, P75USD: 4.8, TotalMedianUSD: 3.1, TotalP75USD: 4.8, Basis: "default", ThresholdUSD: 10, Status: store.EstimateUnder,
			Detail: "from your budgets, with too few past M items to go by (2; it takes 5): half to all of one M build's $6.00 cap and one review's $3.00"},
	} {
		if err := st.SetEstimate(ctx, e); err != nil {
			return err
		}
	}
	// 111 was planned before its build, as an L item, and the plan waits.
	// It's built after 103.
	if err := st.SetDependencies(ctx, ids[111], []int64{ids[103]}, "human", "billing builds on the rate limiter"); err != nil {
		return err
	}
	if _, err := job(111, "triage", "haiku", state.Succeeded, 0.05, `{"decision":"ready","complexity":"L","risk":"low","question":"","summary":"Move billing into its own service behind the existing API."}`); err != nil {
		return err
	}
	plan, err := job(111, "plan", "opus", state.Succeeded, 0.88, `{"ask":"Keep the billing tables in the main database for now (recommended: the switch-over stays reversible), or move them with the service in this change?","approach":"Move billing/ into a new service and call it over the existing internal API, keeping the database tables where they are for now.",`+
		`"files":["billing/service.go","api/billing_client.go","deploy/billing.yaml"],"risks":["Invoices issued during the switch-over could be counted twice."],"tests":"Contract tests between the API and the new service, and the existing billing suite."}`)
	if err != nil {
		return err
	}
	for _, typ := range []string{store.EvPlanWaiting, store.EvPlanAnnounced} {
		if err := st.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: ptr(ids[111]), JobID: &plan, Type: typ}); err != nil {
			return err
		}
	}
	for _, e := range []store.Event{
		{Actor: "orchestrator", ItemID: ptr(ids[103]), Type: "retry", Reason: "Sonnet failed the gates twice on the same lock-ordering bug; retrying one tier up."},
		{Actor: "orchestrator", ItemID: ptr(ids[105]), Type: "set_priority", Reason: "Trivial fix that unblocks the docs release."},
		{Actor: "orchestrator", ItemID: ptr(ids[104]), Type: "park", Reason: "The issue does not say whether anchors merge across files."},
		{Actor: "orchestrator", Type: "summary", Reason: "Shipped 1, 1 waiting on you (#101), 1 parked (#104). $11.17 of $25 spent."},
	} {
		if err := st.AddEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// seedTuning adds a pending tuning proposal for the Settings screen.
func seedTuning(ctx context.Context, st *store.Store) error {
	changes := []store.ProposedChange{
		{Path: []string{"budgets", "concurrent_jobs"}, Value: 4.0, Why: "Jobs waited in the queue on 11 of 30 days (up from 4), while spend stayed under $18 of the $25 cap."},
		{Path: []string{"routing", "implement.S", "effort"}, Value: "high", Why: "implement.S passed first time on 9 of 17 builds (0.53, down from 0.71); check it with factory eval first."},
	}
	edits := make([]config.Change, len(changes))
	for i, c := range changes {
		edits[i] = config.Change{Path: c.Path, Value: c.Value}
	}
	out, err := config.Edit([]byte(sampleConfig), edits)
	if err != nil {
		return err
	}
	from := time.Now().UTC().AddDate(0, 0, -30).Format(store.TimeFormat)
	_, err = st.AddProposal(ctx, store.Proposal{
		Summary: "First-pass rate on S builds fell from 0.71 to 0.53 while cost per merged PR held at $1.90. Jobs often waited for a free slot.",
		Changes: changes,
		Dropped: []store.ProposedChange{{Path: []string{"gates", "commands", "test"}, Value: "go test -short ./...", Why: "Tests take 6 minutes.", Reason: "gates.commands.test isn't a setting tuning may change"}},
		Ideas:   []string{"3 of 5 parks asked which package a change belongs in: tell triage to name the package in its summary."},
		Diff:    config.UnifiedDiff("factory.yaml", []byte(sampleConfig), out), PRURL: "https://github.com/acme/api/pull/120",
		PeriodFrom: from, PeriodTo: store.Now(),
	})
	return err
}

func ptr(v int64) *int64 { return &v }

// sampleClaudeLog is a build's log as Claude Code streams it.
var sampleClaudeLog = []string{
	`{"type":"system","subtype":"init","model":"claude-sonnet-4-5","tools":["Bash","Read","Edit","Write","Glob","Grep","TodoWrite"],"session_id":"s1"}`,
	`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"The bug report says the last token goes missing. Look at the loop bounds first."},{"type":"text","text":"Reading the parser and its tests."}]}}`,
	`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/work/parser/parser.go"}},{"type":"tool_use","id":"t2","name":"Grep","input":{"pattern":"func Tokens","path":"/work/parser"}}]}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package parser\n\nfunc Tokens(in string) []string {\n\ttoks := split(in)\n\tfor i := 0; i < len(toks)-1; i++ {"},{"type":"tool_result","tool_use_id":"t2","content":"parser/parser.go:38:func Tokens(in string) []string {"}]}}`,
	`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"go test ./parser","description":"Run the parser tests"}}]}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"--- FAIL: TestLastToken (0.00s)\n    parser_test.go:9: got [\"a\" \"b\"]\nFAIL","is_error":true}]}}`,
	`{"type":"assistant","message":{"content":[{"type":"text","text":"The loop stops one token early when the input lacks a trailing newline. Writing a failing test first, then:\n\n- fix the bound in ` + "`Tokens`" + `\n- **keep** the empty-input case"}]}}`,
}

// sampleCodexLog is a review's log as Codex streams it.
var sampleCodexLog = []string{
	`{"type":"thread.started","thread_id":"th1"}`,
	`{"type":"turn.started"}`,
	`{"type":"item.completed","item":{"id":"0","type":"reasoning","text":"Check the loop bound and the new test."}}`,
	`{"type":"item.started","item":{"id":"1","type":"command_execution","command":"bash -lc 'git diff HEAD~1'","status":"in_progress"}}`,
	`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"bash -lc 'git diff HEAD~1'","aggregated_output":"diff --git a/parser/parser.go b/parser/parser.go","exit_code":0,"status":"completed"}}`,
	`{"type":"item.completed","item":{"id":"2","type":"agent_message","text":"The fix is right; one test gap on multi-byte input."}}`,
	`{"type":"turn.completed","usage":{"input_tokens":12000,"cached_input_tokens":3000,"output_tokens":800}}`,
}

// seedCancelledRetry adds a build that failed and a retry a person
// cancelled before it started.
func seedCancelledRetry(ctx context.Context, st *store.Store, item int64) error {
	id, err := st.EnqueueJob(ctx, store.Job{ItemID: &item, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 6, Attempt: 1})
	if err != nil {
		return err
	}
	if _, err := st.ClaimJob(ctx); err != nil {
		return err
	}
	if err := st.FinishJob(ctx, id, store.Result{State: state.Failed, CostUSD: 2.4, Error: "gates failed: test (TestAuditPage: want 50 rows, got 49)"}); err != nil {
		return err
	}
	retry, err := st.EnqueueJob(ctx, store.Job{ItemID: &item, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 6, Attempt: 2})
	if err != nil {
		return err
	}
	return st.CancelQueued(ctx, retry, "human", "cancelled from the control room")
}

// seedStopped adds a build that ran out of turns: its log, how it
// stopped, its note and the work it kept.
func seedStopped(ctx context.Context, st *store.Store, jobs string, item int64) error {
	id, err := st.EnqueueJob(ctx, store.Job{ItemID: &item, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 15, Attempt: 1})
	if err != nil {
		return err
	}
	if _, err := st.ClaimJob(ctx); err != nil {
		return err
	}
	dir := filepath.Join(jobs, fmt.Sprint(id))
	if err := os.MkdirAll(filepath.Join(dir, "kept"), 0o755); err != nil {
		return err
	}
	lines := append([]string{}, sampleClaudeLog...)
	lines = append(lines,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"s1","name":"Write","input":{"file_path":"/work/admin/sso.go","content":"package admin"}},{"type":"tool_use","id":"s2","name":"Edit","input":{"file_path":"/work/admin/routes.go"}},{"type":"tool_use","id":"s3","name":"Bash","input":{"command":"go test ./admin/..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"s1","content":"ok"},{"type":"tool_result","tool_use_id":"s2","content":"ok"},{"type":"tool_result","tool_use_id":"s3","content":"ok  admin 0.4s"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The SSO callback works. Committing what I have and writing down what's left, as I'm near my turn limit."}]}}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":80,"total_cost_usd":11.84,"result":""}`)
	logPath := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if err := st.SetJobRuntime(ctx, id, "", logPath, ""); err != nil {
		return err
	}
	stop := `{"reason":"turns","summary":"Stopped: it ran out of turns (80 of 80) before it finished.","turns":80,"max_turns":80,"cost_usd":11.84,"budget_usd":15,` +
		`"note":"Done: the SSO callback and session cookie, with tests.\nLeft: the sign-out route, and the admin settings page for the IdP URL.","kept":true}`
	patch := "diff --git a/admin/sso.go b/admin/sso.go\nnew file mode 100644\n--- /dev/null\n+++ b/admin/sso.go\n@@ -0,0 +1,3 @@\n+package admin\n+\n+// Callback finishes an SSO sign-in.\n"
	for name, body := range map[string]string{"stop.json": stop, "saved.patch": patch, "kept/kept.json": `{"job":1}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return st.FinishJob(ctx, id, store.Result{State: state.Failed, CostUSD: 11.84, Error: "Stopped: it ran out of turns (80 of 80) before it finished. Its work is kept."})
}

// sampleChoices are what the daemon would offer for a job that ended.
func sampleChoices(st *store.Store) func(ctx context.Context, id int64) []web.APIChoice {
	return func(ctx context.Context, id int64) []web.APIChoice {
		j, err := st.GetJob(ctx, id)
		if err != nil || j.ItemID == nil {
			return nil
		}
		it, err := st.GetItem(ctx, *j.ItemID)
		if err != nil {
			return nil
		}
		var out []web.APIChoice
		if _, err := os.Stat(filepath.Join(filepath.Dir(j.LogPath), "kept", "kept.json")); err == nil && j.LogPath != "" {
			out = append(out, web.APIChoice{Action: "continue", Label: "Continue", Model: "claude/sonnet", Available: true,
				Explain: "Picks up where it stopped, in the checkout it kept and the same Claude session, on claude/sonnet, with a fresh 80 turns and $15.00; you can add up to 200% more. A note you write goes to it first, for example to narrow the scope. Nothing is pushed until its checks pass."})
		}
		out = append(out, web.APIChoice{Action: "retry", Label: "Retry", Model: j.Harness + "/" + j.Model, Available: true,
			Explain: "Starts this step over from scratch, on " + j.Harness + "/" + j.Model + ". Anything this job did is dropped."})
		if j.Kind == "implement" {
			out = append(out, web.APIChoice{Action: "tier_up", Label: "Tier up", Model: "claude/opus", Available: true,
				Explain: "Starts the build over from scratch one model tier up, on claude/opus. Anything this job did is dropped."})
		}
		park := web.APIChoice{Action: "park", Label: "Park", Available: it.Stage != state.Parked,
			Explain: "Stops work on the item and takes it off the queue. Nothing is lost. To bring it back, press Retry (or Continue) here, or add the factory:retry label on the issue."}
		if it.Stage == state.Parked {
			park.Why = "It's parked now: nothing runs until you choose. Leaving it parked is fine; bring it back with the buttons here, or add the factory:retry label on the issue."
		}
		return append(out, park)
	}
}

// seedHealth makes acme/api's main flaky, with its issue filed.
func seedHealth(ctx context.Context, st *store.Store) error {
	for _, h := range []store.RepoHealth{
		{Repo: "acme/api", Branch: "main", HeadSHA: "9f2c1e0", State: store.HealthFlaky, Detail: "test"},
		{Repo: "acme/web", Branch: "main", HeadSHA: "41ab7d2", State: store.HealthGreen},
	} {
		if err := st.SetRepoHealth(ctx, h); err != nil {
			return err
		}
	}
	return st.SetHealthIssue(ctx, store.HealthIssue{Repo: "acme/api", Check: "test", Kind: store.HealthFlaky, State: store.HealthFlaky, HeadSHA: "9f2c1e0", IssueNumber: 106})
}

// seedPreview writes screenshots of a made-up page at two sizes, before
// and after, as a build with previews would leave them.
func seedPreview(dir string) error {
	m := &preview.Manifest{Head: "3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a39", Base: "9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2a9c1d", At: store.Now(),
		Notes: []string{"/admin at phone: net::ERR_ABORTED"}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, side := range []string{preview.Before, preview.After} {
		for _, vp := range []struct {
			name string
			w, h int
		}{{"desktop", 320, 200}, {"phone", 98, 211}} {
			img := image.NewRGBA(image.Rect(0, 0, vp.w, vp.h))
			bar := color.RGBA{0x94, 0xa3, 0xb8, 0xff}
			if side == preview.After {
				bar = color.RGBA{0x25, 0x63, 0xeb, 0xff}
			}
			for y := 0; y < vp.h; y++ {
				for x := 0; x < vp.w; x++ {
					c := color.RGBA{0xff, 0xff, 0xff, 0xff}
					if y < vp.h/6 {
						c = bar
					}
					img.Set(x, y, c)
				}
			}
			name := side + "-01-" + vp.name + ".png"
			f, err := os.Create(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			err = png.Encode(f, img)
			f.Close()
			if err != nil {
				return err
			}
			m.Shots = append(m.Shots, preview.Shot{File: name, Page: "/settings", Viewport: vp.name, Side: side, Width: vp.w, Height: vp.h})
		}
	}
	return m.Save(dir)
}

// seedReadiness saves readiness reports: acme/api isn't ready (its lint
// fix is being built, its flaky test waits its turn), and acme/web is.
func seedReadiness(dir string) error {
	at := time.Now().Add(-2 * time.Hour).UTC()
	api := &readiness.Report{Repo: "acme/api", Branch: "main", SHA: "9f2c1e04b7a3", Started: at, Score: 34, Filed: []int{113, 114},
		Findings: []readiness.Finding{
			{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on main",
				Detail: "Every change would fail this check, whatever it does.\n\n```\ninternal/cache/cache.go:41:2: ineffectual assignment to err (ineffassign)\n```\n",
				Issue:  &readiness.Issue{Key: "check:lint", Title: "Make the lint check pass on main", Number: 113, State: "open"}},
			{Level: readiness.Blocker, Area: "tests", Title: "Tests are flaky",
				Detail: "They passed, then failed on the same commit.\n\n```\n--- FAIL: TestCacheEviction (0.02s)\n```\n",
				Issue:  &readiness.Issue{Key: "flaky", Title: "Fix flaky tests", Number: 106, State: "open"}},
			{Level: readiness.Gap, Area: "feature map", Title: "No feature map",
				Detail: "A feature map lists what the product does, where each feature lives and which tests prove it.",
				Issue:  &readiness.Issue{Key: "feature-map", Title: "Start a feature map", Number: 114, State: "open"}},
			{Level: readiness.Gap, Area: "e2e", Title: "No end-to-end tests",
				Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests for the main user flows"}},
			{Level: readiness.Good, Area: "install", Title: "Installs from a clean checkout"},
			{Level: readiness.Good, Area: "ci", Title: "CI runs on the repo"},
		}}
	web := &readiness.Report{Repo: "acme/web", Branch: "main", SHA: "41ab7d2c9e10", Started: at, Score: 92, Ready: true, Filed: []int{114},
		Findings: []readiness.Finding{
			{Level: readiness.Gap, Area: "feature map", Title: "No feature map",
				Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map", Number: 114, State: "open"}},
			{Level: readiness.Good, Area: "install", Title: "Installs from a clean checkout"},
			{Level: readiness.Good, Area: "checks", Title: "Every required check passes on main: lint, test, build"},
			{Level: readiness.Good, Area: "tests", Title: "48 test files, passing twice in a row"},
			{Level: readiness.Good, Area: "e2e", Title: "Has end-to-end tests"},
		}}
	for _, r := range []*readiness.Report{api, web} {
		if _, err := readiness.Save(dir, r); err != nil {
			return err
		}
	}
	return nil
}

// recheck pretends to check a repo again: it takes a moment and finds
// it as it was.
func recheck(dir string) func(context.Context, string) error {
	return func(ctx context.Context, repo string) error {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		r, err := readiness.Load(dir, repo)
		if err != nil || r == nil {
			return fmt.Errorf("devserver: no report for %s", repo)
		}
		r.Started = time.Now().UTC()
		_, err = readiness.Save(dir, r)
		return err
	}
}

// fileIssues is the check again, then says what it would file. It
// changes nothing, so every browser test sees the same report.
func fileIssues(dir string) func(context.Context, string, bool) (string, error) {
	check := recheck(dir)
	return func(ctx context.Context, repo string, _ bool) (string, error) {
		if err := check(ctx, repo); err != nil {
			return "", err
		}
		r, err := readiness.Load(dir, repo)
		if err != nil || r == nil {
			return "", fmt.Errorf("devserver: no report for %s", repo)
		}
		n := 0
		for _, f := range r.Findings {
			if f.Issue != nil && f.Issue.Number == 0 {
				n++
			}
		}
		return fmt.Sprintf("Opened %d issue(s) on %s.", n, repo), nil
	}
}

// seedUsage records Claude's plan windows; Codex has none yet.
func seedUsage(ctx context.Context, st *store.Store) error {
	for _, l := range []store.Limit{
		{Harness: "claude", Window: "5h", UsedPercent: 42, ResetsAt: time.Now().Add(3 * time.Hour).UTC().Format(store.TimeFormat)},
		{Harness: "claude", Window: "weekly", UsedPercent: 18, ResetsAt: time.Now().Add(4 * 24 * time.Hour).UTC().Format(store.TimeFormat)},
	} {
		if err := st.SetLimit(ctx, l); err != nil {
			return err
		}
	}
	return nil
}
