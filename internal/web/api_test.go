package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// TestOpenAPIMatchesGo fails when openapi.yaml and the Go types drift: the
// frontend's types come from the spec, so a field the Go side sends but
// the spec lacks (or the reverse) would be a silent break.
func TestOpenAPIMatchesGo(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	schemas := spec.Components.Schemas
	types := map[string]any{
		"Overview": APIOverview{}, "Item": APIItem{}, "Card": APICard{}, "Column": APIColumn{}, "Board": APIBoard{},
		"Job": APIJob{}, "Gate": APIGate{}, "Event": APIEvent{}, "ItemDetail": APIItemDetail{}, "JobDetail": APIJobDetail{},
		"Finding": APIFinding{}, "Review": APIReview{}, "Check": APICheck{}, "PRPanel": APIPRPanel{}, "Diff": APIDiff{},
		"Feed": APIFeed{}, "Activity": APIActivity{}, "ActivityEntry": APIActivityEntry{}, "DaySpend": APIDaySpend{}, "ModelSpend": APIModelSpend{}, "Spend": APISpend{},
		"Notification": APINotification{}, "Notifications": APINotifications{}, "ActionResult": APIActionResult{},
		"Error": APIError{}, "Metrics": metrics.Report{}, "Group": metrics.Group{}, "Week": metrics.Week{},
		"Settings": APISettings{}, "SettingsGroup": APISettingsGroup{}, "Setting": APISetting{},
	}
	for name, v := range types {
		sc, ok := schemas[name]
		if !ok {
			t.Errorf("openapi.yaml has no schema %s", name)
			continue
		}
		props, required := sc.flatten(schemas)
		goFields := jsonFields(reflect.TypeOf(v))
		if got, want := strings.Join(props, ","), strings.Join(goFields, ","); got != want {
			t.Errorf("%s: spec properties %s, Go fields %s", name, got, want)
		}
		if got, want := strings.Join(required, ","), strings.Join(goFields, ","); got != want {
			t.Errorf("%s: every property is always sent, so all must be required; required = %s", name, got)
		}
	}
}

type schema struct {
	Ref        string            `yaml:"$ref"`
	AllOf      []schema          `yaml:"allOf"`
	Properties map[string]schema `yaml:"properties"`
	Required   []string          `yaml:"required"`
}

func (s schema) flatten(all map[string]schema) (props, required []string) {
	if s.Ref != "" {
		return all[strings.TrimPrefix(s.Ref, "#/components/schemas/")].flatten(all)
	}
	for _, sub := range s.AllOf {
		p, r := sub.flatten(all)
		props, required = append(props, p...), append(required, r...)
	}
	for k := range s.Properties {
		props = append(props, k)
	}
	required = append(required, s.Required...)
	sort.Strings(props)
	sort.Strings(required)
	return props, required
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" || name == "" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func apiSetup(t *testing.T) (string, *store.Store, *fakeActions, *Server) {
	t.Helper()
	srv, st, fa, _ := setup(t)
	ctx := context.Background()
	// A second item that passed review, with a saved diff and CI checks.
	it, _, _ := st.UpsertItem(ctx, "o/r", 2, "Fix the parser")
	pr, branch := 9, "factory/issue-2"
	st.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &pr, Branch: &branch})
	impl, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 2})
	st.ClaimJob(ctx)
	st.SetJobHead(ctx, impl, "abc123")
	st.SetGateResults(ctx, impl, []store.GateResult{{Gate: "test", Status: "pass"}})
	st.FinishJob(ctx, impl, store.Result{State: state.Succeeded, CostUSD: 1})
	rev, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "gpt", BudgetUSD: 2})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, rev, store.Result{State: state.Succeeded, CostUSD: 0.25,
		Verdict: `{"decision":"pass","findings":[{"severity":"minor","file":"p.go","line":3,"claim":"off by one","evidence":"i <= n"}],"challenged_assumptions":[]}`})
	for _, to := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman} {
		if err := st.SetStage(ctx, it.ID, to, "daemon", "test"); err != nil {
			t.Fatal(err)
		}
	}
	jobs := t.TempDir()
	os.MkdirAll(filepath.Join(jobs, itoa(rev)), 0o755)
	os.WriteFile(filepath.Join(jobs, itoa(rev), "diff.patch"), []byte("diff --git a/p.go b/p.go\n+i <= n\n"), 0o644)
	st.AddEvent(ctx, store.Event{Actor: "orchestrator", ItemID: &it.ID, Type: "set_priority", Reason: "unblocks two items"})

	s := &Server{Store: st, Actions: fa, Repos: []string{"o/r"}, DailyCap: 1, AwaitCap: 5, pollPeriod: 10 * time.Millisecond, JobsDir: jobs,
		Issue: func(_ context.Context, _ string, n int) (*github.Issue, error) {
			return &github.Issue{Number: n, Body: "The parser drops the last token."}, nil
		},
		Checks: func(_ context.Context, _, sha string) ([]github.CheckRun, error) {
			return []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "success", URL: "https://ci/" + sha}}, nil
		},
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = h
	return srv.URL, st, fa, s
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(get(t, u)), v); err != nil {
		t.Fatalf("%s: %v", u, err)
	}
}

