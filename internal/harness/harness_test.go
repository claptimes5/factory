package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

type fakeRunner struct {
	out  string
	code int
	spec runner.Spec
}

func (f *fakeRunner) Paths(runner.Spec) runner.Paths {
	return runner.Paths{Work: "/work", Out: "/out", Home: "/home/agent"}
}
func (f *fakeRunner) Run(ctx context.Context, s runner.Spec, w io.Writer) (int, error) {
	f.spec = s
	w.Write([]byte(f.out))
	return f.code, nil
}
func (f *fakeRunner) Alive(context.Context, string) bool { return false }
func (f *fakeRunner) Kill(context.Context, string) error { return nil }
func (f *fakeRunner) KillAll(context.Context) error      { return nil }

func req(t *testing.T, readOnly bool) RunRequest {
	dir := t.TempDir()
	return RunRequest{Name: "j1", Stage: "review", Model: "m", Prompt: "hello", Workdir: dir,
		OutDir: filepath.Join(dir, "out"), ReadOnly: readOnly, Schema: `{"type":"object"}`, SkillsDir: "/skills/tdd"}
}

const claudeStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Bash"}]}}
{"type":"user","message":{"content":[]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":4,"total_cost_usd":0.42,"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":20}}
`

func TestClaudeParsesResultAndCost(t *testing.T) {
	fr := &fakeRunner{out: claudeStream}
	ev := make(chan Event, 100)
	r := req(t, false)
	res, err := NewClaude(fr).Run(context.Background(), r, ev)
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "done" || res.Turns != 4 || res.CostUSD != 0.42 || !res.CostKnown || res.TokensIn != 150 || res.TokensOut != 20 || res.IsError {
		t.Fatalf("unexpected result %+v", res)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	if !strings.Contains(cmd, "--output-format stream-json") || !strings.Contains(cmd, "bypassPermissions") {
		t.Fatalf("cmd = %s", cmd)
	}
	if len(fr.spec.Mounts) != 1 || fr.spec.Mounts[0].Container != "/home/agent/.claude/skills" || !fr.spec.Mounts[0].ReadOnly {
		t.Fatalf("skills mount = %+v", fr.spec.Mounts)
	}
	kinds := map[string]int{}
	close(ev)
	for e := range ev {
		kinds[e.Kind]++
	}
	if kinds["text"] != 1 || kinds["tool_call"] != 1 || kinds["final"] != 1 {
		t.Fatalf("events = %v", kinds)
	}
}

func TestClaudeReadOnlyUsesStructuredOutput(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"result","subtype":"success","result":"","structured_output":{"decision":"ready"},"num_turns":1,"total_cost_usd":0.01}` + "\n"}
	res, err := NewClaude(fr).Run(context.Background(), req(t, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != `{"decision":"ready"}` {
		t.Fatalf("final = %q", res.Final)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	if strings.Contains(cmd, "bypassPermissions") || !strings.Contains(cmd, "--json-schema") {
		t.Fatalf("read-only cmd = %s", cmd)
	}
}

func TestClaudeErrorSubtype(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":60,"total_cost_usd":1}` + "\n", code: 1}
	res, _ := NewClaude(fr).Run(context.Background(), req(t, false), nil)
	if !res.IsError || !strings.Contains(res.Error, "error_max_turns") {
		t.Fatalf("res = %+v", res)
	}
}

const codexStream = `{"type":"thread.started","thread_id":"t"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"git diff"}}
{"type":"item.completed","item":{"id":"2","type":"agent_message","text":"{\"decision\":\"pass\",\"findings\":[],\"challenged_assumptions\":[]}"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":300}}
`

func TestCodexParsesFinalAndUsage(t *testing.T) {
	fr := &fakeRunner{out: codexStream}
	r := req(t, true)
	r.Images = []string{"/out/preview/a.png"}
	res, err := NewCodex(fr).Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Final, `"decision":"pass"`) || res.TokensIn != 1000 || res.TokensCached != 200 || res.TokensOut != 300 || res.CostKnown {
		t.Fatalf("res = %+v", res)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	for _, want := range []string{"codex exec --json", "--sandbox read-only", "--output-schema /out/schema.json", "--image=/out/preview/a.png -"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("cmd missing %q: %s", want, cmd)
		}
	}
	if b, err := os.ReadFile(filepath.Join(r.OutDir, "schema.json")); err != nil || string(b) != `{"type":"object"}` {
		t.Fatalf("schema file: %v %s", err, b)
	}
	if fr.spec.Mounts[0].Container != "/home/agent/.codex/skills" {
		t.Fatalf("codex skills mount = %+v", fr.spec.Mounts)
	}
}

// A Codex builder in a container runs in the container's sandbox, as a
// Claude builder runs with bypassPermissions; on the host it keeps Codex's
// workspace-write, with the out directory writable for its verdict.
func TestCodexBuilderSandbox(t *testing.T) {
	for _, contained := range []bool{true, false} {
		fr := &fakeRunner{out: codexStream}
		r := req(t, false)
		r.Stage, r.Schema, r.Contained = "implement", "", contained
		if _, err := NewCodex(fr).Run(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		cmd := strings.Join(fr.spec.Cmd, " ")
		want, not := "--sandbox danger-full-access", "workspace-write"
		if !contained {
			want, not = "--sandbox workspace-write --skip-git-repo-check --cd /work --add-dir /out", "danger-full-access"
		}
		if !strings.Contains(cmd, want) || strings.Contains(cmd, not) || strings.Contains(cmd, "--output-schema") {
			t.Fatalf("contained %v: builder cmd = %s", contained, cmd)
		}
	}
	// Read-only runs keep the read-only sandbox either way.
	fr := &fakeRunner{out: codexStream}
	r := req(t, true)
	r.Contained = true
	NewCodex(fr).Run(context.Background(), r, nil)
	if cmd := strings.Join(fr.spec.Cmd, " "); !strings.Contains(cmd, "--sandbox read-only") || strings.Contains(cmd, "--add-dir") {
		t.Fatalf("read-only cmd = %s", cmd)
	}
}

// A tool call Codex couldn't make fails the run, though Codex exits 0.
func TestCodexFailedMCPCallFailsTheRun(t *testing.T) {
	// As codex exec --json 0.163.0 prints a call refused for approval.
	failed := `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"factory","tool":"queue_status","arguments":{},"result":null,"error":{"message":"MCP tool call requires approval, but approval policy is never"},"status":"failed"}}` + "\n"
	fr := &fakeRunner{out: failed + codexStream}
	r := req(t, true)
	r.Stage = "orchestrate"
	res, err := NewCodex(fr).Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Error, "factory.queue_status failed: MCP tool call requires approval") {
		t.Fatalf("res = %+v", res)
	}
	// A call that ran and whose tool reported an error is the agent's to handle.
	ok := `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"factory","tool":"enqueue","result":{"content":[{"type":"text","text":"refused"}],"isError":true},"status":"failed"}}` + "\n"
	fr = &fakeRunner{out: ok + codexStream}
	if res, _ := NewCodex(fr).Run(context.Background(), r, nil); res.IsError {
		t.Fatalf("a tool's own error failed the run: %+v", res)
	}
}

// The orchestrator's MCP bridge reaches Codex as mcp_servers overrides.
func TestCodexMCPServers(t *testing.T) {
	fr := &fakeRunner{out: codexStream}
	r := req(t, true)
	r.Stage = "orchestrate"
	r.MCPConfig = `{"mcpServers":{"factory":{"type":"stdio","command":"/opt/factory/factory","args":["mcp-bridge","/run/factory/mcp.sock"]}}}`
	if _, err := NewCodex(fr).Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	for _, want := range []string{`-c mcp_servers.factory.command="/opt/factory/factory"`, `-c mcp_servers.factory.args=["mcp-bridge","/run/factory/mcp.sock"]`,
		`-c mcp_servers.factory.default_tools_approval_mode="approve"`} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("cmd missing %q: %s", want, cmd)
		}
	}
	r.MCPConfig = `{"mcpServers":{"a.b":{"command":"x"}}}`
	if _, err := NewCodex(fr).Run(context.Background(), r, nil); err == nil {
		t.Fatal("a server name that isn't a TOML bare key was accepted")
	}
}

