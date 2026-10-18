package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

type fakeActions struct{ calls []string }

func (f *fakeActions) Retry(_ context.Context, id int64, up bool) error {
	f.calls = append(f.calls, "retry")
	return nil
}
func (f *fakeActions) Cancel(context.Context, int64) error {
	f.calls = append(f.calls, "cancel")
	return nil
}
func (f *fakeActions) Park(_ context.Context, id int64, reason string) error {
	f.calls = append(f.calls, "park:"+reason)
	return nil
}
func (f *fakeActions) ApproveEstimate(_ context.Context, id int64) error {
	f.calls = append(f.calls, "approve-estimate")
	return nil
}

func setup(t *testing.T) (*httptest.Server, *store.Store, *fakeActions, int64) {
	t.Helper()
	dir := t.TempDir()
	st := storetest.Open(t, filepath.Join(dir, "f.db"))
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 1, "Add <script>alert(1)</script>")
	st.SetStage(ctx, it.ID, state.Parked, "daemon", "needs a human")
	id, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 2})
	st.ClaimJob(ctx)
	log := filepath.Join(dir, "log.jsonl")
	os.WriteFile(log, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"writing <b>tests</b>"}]}}`+"\n"), 0o644)
	st.SetJobRuntime(ctx, id, "c", log, "")
	st.SetGateResults(ctx, id, []store.GateResult{{Gate: "test", Status: "fail", Detail: "boom"}})
	st.FinishJob(ctx, id, store.Result{State: state.Failed, CostUSD: 0.5, Error: "gates failed"})
	fa := &fakeActions{}
	cmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(cmpDir, "5-run"), 0o755)
	os.WriteFile(filepath.Join(cmpDir, "5-run", "report.json"), []byte(`{"issue":5,"title":"Add it","arms":[{"skillset":"tdd","stage":"awaiting_human","cost_usd":1.5},{"skillset":"none","stage":"parked"}]}`), 0o644)
	os.WriteFile(filepath.Join(cmpDir, "5-run", "report.md"), []byte("# Skill comparison: <b>#5</b>"), 0o644)
	s := &Server{Store: st, Actions: fa, Repos: []string{"o/r"}, DailyCap: 60, AwaitCap: 5, pollPeriod: 10 * time.Millisecond, CompareDir: cmpDir}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, st, fa, id
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", u, resp.StatusCode, b)
	}
	return string(b)
}

func TestPagesRender(t *testing.T) {
	t.Parallel()
	srv, _, _, id := setup(t)
	pages := map[string][]string{
		"/":                 {"Needs you", "needs a human"},
		"/board":            {"parked (1)"},
		"/items/1":          {"Audit trail", "implement"},
		"/jobs/" + itoa(id): {"gates failed", "boom", "sse-connect"},
		"/orchestrator":     {"No orchestrator actions yet"},
		"/spend":            {"$0.50"},
		"/metrics":          {"Cost per merged PR", "First-pass rate"},
		"/metrics.json":     {"\"cost_per_merged_usd\""},
		"/compare":          {"#5 Add it", "tdd:</span> passed $1.50", "none:</span> parked"},
		"/compare/5-run":    {"Skill comparison: &lt;b&gt;#5"},
	}
	for path, wants := range pages {
		body := get(t, srv.URL+path)
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s missing %q", path, w)
			}
		}
		if strings.Contains(body, "<script>alert(1)") {
			t.Errorf("%s does not escape issue titles", path)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestStreamSendsEscapedLinesThenDone(t *testing.T) {
	t.Parallel()
	srv, _, _, id := setup(t)
	body := get(t, srv.URL+"/jobs/"+itoa(id)+"/stream")
	if !strings.Contains(body, "event: line") || !strings.Contains(body, "writing &lt;b&gt;tests&lt;/b&gt;") || !strings.Contains(body, "event: done") {
		t.Fatalf("stream = %q", body)
	}
}

func TestActionsAndCrossSiteRefusal(t *testing.T) {
	t.Parallel()
	srv, _, fa, id := setup(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(srv.URL+"/items/1/park", url.Values{"reason": {"later"}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("park: %v %v", err, resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/jobs/"+itoa(id)+"/retry", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = client.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST got %d", resp.StatusCode)
	}
	// A DNS-rebinding page reaches the server under its own name.
	req, _ = http.NewRequest("GET", srv.URL+"/board", nil)
	req.Host = "evil.example"
	if resp, _ = client.Do(req); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host got %d", resp.StatusCode)
	}
	if len(fa.calls) != 1 || fa.calls[0] != "park:later" {
		t.Fatalf("calls = %v", fa.calls)
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()
	if got := summarize(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`); got != "→ go test ./..." {
		t.Fatalf("got %q", got)
	}
	if got := summarize(`{"type":"result","subtype":"success","result":"done"}`); !strings.Contains(got, "done") {
		t.Fatalf("got %q", got)
	}
}