func TestAPIReads(t *testing.T) {
	base, _, _, _ := apiSetup(t)
	var b APIBoard
	getJSON(t, base+"/api/v1/board", &b)
	var awaiting, parked APIColumn
	for _, c := range b.Columns {
		switch c.Stage {
		case "awaiting_human":
			awaiting = c
		case "parked":
			parked = c
		}
	}
	if len(b.Columns) != len(boardStages) || len(awaiting.Cards) != 1 || len(parked.Cards) != 1 {
		t.Fatalf("board = %+v", b)
	}
	if c := awaiting.Cards[0]; c.CostUSD != 1.25 || c.Model != "sonnet" || !c.NeedsYou || c.PRURL != "https://github.com/o/r/pull/9" ||
		c.GatesPassed != 1 || c.GatesTotal != 1 || c.Attempt != 1 || c.Live {
		t.Fatalf("card = %+v", c)
	}
	if c := parked.Cards[0]; c.BlockedNote != "needs a human" {
		t.Fatalf("parked card = %+v", c)
	}

	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/2", &d)
	if d.IssueBody != "The parser drops the last token." || len(d.Jobs) != 2 || d.Jobs[0].Kind != "implement" {
		t.Fatalf("item = %+v", d)
	}

	var p APIPRPanel
	getJSON(t, base+"/api/v1/items/2/pr", &p)
	if p.HeadSHA != "abc123" || len(p.Checks) != 1 || p.Checks[0].URL != "https://ci/abc123" || len(p.Gates) != 1 ||
		p.Review == nil || p.Review.Decision != "pass" || len(p.Review.Findings) != 1 || p.LastJobID == nil {
		t.Fatalf("pr panel = %+v", p)
	}

	var diff APIDiff
	getJSON(t, base+"/api/v1/items/2/diff", &diff)
	if !strings.Contains(diff.Patch, "+i <= n") || len(diff.Findings) != 1 || diff.Findings[0].Line != 3 {
		t.Fatalf("diff = %+v", diff)
	}
	if resp, _ := http.Get(base + "/api/v1/items/1/diff"); resp.StatusCode != 404 {
		t.Fatalf("an item with no review has no diff, got %d", resp.StatusCode)
	}

	var f APIFeed
	getJSON(t, base+"/api/v1/orchestrator?item=2", &f)
	if len(f.Events) != 1 || f.Events[0].Reason != "unblocks two items" {
		t.Fatalf("feed = %+v", f)
	}
	getJSON(t, base+"/api/v1/orchestrator?item=1", &f)
	if len(f.Events) != 0 {
		t.Fatalf("filtered feed = %+v", f)
	}

	var act APIActivity
	getJSON(t, base+"/api/v1/activity", &act)
	var lines []string
	for _, e := range act.Entries {
		lines = append(lines, e.Ref+" "+e.Text+" ("+e.Tone+")")
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"#2 PR #9 is ready for you (needs_you)", "#2 passed its gates; review started (live)", "#2 started building with sonnet (live)",
		"#2 codex started reviewing (live)", "#2 unblocks two items (info)"} {
		if !strings.Contains(got, want) {
			t.Errorf("activity lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "job_queued") || act.Entries[0].Text != "unblocks two items" || act.Entries[0].Actor != "orchestrator" {
		t.Errorf("activity:\n%s", got)
	}

	getJSON(t, base+"/api/v1/activity?repo=other/repo", &act)
	for _, e := range act.Entries {
		if e.Ref != "" {
			t.Fatalf("another repo's feed has %+v", e)
		}
	}

	var n APINotifications
	getJSON(t, base+"/api/v1/notifications", &n)
	kinds := map[string]bool{}
	for _, x := range n.Notifications {
		kinds[x.Kind] = true
	}
	// $1.75 spent against a $1 cap.
	if !kinds["ready_for_review"] || !kinds["needs_input"] || !kinds["budget"] {
		t.Fatalf("notifications = %+v", n)
	}

	var sp APISpend
	getJSON(t, base+"/api/v1/spend", &sp)
	if len(sp.Days) != 30 || sp.WeekUSD != 1.75 || len(sp.ByModel) != 2 {
		t.Fatalf("spend = %+v", sp)
	}
	var m metrics.Report
	getJSON(t, base+"/api/v1/metrics", &m)
	if m.Items != 2 || m.Weekly == nil {
		t.Fatalf("metrics = %+v", m)
	}
	var o APIOverview
	getJSON(t, base+"/api/v1/overview", &o)
	if o.Awaiting != 1 || o.Version == "" || o.SpentTodayUSD != 1.75 {
		t.Fatalf("overview = %+v", o)
	}
	if !strings.Contains(get(t, base+"/api/v1/openapi.yaml"), "openapi: 3.1.0") {
		t.Fatal("spec not served")
	}
	if resp, _ := http.Get(base + "/api/v1/nope"); resp.StatusCode != 404 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatal("unknown endpoints must 404 as JSON")
	}
}

func TestAPIActionsNeedSameOriginJSON(t *testing.T) {
	base, _, fa, _ := apiSetup(t)
	post := func(path, ct, origin, body string) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/api/v1/jobs/1/retry", "text/plain", "", `{"tier_up":true}`); c != http.StatusUnsupportedMediaType {
		t.Fatalf("a form-style POST must be refused, got %d", c)
	}
	if c := post("/api/v1/jobs/1/retry", "application/json", "https://evil.example", `{}`); c != http.StatusForbidden {
		t.Fatalf("a cross-origin POST must be refused, got %d", c)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("refused requests reached the daemon: %v", fa.calls)
	}
	if c := post("/api/v1/jobs/1/retry", "application/json", base, `{"tier_up":true}`); c != 200 {
		t.Fatalf("retry = %d", c)
	}
	if c := post("/api/v1/items/2/park", "application/json", "", `{"reason":"needs a decision"}`); c != 200 {
		t.Fatalf("park = %d", c)
	}
	if c := post("/api/v1/jobs/1/cancel", "application/json", "", ``); c != 200 {
		t.Fatalf("cancel = %d", c)
	}
	if strings.Join(fa.calls, "|") != "retry|park:needs a decision|cancel" {
		t.Fatalf("calls = %v", fa.calls)
	}
}