func TestCodexTurnFailed(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"turn.failed","error":{"message":"rate limited"}}` + "\n", code: 1}
	res, _ := NewCodex(fr).Run(context.Background(), req(t, true), nil)
	if !res.IsError || !strings.Contains(res.Error, "rate limited") {
		t.Fatalf("res = %+v", res)
	}
}

func TestClaudeResumeAndSessionMount(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"system","subtype":"init","session_id":"abc-123"}` + "\n" + claudeStream}
	r := req(t, false)
	r.Resume, r.SessionDir = "prev-9", t.TempDir()
	res, err := NewClaude(fr).Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID == "" {
		t.Fatal("session id not parsed")
	}
	if cmd := strings.Join(fr.spec.Cmd, " "); !strings.Contains(cmd, "--resume prev-9") {
		t.Fatalf("cmd = %s", cmd)
	}
	var mounted bool
	for _, m := range fr.spec.Mounts {
		mounted = mounted || (m.Host == r.SessionDir && m.Container == "/home/agent/.claude/projects" && !m.ReadOnly)
	}
	if !mounted {
		t.Fatalf("mounts = %+v", fr.spec.Mounts)
	}
}

func TestClaudeFastMode(t *testing.T) {
	for _, fast := range []bool{false, true} {
		fr := &fakeRunner{out: `{"type":"result","subtype":"success","result":"ok","num_turns":1,"total_cost_usd":0.01}` + "\n"}
		r := req(t, false)
		r.Fast = fast
		if _, err := NewClaude(fr).Run(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(fr.spec.Cmd, " "), `--settings {"fastMode":true}`); got != fast {
			t.Errorf("fast=%v: cmd %q", fast, fr.spec.Cmd)
		}
	}
}

