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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
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
	jobs := filepath.Join(dir, "jobs")
	if err := seed(context.Background(), st, jobs); err != nil {
		log.Fatal(err)
	}
	s := &web.Server{
		Store: st, Actions: actions{}, Repo: "acme/widgets", DailyCap: 25, AwaitCap: 5, JobsDir: jobs,
		Issue: func(_ context.Context, n int) (*github.Issue, error) {
			return &github.Issue{Number: n, Body: fmt.Sprintf("Issue #%d.\n\nThe parser drops the last token when the input has no trailing newline.\n\nExpected: every token is kept.\nVerify: `go test ./parser`.", n)}, nil
		},
		Checks: func(_ context.Context, sha string) ([]github.CheckRun, error) {
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
	log.Fatal(srv.ListenAndServe())
}

func seed(ctx context.Context, st *store.Store, jobs string) error {
	const repo = "acme/widgets"
	path := map[state.Stage][]state.Stage{
		state.Triage:        {},
		state.Ready:         {state.Ready},
		state.Implementing:  {state.Ready, state.Implementing},
		state.Reviewing:     {state.Ready, state.Implementing, state.Reviewing},
		state.AwaitingHuman: {state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman},
		state.Parked:        {state.Parked},
		state.Done:          {state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done},
	}
	items := []struct {
		issue int
		title string
		stage state.Stage
		size  string
	}{
		{101, "Parser drops the last token without a trailing newline", state.AwaitingHuman, "S"},
		{102, "Add retry with backoff to the webhook client", state.Implementing, "M"},
		{103, "Rate-limit the public search endpoint", state.Reviewing, "M"},
		{104, "Support YAML anchors in config files", state.Parked, "L"},
		{105, "Typo in the CLI help for --dry-run", state.Ready, "S"},
		{106, "Flaky TestCacheEviction on CI", state.Triage, ""},
		{107, "Export metrics in OpenMetrics format", state.Done, "M"},
	}
	ids := map[int]int64{}
	for i, x := range items {
		it, _, err := st.UpsertItem(ctx, repo, x.issue, x.title)
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
	rev, err := job(101, "review", "gpt-5", state.Succeeded, 0.61, review)
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
	if _, err := job(102, "implement", "sonnet", state.Running, 0, ""); err != nil {
		return err
	}
	if _, err := job(103, "implement", "sonnet", state.Failed, 2.1, ""); err != nil {
		return err
	}
	if _, err := job(103, "implement", "opus", state.Succeeded, 4.3, ""); err != nil {
		return err
	}
	if _, err := job(103, "review", "gpt-5", state.Running, 0, ""); err != nil {
		return err
	}
	if _, err := job(107, "implement", "sonnet", state.Succeeded, 2.2, ""); err != nil {
		return err
	}
	if _, err := job(107, "review", "gpt-5", state.Succeeded, 0.5, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`); err != nil {
		return err
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

func ptr(v int64) *int64 { return &v }