func TestAPIStreamAndLog(t *testing.T) {
	base, st, _, _ := apiSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	versions := 0
	for sc.Scan() && versions < 2 {
		if strings.HasPrefix(sc.Text(), "event: version") {
			versions++
			if versions == 1 {
				st.AddEvent(ctx, store.Event{Actor: "daemon", Type: "note"})
			}
		}
	}
	if versions != 2 {
		t.Fatal("a new event must produce a second version event")
	}

	body := get(t, base+"/api/v1/jobs/1/log")
	if !strings.Contains(body, `event: line`) || !strings.Contains(body, `"text":"writing \u003cb\u003etests\u003c/b\u003e"`) ||
		!strings.Contains(body, `event: done`+"\n"+`data: {"state":"failed"}`) {
		t.Fatalf("log stream = %q", body)
	}
}

func TestTerminal(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, _, _, s := apiSetup(t)
	s.Exec = func(ctx context.Context, id int64) (*exec.Cmd, error) { return exec.CommandContext(ctx, "sh"), nil }
	h, _ := s.Handler()
	srv := httpServer(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(srv, "http") + "/api/v1/jobs/1/terminal"

	// Another site's page cannot attach.
	if _, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}}); err == nil {
		t.Fatal("a cross-origin WebSocket must be refused")
	}
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	msg, _ := json.Marshal(termMsg{Type: "input", Data: "echo hi-$((40+2))\n"})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "hi-42") {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, out.String())
		}
		out.Write(b)
	}
}

func httpServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAppServesIndexForRoutes(t *testing.T) {
	base, _, _, _ := apiSetup(t)
	for _, p := range []string{"/app/", "/app/items/2", "/app/board"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
			!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatalf("%s: %d %v", p, resp.StatusCode, resp.Header)
		}
	}
	if resp, _ := http.Get(base + "/app/assets/missing.js"); resp.StatusCode != 404 {
		t.Fatalf("a missing asset must 404, got %d", resp.StatusCode)
	}
}

// A critic that blocks after the reviewer passed is what the PR panel and
// the diff viewer must show, both from the same job.
func TestPanelAndDiffFollowTheLatestVerdict(t *testing.T) {
	base, st, _, s := apiSetup(t)
	ctx := context.Background()
	itemID := int64(2)
	crit, _ := st.EnqueueJob(ctx, store.Job{ItemID: &itemID, Kind: "critic", Harness: "claude", Model: "opus", BudgetUSD: 2})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, crit, store.Result{State: state.Succeeded,
		Verdict: `{"decision":"block","findings":[{"severity":"critical","file":"q.go","line":7,"claim":"races","evidence":"x"}],"challenged_assumptions":[]}`})
	os.MkdirAll(filepath.Join(s.JobsDir, itoa(crit)), 0o755)
	os.WriteFile(filepath.Join(s.JobsDir, itoa(crit), "diff.patch"), []byte("diff --git a/q.go b/q.go\n"), 0o644)
	// A newer review still running has saved its diff but has no verdict yet.
	rev, _ := st.EnqueueJob(ctx, store.Job{ItemID: &itemID, Kind: "review", Harness: "codex", Model: "gpt", BudgetUSD: 2})
	st.ClaimJob(ctx)
	os.MkdirAll(filepath.Join(s.JobsDir, itoa(rev)), 0o755)
	os.WriteFile(filepath.Join(s.JobsDir, itoa(rev), "diff.patch"), []byte("diff --git a/r.go b/r.go\n"), 0o644)

	var p APIPRPanel
	getJSON(t, base+"/api/v1/items/2/pr", &p)
	if p.Review == nil || p.Review.JobID != crit || p.Review.Decision != "block" {
		t.Fatalf("panel review = %+v", p.Review)
	}
	var d APIDiff
	getJSON(t, base+"/api/v1/items/2/diff", &d)
	if d.JobID != crit || len(d.Findings) != 1 || d.Findings[0].File != "q.go" {
		t.Fatalf("diff = %+v", d)
	}
}