func TestClaudeReadsRateLimitEvents(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1760800000,"rateLimitType":"five_hour","utilization":0.82}}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1761000000,"rateLimitType":"seven_day"}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.9}}
{"type":"result","subtype":"success","result":"ok","num_turns":1,"total_cost_usd":0.01}
`}
	res, err := NewClaude(fr).Run(context.Background(), req(t, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Limit{{Window: "5h", UsedPercent: 90}, {Window: "weekly", UsedPercent: 100, ResetsAt: time.Unix(1761000000, 0).UTC()}}
	if len(res.Limits) != 2 || res.Limits[0].Window != want[0].Window || int(res.Limits[0].UsedPercent) != 90 || res.Limits[1] != want[1] {
		t.Fatalf("limits = %+v", res.Limits)
	}
}

// Newer CLIs send every window's reading on each event, well under the
// limits.
func TestClaudeReadsUnifiedWindows(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792516200,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.02,"resetsAt":1792516200},"seven_day":{"utilization":0.74,"resetsAt":1792659600}}}}
{"type":"result","subtype":"success","result":"ok","num_turns":1,"total_cost_usd":0.01}
`}
	res, err := NewClaude(fr).Run(context.Background(), req(t, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Limit{}
	for _, l := range res.Limits {
		got[l.Window] = l
	}
	if len(got) != 2 || int(got["5h"].UsedPercent) != 2 || int(got["weekly"].UsedPercent) != 74 || got["weekly"].ResetsAt != time.Unix(1792659600, 0).UTC() {
		t.Fatalf("limits = %+v", res.Limits)
	}
}

func TestCodexCapturesLimitsFromItsSessionFile(t *testing.T) {
	r := req(t, true)
	cmd, err := codexAdapter{}.command(RunRequest{Model: "m", CaptureLimits: true}, runner.Paths{Work: "/w", Out: "/out", Home: "/home/agent"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd[0] != "sh" || !strings.Contains(cmd[2], "/out/codex-limits.json") || cmd[4] != "exec" || cmd[len(cmd)-1] != "-" {
		t.Fatalf("cmd = %q", cmd)
	}
	plain, _ := codexAdapter{}.command(RunRequest{Model: "m"}, runner.Paths{Work: "/w", Out: "/out"})
	if plain[0] != "codex" {
		t.Fatalf("without CaptureLimits: %q", plain)
	}
	rec := `{"timestamp":"2026-10-18T10:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":42.5,"window_minutes":299,"resets_in_seconds":3600},"secondary":{"used_percent":12,"window_minutes":10079,"resets_at":1761000000}}}}`
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.OutDir, codexLimitsFile), []byte(rec+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	codexAdapter{}.limitsAfter(r.OutDir, res)
	want := []Limit{
		{Window: "5h", UsedPercent: 42.5, ResetsAt: time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)},
		{Window: "weekly", UsedPercent: 12, ResetsAt: time.Unix(1761000000, 0).UTC()},
	}
	if len(res.Limits) != 2 || res.Limits[0] != want[0] || res.Limits[1] != want[1] {
		t.Fatalf("limits = %+v", res.Limits)
	}
}
