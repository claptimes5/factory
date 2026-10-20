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
	e := &Egress{Allow: []string{"api.anthropic.com"}, Binary: "/bin/factory"}
	d := &Docker{Network: "mynet", Egress: e}
	args := strings.Join(d.Args(Spec{Name: "factory-job-1", Image: "img", Workdir: "/w", OutDir: "/o"}), " ")
	if !strings.Contains(args, "--network "+EgressNet) || strings.Contains(args, "mynet") {
		t.Fatalf("args: %s", args)
	}
	if args := strings.Join(d.Args(Spec{Name: "x", Image: "img", NoNetwork: true}), " "); !strings.Contains(args, "--network none") {
		t.Fatalf("no network: %s", args)
	}
	env := e.proxyEnv(map[string]string{"A": "1"}, []string{"db"})
	if env["A"] != "1" || env["HTTPS_PROXY"] != "http://"+e.name()+":3128" || !strings.Contains(env["JAVA_TOOL_OPTIONS"], "-Dhttps.proxyHost="+e.name()) || env["no_proxy"] != "localhost,127.0.0.1,::1,db" {
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
  "inspect -f")
    case "$3" in
      *Status*) test -f ` + dir + `/up && echo running || exit 1 ;;
      *Running*) test -f ` + dir + `/up && echo true || exit 1 ;;
      *) exit 1 ;;
    esac ;;
  "ps -a") ;;
  "start "*) touch ` + dir + `/up ;;
  "version --format") echo 28.1.0 ;;
  "network inspect") test -f ` + dir + `/net && echo 'true isolated isolated' || exit 1 ;;
  "network create") touch ` + dir + `/net ;;
  "image inspect") exit 1 ;;
  "import -") cat > /dev/null ;;
  logs*) echo 'level=INFO msg="egress proxy listening"' ;;
  "run --rm") echo hi ;;
esac
exit 0
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Docker{Bin: bin, Egress: &Egress{Allow: []string{"api.anthropic.com", "*.example.com"}, Private: []string{"git.corp.example"}, Binary: "/opt/factory"}}, log
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
		"import - factory-egress-base:1",
		"create --name " + d.Egress.name(),
		"--network factory-egress --user 65534:65534 --cap-drop ALL",
		"-v /opt/factory:/opt/factory/factory:ro --entrypoint /opt/factory/factory",
		"FACTORY_EGRESS_ALLOW=api.anthropic.com,*.example.com", "FACTORY_EGRESS_PRIVATE=git.corp.example",
		"factory-egress-base:1 egress-proxy -listen :3128",
		"network connect bridge " + d.Egress.name(), "start " + d.Egress.name(),
		"run --rm -i --name factory-job-1", "-e HTTPS_PROXY",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in:\n%s", want, calls)
		}
	}
	// The second job finds it up (cached), so it's created once.
	if n := strings.Count(calls, "create --name "+d.Egress.name()); n != 1 {
		t.Errorf("proxy created %d times", n)
	}
}

// A network of the jobs' name that isn't internal is refused, not used.
func TestEgressRefusesAnOpenNetwork(t *testing.T) {
	d, _ := fakeEgressDocker(t)
	script, _ := os.ReadFile(d.Bin)
	if err := os.WriteFile(d.Bin, []byte(strings.Replace(string(script), "test -f "+filepath.Dir(d.Bin)+"/net", "echo 'false  '; exit 0; test -f", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := d.Run(context.Background(), Spec{Name: "factory-job-1", Image: "img", Cmd: []string{"true"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "isn't the factory's") {
		t.Fatalf("err = %v", err)
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

// The daemon removes only its own owner's stale proxies (and the old
// name), never one an eval beside it made.
func TestStaleEgressProxiesKeepsOthers(t *testing.T) {
	d, log := fakeEgressDocker(t)
	d.Egress.Owner = "daemon-1"
	script, _ := os.ReadFile(d.Bin)
	script = []byte(strings.Replace(string(script), `"ps -a") ;;`, `"ps -a") echo factory-egress-proxy-old; echo `+d.Egress.name()+` ;;`, 1))
	if err := os.WriteFile(d.Bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.StaleEgressProxies(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	for _, want := range []string{"--filter label=factory.egress.owner=daemon-1", "rm -f factory-egress-proxy-old", "rm -f factory-egress-proxy\n"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "rm -f "+d.Egress.name()) {
		t.Errorf("removed its own proxy:\n%s", calls)
	}
}

// The proxy isn't set to restart, and StopEgress removes it only once no
// job is left on its network.
func TestStopEgressWaitsForJobs(t *testing.T) {
	d, log := fakeEgressDocker(t)
	ctx := context.Background()
	if _, err := d.Run(ctx, Spec{Name: "factory-job-1", Image: "img", Cmd: []string{"true"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	script, _ := os.ReadFile(d.Bin)
	busy := strings.Replace(string(script), `"ps -a") ;;`, `"ps -a") ;;
  "ps --filter") echo `+d.Egress.name()+`; echo factory-job-2 ;;`, 1)
	if err := os.WriteFile(d.Bin, []byte(busy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.StopEgress(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if calls := string(b); strings.Contains(calls, "--restart") || strings.Contains(calls, "rm -f "+d.Egress.name()) {
		t.Fatalf("restarts, or removed in use:\n%s", calls)
	}
	if err := d.StopEgress(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	if calls := string(b); !strings.Contains(calls, "ps --filter network=factory-egress") || !strings.Contains(calls, "rm -f "+d.Egress.name()) {
		t.Fatalf("not removed:\n%s", calls)
	}
}
