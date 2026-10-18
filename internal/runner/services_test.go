package runner

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestServiceArgs(t *testing.T) {
	d := &Docker{CPUs: "2", Memory: "4g", Network: "egress"}
	args := strings.Join(d.ServiceArgs("factory-job-7", Service{Name: "postgres", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "x"}}), " ")
	for _, want := range []string{"run -d --rm --name factory-job-7-svc-postgres", "--network factory-job-7-svc", "--network-alias postgres",
		"no-new-privileges", "--label factory.job=factory-job-7", "--memory 4g", "-e POSTGRES_PASSWORD=x", "postgres:16"} {
		if !strings.Contains(args, want) {
			t.Errorf("service args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "egress") {
		t.Errorf("a service joined the egress network:\n%s", args)
	}
	job := strings.Join(d.Args(Spec{Name: "factory-job-7-gate-1", Image: "img", Workdir: "/w", OutDir: "/o", Services: "factory-job-7-svc"}), " ")
	if !strings.Contains(job, "--network factory-job-7-svc") || strings.Contains(job, "--network egress") {
		t.Errorf("job args:\n%s", job)
	}
}

// With Docker, a job's commands reach its services by name, a check run
// without network still reaches them (and nothing else), and stopping
// removes the services and their network.
func TestServicesWithDocker(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("no docker")
	}
	if exec.Command("docker", "image", "inspect", "redis:7-alpine").Run() != nil || exec.Command("docker", "image", "inspect", "alpine:3.20").Run() != nil {
		t.Skip("needs the redis:7-alpine and alpine:3.20 images")
	}
	ctx := context.Background()
	d := &Docker{}
	job := "factory-test-svc-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	t.Cleanup(func() { _ = d.StopServices(context.Background(), job) })
	net, err := d.StartServices(ctx, job, []Service{{Name: "cache", Image: "redis:7-alpine", Ready: "redis-cli ping"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string, noNet bool, cmd string) (int, string) {
		var out bytes.Buffer
		code, err := d.Run(ctx, Spec{Name: job + "-" + name, Image: "alpine:3.20", Workdir: t.TempDir(), OutDir: t.TempDir(), Services: net, NoNetwork: noNet,
			Cmd: []string{"sh", "-c", cmd}, Timeout: time.Minute}, &out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return code, out.String()
	}
	if code, out := run("gate", false, "printf 'PING\\r\\n' | nc -w 3 cache 6379"); code != 0 || !strings.Contains(out, "PONG") {
		t.Fatalf("gate: exit %d: %s", code, out)
	}
	if code, out := run("scenario", true, "printf 'PING\\r\\n' | nc -w 3 cache 6379 && ! wget -q -T 3 -O /dev/null http://1.1.1.1"); code != 0 || !strings.Contains(out, "PONG") {
		t.Fatalf("scenario: exit %d: %s", code, out)
	}
	if err := d.StopServices(ctx, job); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+job).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("containers left: %s", out)
	}
	if exec.Command("docker", "network", "inspect", net).Run() == nil {
		t.Fatal("network left behind")
	}
}