func TestNotificationIDsSurviveItemEdits(t *testing.T) {
	base, st, _, _ := apiSetup(t)
	ids := func() map[string]bool {
		var n APINotifications
		getJSON(t, base+"/api/v1/notifications", &n)
		m := map[string]bool{}
		for _, x := range n.Notifications {
			m[x.ID] = true
		}
		return m
	}
	before := ids()
	st.SetPriority(context.Background(), 2, 9)
	after := ids()
	if len(before) != len(after) {
		t.Fatal()
	}
	for id := range before {
		if !after[id] {
			t.Fatalf("a priority change re-raised %s: %v", id, after)
		}
	}
}

// A running job's card says what its log shows it doing now.
func TestLiveActivity(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log.jsonl")
	write := func(lines ...string) {
		os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	write(`{"type":"system","subtype":"init"}`)
	if a := liveActivity(log); a != "" {
		t.Fatalf("before any tool: %q", a)
	}
	write(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...\necho done"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Fixing it."}]}}`)
	if a := liveActivity(log); a != "running go test ./..." {
		t.Fatalf("bash: %q", a)
	}
	write(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/internal/webhook/retry.go"}}]}}`)
	if a := liveActivity(log); a != "editing webhook/retry.go" {
		t.Fatalf("edit: %q", a)
	}
	write(`{"type":"item.started","item":{"type":"command_execution","command":"npm test"}}`)
	if a := liveActivity(log); a != "running npm test" {
		t.Fatalf("codex: %q", a)
	}
	// A tool result bigger than the first window read doesn't hide the call before it.
	big := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + strings.Repeat("x", 200<<10) + `"}]}}`
	write(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test -v ./..."}}]}}`, big)
	if a := liveActivity(log); a != "running go test -v ./..." {
		t.Fatalf("after a big line: %q", a)
	}
	if got := trim("ééé", 3); got != "é…" {
		t.Fatalf("trim cut a character: %q", got)
	}
	if a := liveActivity(filepath.Join(t.TempDir(), "none")); a != "" {
		t.Fatalf("no log: %q", a)
	}
}

// A card shows the latest attempt's gates, not an earlier attempt's.
func TestCardGatesFollowTheLatestAttempt(t *testing.T) {
	base, st, _, _ := apiSetup(t)
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 3, "Retry it")
	first, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 2})
	st.ClaimJob(ctx)
	st.SetGateResults(ctx, first, []store.GateResult{{Gate: "test", Status: "fail"}, {Gate: "lint", Status: "pass"}})
	st.FinishJob(ctx, first, store.Result{State: state.Failed})
	second, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "opus", BudgetUSD: 2, Attempt: 2})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, second, store.Result{State: state.TimedOut})
	st.SetStage(ctx, it.ID, state.Ready, "daemon", "")
	var b APIBoard
	getJSON(t, base+"/api/v1/board", &b)
	found := false
	for _, col := range b.Columns {
		for _, c := range col.Cards {
			if c.ID == it.ID {
				found = true
				if c.Attempt != 2 || c.GatesTotal != 0 {
					t.Fatalf("card = %+v", c)
				}
			}
		}
	}
	if !found {
		t.Fatal("no card for the item")
	}
}

// A workspace change lists its PRs in merge order, and its main PR is the
// one in the item's own repo.
func TestWorkspacePRs(t *testing.T) {
	base, st, _, _ := apiSetup(t)
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/web", 4, "Show totals")
	n := 8
	st.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &n})
	st.SetItemPRs(ctx, it.ID, []store.RepoPR{{Repo: "o/api", Number: 5}, {Repo: "o/web", Number: 8}})
	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/"+itoa(it.ID), &d)
	a := d.Item
	if len(a.PRs) != 2 || a.PRs[0].Ref != "api#5" || a.PRs[0].URL != "https://github.com/o/api/pull/5" || a.PRURL != "https://github.com/o/web/pull/8" {
		t.Fatalf("item = %+v", a)
	}
	var p APIPRPanel
	getJSON(t, base+"/api/v1/items/"+itoa(it.ID)+"/pr", &p)
	if len(p.PRs) != 2 || p.PRURL != a.PRURL {
		t.Fatalf("panel = %+v", p)
	}
}
