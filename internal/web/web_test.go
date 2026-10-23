package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
func (f *fakeActions) ApproveEstimate(_ context.Context, id, planJob int64) error {
	f.calls = append(f.calls, "approve-estimate")
	if planJob != 0 {
		f.calls[len(f.calls)-1] += ":" + strconv.FormatInt(planJob, 10)
	}
	return nil
}
func (f *fakeActions) Replan(_ context.Context, id, planJob int64, ask string) error {
	f.calls = append(f.calls, "replan:"+strconv.FormatInt(planJob, 10)+":"+ask)
	return nil
}
func (f *fakeActions) Continue(_ context.Context, id int64, extra float64, note string) error {
	f.calls = append(f.calls, fmt.Sprintf("continue:%d:%g:%s", id, extra, note))
	return nil
}
func (f *fakeActions) Steer(_ context.Context, id int64, text string) error {
	f.calls = append(f.calls, fmt.Sprintf("steer:%d:%s", id, text))
	return nil
}
func (f *fakeActions) ResetAutoMerge(_ context.Context, repo, by string) error {
	f.calls = append(f.calls, "reset-auto-merge:"+repo+":"+by)
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
	os.WriteFile(filepath.Join(cmpDir, "5-run", "report.json"), []byte(`{"issue":5,"title":"Add it","started":"2026-10-01T10:00:00Z","arms":[{"skillset":"tdd","stage":"awaiting_human","cost_usd":1.5,"gates":[{"gate":"lint","status":"fail"}]},{"skillset":"none","stage":"parked"}]}`), 0o644)
	os.MkdirAll(filepath.Join(cmpDir, "6-later"), 0o755)
	os.WriteFile(filepath.Join(cmpDir, "6-later", "report.json"), []byte(`{"issue":6,"title":"Later","started":"2026-10-02T10:00:00Z","arms":[]}`), 0o644)
	os.MkdirAll(filepath.Join(cmpDir, "7-broken"), 0o755)
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

func TestRootLeadsToTheDashboard(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := setup(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/app/" {
		t.Fatalf("/ = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if body := get(t, srv.URL+"/metrics.json"); !strings.Contains(body, `"cost_per_merged_usd"`) {
		t.Fatalf("/metrics.json = %s", body)
	}
	// A DNS-rebinding page reaches the server under its own name.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/board", nil)
	req.Host = "evil.example"
	if resp, _ = noFollow.Do(req); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host got %d", resp.StatusCode)
	}
}

func TestCompareListsRunsNewestFirst(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := setup(t)
	var c APIComparisons
	getJSON(t, srv.URL+"/api/v1/compare", &c)
	if len(c.Runs) != 2 || c.Runs[0].ID != "6-later" || c.Runs[1].ID != "5-run" {
		t.Fatalf("runs = %+v", c.Runs)
	}
	r := c.Runs[1]
	if r.Ref != "#5" || r.Title != "Add it" || len(r.Arms) != 2 {
		t.Fatalf("run = %+v", r)
	}
	if a := r.Arms[0]; a.Skillset != "tdd" || a.Stage != "awaiting_human" || a.CostUSD != 1.5 || len(a.Gates) != 1 || a.Gates[0].Status != "fail" {
		t.Fatalf("arm = %+v", a)
	}
	if a := r.Arms[1]; a.Gates == nil || a.Findings == nil {
		t.Fatalf("empty lists should be [], not null: %+v", a)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestSummarize(t *testing.T) {
	t.Parallel()
	if got := summarize(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`); got != "→ go test ./..." {
		t.Fatalf("got %q", got)
	}
	if got := summarize(`{"type":"result","subtype":"success","result":"done"}`); !strings.Contains(got, "done") {
		t.Fatalf("got %q", got)
	}
}
