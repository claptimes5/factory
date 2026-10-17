package runner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDockerArgsIsolateAndHideSecrets(t *testing.T) {
	d := &Docker{CPUs: "2", Memory: "4g", Network: "factory-egress", UID: 1000, GID: 1000}
	args := strings.Join(d.Args(Spec{
		Name: "factory-job-1", Image: "img", Workdir: "/w", OutDir: "/o", ReadOnly: true,
		Mounts: []Mount{{Host: "/skills/a", Container: "/home/agent/.claude/skills", ReadOnly: true}},
		Env:    map[string]string{"ANTHROPIC_API_KEY": "sk-secret"},
		Cmd:    []string{"claude", "-p"},
	}), " ")
	for _, want := range []string{"--rm", "--cpus 2", "--memory 4g", "--network factory-egress", "--cap-drop ALL",
		"-v /w:/work:ro", "-v /o:/out", "/skills/a:/home/agent/.claude/skills:ro", "-e ANTHROPIC_API_KEY", "img claude -p"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "sk-secret") {
		t.Fatal("secret value leaked into docker args")
	}
}

func TestLocalRunStreamsAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	code, err := Local{}.Run(context.Background(), Spec{Workdir: dir, Cmd: []string{"sh", "-c", "echo $FOO; exit 3"}, Env: map[string]string{"FOO": "bar"}}, &out)
	if err != nil || code != 3 || strings.TrimSpace(out.String()) != "bar" {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
	_, err = Local{}.Run(context.Background(), Spec{Workdir: dir, Cmd: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond}, &out)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
}
