package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func all(lines string) []APITranscriptEntry {
	var out []APITranscriptEntry
	for _, l := range strings.Split(strings.TrimSpace(lines), "\n") {
		out = append(out, entries(l)...)
	}
	return out
}

func TestClaudeTranscript(t *testing.T) {
	t.Parallel()
	es := all(`{"type":"system","subtype":"init","model":"claude-sonnet-4-5","tools":["Bash","Read","Edit"],"session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"where is the parser?"},{"type":"text","text":"I'll look at **the parser** first."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/work/app/models/user.rb"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"bin/rails test\necho done","description":"Run the tests"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"class User\nend"},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"1 failure"}],"is_error":true}]}}
{"type":"user","message":{"role":"user","content":"also fix the README"}}
{"type":"factory_message","from":"you","text":"also fix the README","delivered":true}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"mcp__factory__status","input":{}},{"type":"tool_use","id":"t4","name":"Grep","input":{"pattern":"TODO","path":"/work/lib"}}]}}
{"type":"rate_limit_event","rate_limit_info":{}}
{"type":"system","subtype":"hook_response"}
{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":80,"total_cost_usd":4.5,"result":""}`)
	kinds := []string{}
	for _, e := range es {
		kinds = append(kinds, e.Kind)
	}
	want := "init,thinking,text,tool,tool,tool_result,tool_result,you,tool,tool,outcome"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("kinds = %s", strings.Join(kinds, ","))
	}
	if es[0].Model != "claude-sonnet-4-5" || es[0].Tools != 3 {
		t.Fatalf("init = %+v", es[0])
	}
	if es[3].Summary != "Read app/models/user.rb" || es[3].ToolID != "t1" {
		t.Fatalf("read = %+v", es[3])
	}
	if es[4].Summary != "Ran bin/rails test" || !strings.Contains(es[4].Input, "# Run the tests") {
		t.Fatalf("bash = %+v", es[4])
	}
	if es[5].Output != "class User\nend" || es[6].Output != "1 failure" || !es[6].IsError || es[6].ToolID != "t2" {
		t.Fatalf("results = %+v %+v", es[5], es[6])
	}
	if es[7].Text != "also fix the README" || !es[7].Delivered {
		t.Fatalf("you = %+v", es[7])
	}
	if es[8].Summary != "Called factory.status" || es[9].Summary != `Searched for "TODO" in lib` {
		t.Fatalf("tools = %q %q", es[8].Summary, es[9].Summary)
	}
	o := es[10]
	if !o.IsError || o.Turns != 80 || o.CostUSD != 4.5 || !strings.Contains(o.Summary, "ran out of turns (80)") {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestCodexTranscript(t *testing.T) {
	t.Parallel()
	es := all(`{"type":"thread.started","thread_id":"t"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"0","type":"reasoning","text":"thinking it over"}}
{"type":"item.started","item":{"id":"1","type":"command_execution","command":"bash -lc 'go test ./...'","status":"in_progress"}}
{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"bash -lc 'go test ./...'","aggregated_output":"FAIL","exit_code":1,"status":"failed"}}
{"type":"item.completed","item":{"id":"2","type":"file_change","changes":[{"path":"/work/a.go","kind":"update"},{"path":"/work/b.go","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"3","type":"mcp_tool_call","server":"factory","tool":"status","arguments":{"x":1},"status":"failed","error":{"message":"refused"}}}
{"type":"item.completed","item":{"id":"4","type":"agent_message","text":"Done."}}
{"type":"turn.completed","usage":{"input_tokens":1}}
{"type":"turn.failed","error":{"message":"stream disconnected"}}`)
	kinds := []string{}
	for _, e := range es {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "init,thinking,tool,tool_result,tool_result,tool_result,text,error" {
		t.Fatalf("kinds = %s", got)
	}
	if es[2].Summary != "Ran go test ./..." || es[2].ToolID != "1" {
		t.Fatalf("call = %+v", es[2])
	}
	if r := es[3]; r.Output != "FAIL" || !r.IsError || r.Summary != "Ran go test ./..." {
		t.Fatalf("result = %+v", r)
	}
	if es[4].Summary != "Edited 2 files" || !strings.Contains(es[4].Input, "add /work/b.go") {
		t.Fatalf("file change = %+v", es[4])
	}
	if es[5].Summary != "Called factory.status" || !es[5].IsError || es[5].Output != "refused" {
		t.Fatalf("mcp = %+v", es[5])
	}
	if es[7].Text != "stream disconnected" {
		t.Fatalf("error = %+v", es[7])
	}
}

func TestLongOutputIsCut(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("é", maxOutput)
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": big}}}})
	es := entries(string(b))
	if len(es) != 1 || !es[0].Truncated || len(es[0].Output) > maxOutput || !strings.HasSuffix(es[0].Output, "é") {
		t.Fatalf("cut = %d bytes, truncated %v", len(es[0].Output), es[0].Truncated)
	}
}

// A stopped build: its job page has the stop, the saved work and the
// choices; Continue and Steer reach the daemon, and adding budget needs
// the password.
func TestStoppedJobAPI(t *testing.T) {
	t.Parallel()
	base, st, fa, s := apiSetup(t)
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 30, "Big change")
	id, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 6})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, id, store.Result{State: state.Failed, Error: "Stopped: it ran out of turns (80 of 80) before it finished. Its work is kept."})
	st.SetStage(ctx, it.ID, state.Parked, "daemon", "Build job stopped. Continue picks up where it stopped.")
	dir := filepath.Join(s.JobsDir, itoa(id))
	os.MkdirAll(filepath.Join(dir, "kept"), 0o755)
	os.WriteFile(filepath.Join(dir, "kept", "kept.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "stop.json"), []byte(`{"reason":"turns","summary":"Stopped: it ran out of turns (80 of 80) before it finished.","turns":80,"max_turns":80,"note":"Left: tests","kept":true}`), 0o644)
	os.WriteFile(filepath.Join(dir, "saved.patch"), []byte("diff --git a/a.go b/a.go\n+half\n"), 0o644)
	s.Choices = func(_ context.Context, jobID int64) []APIChoice {
		return []APIChoice{{Action: "continue", Label: "Continue", Model: "claude/sonnet", Explain: "Picks up where it stopped.", Available: true},
			{Action: "park", Label: "Park", Why: "It's parked now."}}
	}

	var d APIJobDetail
	getJSON(t, base+"/api/v1/jobs/"+itoa(id), &d)
	if d.Stop == nil || !d.Stop.Kept || !d.Stop.SavedDiff || d.Stop.Note != "Left: tests" || len(d.Choices) != 2 {
		t.Fatalf("detail = %+v", d)
	}
	var diff APIDiff
	getJSON(t, base+"/api/v1/jobs/"+itoa(id)+"/saved-diff", &diff)
	if !strings.Contains(diff.Patch, "+half") {
		t.Fatalf("saved diff = %+v", diff)
	}
	var item APIItemDetail
	getJSON(t, base+"/api/v1/items/"+itoa(it.ID), &item)
	if item.Item.ChoiceJobID == nil || *item.Item.ChoiceJobID != id || len(item.Item.Choices) != 2 {
		t.Fatalf("item choices = %+v %v", item.Item.Choices, item.Item.ChoiceJobID)
	}
	var ns APINotifications
	getJSON(t, base+"/api/v1/notifications", &ns)
	found := false
	for _, n := range ns.Notifications {
		found = found || n.ChoiceJobID != nil && *n.ChoiceJobID == id && len(n.Choices) == 2
	}
	if !found {
		t.Fatalf("notifications = %+v", ns)
	}

	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if c, b := post("/api/v1/jobs/"+itoa(id)+"/continue", `{"extra":0.5,"note":"narrow it"}`); c != http.StatusForbidden || !strings.Contains(b, "password") {
		t.Fatalf("extra budget without a password = %d %s", c, b)
	}
	if c, _ := post("/api/v1/jobs/"+itoa(id)+"/continue", `{"note":"narrow it"}`); c != 200 {
		t.Fatalf("continue = %d", c)
	}
	if c, _ := post("/api/v1/jobs/"+itoa(id)+"/steer", `{"text":"hello","files":[{"name":"x.png","data":"cG5n"}]}`); c != 200 {
		t.Fatalf("steer = %d", c)
	}
	if got := strings.Join(fa.calls, "|"); got != "continue:"+itoa(id)+":0:narrow it|steer:"+itoa(id)+":hello|file:x.png:png" {
		t.Fatalf("calls = %s", got)
	}

	// The raw log downloads, and the stream carries structured entries.
	resp, err := http.Get(base + "/api/v1/jobs/1/log.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "job-1.jsonl") || !strings.Contains(string(b), `"type":"assistant"`) {
		t.Fatalf("raw log = %d %q", resp.StatusCode, b)
	}
	if body := get(t, base+"/api/v1/jobs/1/log"); !strings.Contains(body, `"entries":[{"kind":"text"`) {
		t.Fatalf("stream = %s", body)
	}
}
