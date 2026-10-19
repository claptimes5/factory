package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// fakeClaude puts a stand-in claude on PATH that speaks stream-json: it
// reads the prompt, says it started, then echoes each further message
// back and ends the run on the first one, and exits only at the end of
// its input, as the real CLI does.
func fakeClaude(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
read -r prompt
echo '{"type":"system","subtype":"init","model":"m","tools":["Bash"],"session_id":"s1"}'
while read -r msg; do
  echo "$msg" | sed 's/^{"type":"user"/{"type":"user","isReplay":true/'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":2,"total_cost_usd":0.1}'
done
echo '{"type":"system","subtype":"bye"}'
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClaudeSteeringKeepsInputOpenUntilAnswered(t *testing.T) {
	fakeClaude(t)
	r := req(t, false)
	r.Workdir = t.TempDir()
	steer := make(chan SteerMsg)
	r.Steer = steer
	ev := make(chan Event, 100)
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := NewClaude(runner.Local{}).Run(context.Background(), r, ev)
		done <- out{res, err}
	}()
	var lines []string
	deadline := time.After(10 * time.Second)
	// Wait for the CLI to start, then steer it.
	for started := false; !started; {
		select {
		case e := <-ev:
			lines = append(lines, string(e.Payload))
			started = e.Kind == "raw" && strings.Contains(string(e.Payload), `"init"`)
		case <-deadline:
			t.Fatal("the fake CLI never started")
		}
	}
	errc := make(chan error, 1)
	steer <- SteerMsg{Text: "also fix the README", Err: errc}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	var o out
	for got := false; !got; {
		select {
		case e := <-ev:
			lines = append(lines, string(e.Payload))
		case o = <-done:
			got = true
		case <-deadline:
			t.Fatal("the run never ended: its input stayed open")
		}
	}
	for len(ev) > 0 {
		lines = append(lines, string((<-ev).Payload))
	}
	if o.err != nil || o.res.IsError || o.res.Final != "done" {
		t.Fatalf("res = %+v, err = %v", o.res, o.err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, `"type":"factory_message"`) || !strings.Contains(all, "also fix the README") || !strings.Contains(all, `"bye"`) {
		t.Fatalf("log:\n%s", all)
	}
	// The prompt went in as a stream-json message.
	var m map[string]any
	if err := json.Unmarshal(claudeAdapter{}.userLine("hi"), &m); err != nil || m["type"] != "user" {
		t.Fatalf("userLine = %v, %v", m, err)
	}
}

func TestSteeringANonStreamingCLIIsRefused(t *testing.T) {
	fr := &fakeRunner{out: codexStream}
	r := req(t, false)
	steer := make(chan SteerMsg, 1)
	r.Steer = steer
	errc := make(chan error, 1)
	steer <- SteerMsg{Text: "hi", Err: errc}
	if _, err := NewCodex(fr).Run(context.Background(), r, make(chan Event, 100)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != ErrNotListening {
			t.Fatalf("err = %v", err)
		}
	default:
		// Run may end before the message is read: that's fine too.
	}
}

func TestClaudeStopsAndWrapUpHook(t *testing.T) {
	fr := &fakeRunner{out: `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":80}` + "\n", code: 1}
	r := req(t, false)
	r.MaxTurns = 80
	r.Fast = true
	res, _ := NewClaude(fr).Run(context.Background(), r, nil)
	if res.Stopped != "turns" {
		t.Fatalf("stopped = %q", res.Stopped)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	if !strings.Contains(cmd, `"fastMode":true`) || !strings.Contains(cmd, `"PostToolUse"`) || !strings.Contains(cmd, "/out/.factory-wrapup.sh") {
		t.Fatalf("cmd = %s", cmd)
	}
	b, err := os.ReadFile(filepath.Join(r.OutDir, wrapUpScript))
	if err != nil || !strings.Contains(string(b), `-eq 70 ]`) || !strings.Contains(string(b), "/out/progress.md") {
		t.Fatalf("script = %s, %v", b, err)
	}
	// Read-only runs get no hook.
	fr2 := &fakeRunner{out: `{"type":"result","subtype":"error_max_budget_usd","is_error":true}` + "\n"}
	r2 := req(t, true)
	r2.MaxTurns = 80
	res2, _ := NewClaude(fr2).Run(context.Background(), r2, nil)
	if res2.Stopped != "budget" || strings.Contains(strings.Join(fr2.spec.Cmd, " "), "PostToolUse") {
		t.Fatalf("read-only: %+v %v", res2, fr2.spec.Cmd)
	}
}

func TestWrapUpHookScriptCounts(t *testing.T) {
	r := req(t, false)
	r.MaxTurns = 12
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := runner.Paths{Work: r.Workdir, Out: r.OutDir, Home: t.TempDir()}
	if _, _, err := (claudeAdapter{}).prepare(context.Background(), r, p, nil); err != nil {
		t.Fatal(err)
	}
	var said []int
	for i := 1; i <= 12; i++ {
		out := runSh(t, filepath.Join(r.OutDir, wrapUpScript))
		if out != "" {
			var h struct {
				Out struct {
					Event string `json:"hookEventName"`
					Ctx   string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &h); err != nil || h.Out.Event != "PostToolUse" || !strings.Contains(h.Out.Ctx, "progress.md") {
				t.Fatalf("call %d: %q (%v)", i, out, err)
			}
			said = append(said, i)
		}
	}
	if len(said) != 2 || said[0] != 8 || said[1] != 9 {
		t.Fatalf("warned at %v", said)
	}
}

func runSh(t *testing.T, script string) string {
	t.Helper()
	cmd := execCommand("sh", script)
	cmd.Stdin = strings.NewReader(`{"tool_name":"Bash"}`)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

var execCommand = exec.Command
