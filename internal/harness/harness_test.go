package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	res, err := NewCodex(fr).Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Final, `"decision":"pass"`) || res.TokensIn != 1000 || res.TokensOut != 300 || res.CostKnown {
		t.Fatalf("res = %+v", res)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	for _, want := range []string{"codex exec --json", "--sandbox read-only", "--output-schema /out/schema.json"} {
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

func TestCodexTurnFailed(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"turn.failed","error":{"message":"rate limited"}}` + "\n", code: 1}
	res, _ := NewCodex(fr).Run(context.Background(), req(t, true), nil)
	if !res.IsError || !strings.Contains(res.Error, "rate limited") {
		t.Fatalf("res = %+v", res)
	}
}
