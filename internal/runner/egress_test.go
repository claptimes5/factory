package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressArgsAndEnv(t *testing.T) {
	e := &Egress{Allow: []string{"api.anthropic.com"}, Image: "img", Binary: "/bin/factory"}
	d := &Docker{Network: "mynet", Egress: e}
	args := strings.Join(d.Args(Spec{Name: "factory-job-1", Image: "img", Workdir: "/w", OutDir: "/o"}), " ")
	if !strings.Contains(args, "--network "+EgressNet) || strings.Contains(args, "mynet") {
		t.Fatalf("args: %s", args)
	}
	if args := strings.Join(d.Args(Spec{Name: "x", Image: "img", NoNetwork: true}), " "); !strings.Contains(args, "--network none") {
		t.Fatalf("no network: %s", args)
	}
	env := e.proxyEnv(map[string]string{"A": "1"}, []string{"db"})
	if env["A"] != "1" || env["HTTPS_PROXY"] != "http://"+EgressProxy+":3128" || env["no_proxy"] != "localhost,127.0.0.1,::1,db" {
		t.Fatalf("env = %v", env)
	}
}

// fakeEgressDocker is a docker that has no proxy yet and logs its calls.
func fakeEgressDocker(t *testing.T) (*Docker, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$1 $2" in
  "inspect -f") exit 1 ;;
  "version --format") echo 28.1.0 ;;
  "network inspect") exit 1 ;;
  "logs factory-egress-proxy") echo 'level=INFO msg="egress proxy listening"' ;;
  "run --rm") echo hi ;;
esac
exit 0
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Docker{Bin: bin, Egress: &Egress{Allow: []string{"api.anthropic.com", "*.example.com"}, Private: []string{"git.corp.example"}, Image: "img", Binary: "/opt/factory"}}, log
}

func TestEnsureEgressStartsTheProxyOnce(t *testing.T) {
	d, log := fakeEgressDocker(t)
	ctx := context.Background()
	if _, err := d.Run(ctx, Spec{Name: "factory-job-1", Image: "img", Workdir: "/w", OutDir: "/o", Cmd: []string{"true"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(ctx, Spec{Name: "factory-job-2", Image: "img", Workdir: "/w", OutDir: "/o", Cmd: []string{"true"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	for _, want := range []string{
		"network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated",
		"create --name factory-egress-proxy",
		"--network factory-egress --user 65534:65534 --cap-drop ALL",
		"-v /opt/factory:/opt/factory/factory:ro --entrypoint /opt/factory/factory",
		"FACTORY_EGRESS_ALLOW=api.anthropic.com,*.example.com", "FACTORY_EGRESS_PRIVATE=git.corp.example",
		"img egress-proxy -listen :3128",
		"network connect bridge factory-egress-proxy", "start factory-egress-proxy",
		"run --rm -i --name factory-job-1", "-e HTTPS_PROXY",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in:\n%s", want, calls)
		}
	}
	// The second job finds it up (cached), so it's created once.
	if n := strings.Count(calls, "create --name factory-egress-proxy"); n != 1 {
		t.Errorf("proxy created %d times", n)
	}
}

func TestEnsureEgressNeedsDocker28(t *testing.T) {
	d, _ := fakeEgressDocker(t)
	script, _ := os.ReadFile(d.Bin)
	if err := os.WriteFile(d.Bin, []byte(strings.Replace(string(script), "echo 28.1.0", "echo 27.5.1", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := d.Run(context.Background(), Spec{Name: "factory-job-1", Image: "img", Cmd: []string{"true"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "runner.egress.mode: open") {
		t.Fatalf("err = %v", err)
	}
}
