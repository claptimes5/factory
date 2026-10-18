package runner

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestServiceArgs(t *testing.T) {
	d := &Docker{CPUs: "2", Memory: "4g", Network: "egress"}
	args := strings.Join(d.ServiceArgs("factory-job-7", Service{Name: "postgres", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "x"}}), " ")
	for _, want := range []string{"run -d --name factory-job-7-svc-postgres", "--cap-drop NET_RAW", "--network factory-job-7-svc", "--network-alias postgres",
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
	netName, err := d.StartServices(ctx, job, []Service{{Name: "cache", Image: "redis:7-alpine", Ready: "redis-cli ping"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string, noNet bool, cmd string) (int, string) {
		var out bytes.Buffer
		code, err := d.Run(ctx, Spec{Name: job + "-" + name, Image: "alpine:3.20", Workdir: t.TempDir(), OutDir: t.TempDir(), Services: netName, NoNetwork: noNet,
			Env: map[string]string{"FACTORY_TEST_ENV": "from-spec"}, Cmd: []string{"sh", "-c", cmd}, Timeout: time.Minute}, &out)
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
	// The spec's env is set: it's fixed at create, not at start.
	if code, out := run("env", true, "echo \"[$FACTORY_TEST_ENV]\""); code != 0 || !strings.Contains(out, "[from-spec]") {
		t.Fatalf("env: exit %d: %s", code, out)
	}
	// Nothing on the services network reaches the host's own ports.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("HOST\n"))
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	gw, _ := exec.Command("docker", "network", "inspect", "-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", netName).Output()
	targets := strings.Fields(string(gw) + " 172.17.0.1 host.docker.internal")
	for _, h := range targets {
		if code, out := run("host", true, fmt.Sprintf("nc -w 2 %s %d", h, port)); code == 0 || strings.Contains(out, "HOST") {
			t.Fatalf("a scenario reached the host at %s: exit %d: %s", h, code, out)
		}
	}
	if err := d.StopServices(ctx, job); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+job).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("containers left: %s", out)
	}
	if exec.Command("docker", "network", "inspect", netName).Run() == nil {
		t.Fatal("network left behind")
	}
}
