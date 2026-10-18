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
func (actions) ApproveEstimate(_ context.Context, id int64) error {
	return fmt.Errorf("devserver: would approve the estimate of item %d", id)
}

// restarter pretends one job is still running, so a restart stays pending.
type restarter struct{ pending atomic.Bool }

func (r *restarter) RequestRestart(context.Context) int { r.pending.Store(true); return 1 }
func (r *restarter) CancelRestart(context.Context) bool { return r.pending.Swap(false) }
func (r *restarter) RestartPending() bool               { return r.pending.Load() }
func (r *restarter) Running() int                       { return 1 }

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
	s := &web.Server{
		Store: st, Actions: actions{}, Repos: []string{"acme/api", "acme/web"}, DailyCap: 25, AwaitCap: 5, JobsDir: jobs,
		ConfigPath: cfgPath, Listen: *listen, Restarter: &restarter{}, StartedConfig: web.ConfigVersion([]byte(sampleConfig)),
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
	log.Printf("dashboard on http://%s/app/", *listen)
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
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
		{"acme/api", 110, "Rewrite the job scheduler", state.Blocked, "L"}, // over the cost limit
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
			}
			if to == state.Blocked {
				reason = "Waits on #102, which changes the same client."
				if x.issue == 110 {
					reason = "Waiting for your OK on the cost: estimated $9.40–$16.20, over the $10.00 limit. Add factory:go or approve it here."
				}
			}
			if err := st.SetStage(ctx, it.ID, to, "daemon", reason); err != nil {
				return err
			}
		}
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
		lines := []string{
			`{"type":"system","subtype":"init"}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the parser and its tests."}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./parser"}}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"The loop stops one token early when the input lacks a trailing newline. Writing a failing test first."}]}}`,
		}
		if end == state.Running && kind == "implement" {
			lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/webhook/retry.go"}}]}}`)
		}
		if end != state.Running {
			lines = append(lines, `{"type":"result","subtype":"success","result":"Fixed and tested."}`)
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
