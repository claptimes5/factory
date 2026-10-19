package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestRunnersCopyStderrOnAFailedRun(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'error: 401 Unauthorized' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]Runner{"docker": &Docker{Bin: bin}, "local": Local{}} {
		var stderr bytes.Buffer
		s := Spec{Name: "j", Image: "img", Workdir: t.TempDir(), OutDir: t.TempDir(), Stderr: &stderr,
			Cmd: []string{"sh", "-c", "echo 'error: 401 Unauthorized' >&2; exit 1"}}
		code, err := r.Run(context.Background(), s, io.Discard)
		if err != nil || code != 1 || !strings.Contains(stderr.String(), "401 Unauthorized") {
			t.Fatalf("%s: code %d %v, stderr %q", name, code, err, stderr.String())
		}
	}
}

// A command that leaves a child holding its output still returns once
// it exits, and a killed one once it's killed.
func TestLocalDoesntWaitForLeftoverChildren(t *testing.T) {
	var out strings.Builder
	start := time.Now()
	code, err := Local{}.Run(context.Background(), Spec{Cmd: []string{"sh", "-c", "sleep 30 & echo hi"}}, &out)
	if err != nil || code != 0 || out.String() != "hi\n" || time.Since(start) > 15*time.Second {
		t.Fatalf("exit %d %v %q after %s", code, err, out.String(), time.Since(start))
	}
	start = time.Now()
	_, err = Local{}.Run(context.Background(), Spec{Cmd: []string{"sh", "-c", "sleep 30 & sleep 30"}, Timeout: time.Second}, &out)
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 15*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

// Exit code 125 is Docker failing to start the container: its own message
// is the error, with a hint when the image is missing.
func TestDockerExplainsAContainerThatDidntStart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\necho \"Unable to find image 'factory-job-x:latest' locally\" >&2\necho 'docker: Error response from daemon: pull access denied for factory-job-x' >&2\nexit 125\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Spec{Name: "j", Image: "factory-job-x:latest", Workdir: t.TempDir(), OutDir: t.TempDir(), Cmd: []string{"true"}}
	code, err := (&Docker{Bin: bin}).Run(context.Background(), s, io.Discard)
	if code != 125 || err == nil || !strings.Contains(err.Error(), "pull access denied") || !strings.Contains(err.Error(), "./factory images") {
		t.Fatalf("code %d, %v", code, err)
	}
}

// A job command that exits 125 itself is its own failure, not Docker's.
func TestDockerPassesOnACommandsOwn125(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'xargs: child exited 255' >&2\nexit 125\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Spec{Name: "j", Image: "img", Workdir: t.TempDir(), OutDir: t.TempDir(), Cmd: []string{"true"}}
	if code, err := (&Docker{Bin: bin}).Run(context.Background(), s, io.Discard); code != 125 || err != nil {
		t.Fatalf("code %d, %v", code, err)
	}
}
