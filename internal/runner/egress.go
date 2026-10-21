package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Egress sends every job's traffic through the egress proxy: jobs join a
// network with no way out, and the proxy, on it and on the usual one,
// forwards to allowlisted hosts only (see internal/egress).
type Egress struct {
	Allow   []string // rules, as egress.ParseRule reads them
	Private []string // rules for hosts on a private network
	Binary  string   // this binary, on the host, which the proxy runs
	// Owner labels the proxies this runner makes. StaleEgressProxies
	// removes only its own owner's, so the daemon leaves alone a proxy
	// a factory eval beside it is using or still setting up.
	Owner string
}

const (
	// EgressNet is the jobs' network: internal, its gateway off the host.
	EgressNet = "factory-egress"
	// EgressProxy starts the proxy container's name, and its host name on
	// EgressNet; the rest is the fingerprint of its rules, so a factory
	// eval or a second binary with other rules runs its own proxy beside
	// the daemon's instead of replacing it.
	EgressProxy = "factory-egress-proxy-"
	egressPort  = "3128"
	// egressImage is an empty image: the proxy is a static binary, mounted.
	egressImage = "factory-egress-base:1"
)

// EgressWait bounds the wait for a fresh proxy to listen.
var EgressWait = 20 * time.Second

func (e *Egress) name() string     { return EgressProxy + e.fingerprint() }
func (e *Egress) proxyURL() string { return "http://" + e.name() + ":" + egressPort }

// fingerprint changes whenever the proxy should be recreated: other rules
// or another binary.
func (e *Egress) fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%q %q %q", e.Allow, e.Private, e.Binary)
	if fi, err := os.Stat(e.Binary); err == nil {
		fmt.Fprintf(h, " %d %d", fi.Size(), fi.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// proxyEnv is what points a job's tools at the proxy. Most read the lower
// or upper case names; Node's own fetch needs NODE_USE_ENV_PROXY, and the
// JVM (Gradle, sbt; Maven partly) its own properties.
func (e *Egress) proxyEnv(env map[string]string, local []string) map[string]string {
	out := make(map[string]string, len(env)+8)
	for k, v := range env {
		out[k] = v
	}
	u := e.proxyURL()
	direct := append([]string{"localhost", "127.0.0.1", "::1"}, local...)
	noProxy := strings.Join(direct, ",")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		out[k] = u
	}
	out["NO_PROXY"], out["no_proxy"] = noProxy, noProxy
	out["NODE_USE_ENV_PROXY"] = "1"
	java := fmt.Sprintf("-Dhttps.proxyHost=%[1]s -Dhttps.proxyPort=%[2]s -Dhttp.proxyHost=%[1]s -Dhttp.proxyPort=%[2]s -Dhttp.nonProxyHosts=%[3]s",
		e.name(), egressPort, strings.Join(direct, "|"))
	out["JAVA_TOOL_OPTIONS"] = strings.TrimSpace(out["JAVA_TOOL_OPTIONS"] + " " + java)
	return out
}

// ensureEgress starts the proxy and its network unless they're already
// up with these rules. Checked before each job, so a proxy that died or a
// Docker restart is mended rather than failing jobs.
func (d *Docker) ensureEgress(ctx context.Context) error {
	e := d.Egress
	d.egressMu.Lock()
	defer d.egressMu.Unlock()
	fp := e.fingerprint()
	if time.Since(d.egressAt) < 30*time.Second && d.egressFP == fp {
		return nil
	}
	name := e.name() // the fingerprint is in it: same name, same rules
	status := func() string {
		out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{.State.Status}}", name).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	ok := func() error { d.egressAt, d.egressFP = time.Now(), fp; return nil }
	switch status() {
	case "running":
		return ok()
	case "created":
		// Another process with the same rules is setting it up.
		if d.waitEgress(ctx, name) == nil {
			return ok()
		}
	case "exited", "dead":
		if exec.CommandContext(ctx, d.bin(), "start", name).Run() == nil && d.waitEgress(ctx, name) == nil {
			return ok()
		}
	}
	if err := d.isolatedGateways(ctx); err != nil {
		return fmt.Errorf("the egress allowlist: %w (or set runner.egress.mode: open)", err)
	}
	if err := d.egressNetwork(ctx); err != nil {
		return err
	}
	if exec.CommandContext(ctx, d.bin(), "image", "inspect", egressImage).Run() != nil {
		if err := d.importEmptyImage(ctx); err != nil {
			return err
		}
	}
	if st := status(); st != "" && st != "running" {
		_ = exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run() // broken: start afresh
	}
	args := []string{"create", "--name", name, "--label", "factory.egress=" + fp, "--label", "factory.egress.owner=" + e.Owner,
		"--network", EgressNet, "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--memory", "256m", "--pids-limit", "512",
		"-v", e.Binary + ":/opt/factory/factory:ro", "--entrypoint", "/opt/factory/factory",
		"-e", "FACTORY_EGRESS_ALLOW=" + strings.Join(e.Allow, ","), "-e", "FACTORY_EGRESS_PRIVATE=" + strings.Join(e.Private, ","),
		egressImage, "egress-proxy", "-listen", ":" + egressPort}
	if out, err := exec.CommandContext(ctx, d.bin(), args...).CombinedOutput(); err != nil {
		// Another process with the same rules made it first: use theirs.
		if strings.Contains(string(out), "already in use") && d.waitEgress(ctx, name) == nil {
			return ok()
		}
		return fmt.Errorf("creating the egress proxy: %v: %s", err, strings.TrimSpace(string(out)))
	}
	outer := d.Network
	if outer == "" {
		outer = "bridge"
	}
	// On failure the container is removed, so the next try starts afresh.
	fail := func(what string, err error, out []byte) error {
		_ = exec.Command(d.bin(), "rm", "-f", name).Run()
		return fmt.Errorf("%s: %v: %s", what, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, d.bin(), "network", "connect", outer, name).CombinedOutput(); err != nil {
		return fail("connecting the egress proxy to "+outer, err, out)
	}
	if out, err := exec.CommandContext(ctx, d.bin(), "start", name).CombinedOutput(); err != nil {
		return fail("starting the egress proxy", err, out)
	}
	if err := d.waitEgress(ctx, name); err != nil {
		return fail("the egress proxy didn't start", err, nil)
	}
	d.egressAt, d.egressFP = time.Now(), fp
	return nil
}

// waitEgress waits for the proxy to listen. Only log lines since its
// latest start count: a restarted container still has the old run's.
func (d *Docker) waitEgress(ctx context.Context, name string) error {
	deadline := time.Now().Add(EgressWait)
	for {
		st, _ := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{.State.Status}} {{.State.StartedAt}}", name).Output()
		status, started, _ := strings.Cut(strings.TrimSpace(string(st)), " ")
		args := []string{"logs", name}
		if started != "" && !strings.HasPrefix(started, "0001-") {
			args = []string{"logs", "--since", started, name}
		}
		logs, _ := exec.CommandContext(ctx, d.bin(), args...).CombinedOutput()
		if status == "running" && strings.Contains(string(logs), "egress proxy listening") {
			return nil
		}
		if status == "exited" || status == "dead" {
			return fmt.Errorf("it exited: %s", tail(string(logs), 1500))
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("not listening after %s: %s", EgressWait, tail(string(logs), 1500))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// egressNet makes the jobs' network, or checks the one there is: a
// network of that name that isn't internal with its gateway off the host
// would give jobs the internet, so it's refused rather than used.
func (d *Docker) egressNetwork(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, d.bin(), "network", "inspect", "-f",
		`{{.Internal}} {{index .Options "com.docker.network.bridge.gateway_mode_ipv4"}} {{index .Options "com.docker.network.bridge.gateway_mode_ipv6"}}`, EgressNet).Output()
	if err == nil {
		if got := strings.TrimSpace(string(out)); got != "true isolated isolated" {
			return fmt.Errorf("the Docker network %s isn't the factory's (internal, gateway isolated: got %q); remove it with `docker network rm %s`", EgressNet, got, EgressNet)
		}
		return nil
	}
	// Internal: no route out. Isolated gateways: the host takes no
	// address on it, so jobs can't reach the host's ports either.
	if out, err := exec.CommandContext(ctx, d.bin(), "network", "create", "--internal",
		"-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "-o", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
		"--label", "factory.egress", EgressNet).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already exists") {
			return d.egressNetwork(ctx) // made meanwhile: check it
		}
		return fmt.Errorf("docker network create %s: %v: %s", EgressNet, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// importEmptyImage makes egressImage from an empty filesystem, so the
// proxy needs no image pulled or built.
func (d *Docker) importEmptyImage(ctx context.Context) error {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, dir := range []string{"opt/", "opt/factory/", "tmp/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, d.bin(), "import", "-", egressImage)
	cmd.Stdin = &b
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("making the egress proxy's image: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StopEgress removes this runner's proxy once no job is left on its
// network, so it doesn't outlive the factory: it isn't set to restart, and
// ensureEgress makes it again before the next job.
// ponytail: another process sharing these rules, idle right now, may
// start a job within ensureEgress's 30s cache and find no proxy; refcount
// the users if that bites.
func (d *Docker) StopEgress(ctx context.Context) error {
	if d.Egress == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "--filter", "network="+EgressNet, "--format", "{{.Names}}").Output()
	if err != nil {
		return err
	}
	for _, n := range strings.Fields(string(out)) {
		if !strings.HasPrefix(n, EgressProxy) {
			return nil // a job still uses it
		}
	}
	d.egressMu.Lock()
	d.egressAt = time.Time{}
	d.egressMu.Unlock()
	return exec.CommandContext(ctx, d.bin(), "rm", "-f", d.Egress.name()).Run()
}

// StaleEgressProxies removes the proxies this runner's owner made for
// other rules or binaries, and the one name earlier versions used,
// keeping this runner's. The daemon calls it at start.
func (d *Docker) StaleEgressProxies(ctx context.Context) error {
	if d.Egress == nil || d.Egress.Owner == "" {
		return nil
	}
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-a", "--filter", "label=factory.egress.owner="+d.Egress.Owner, "--format", "{{.Names}}").Output()
	if err != nil {
		return err
	}
	stale := append(strings.Fields(string(out)), "factory-egress-proxy")
	for _, n := range stale {
		if n != d.Egress.name() {
			_ = exec.CommandContext(ctx, d.bin(), "rm", "-f", n).Run()
		}
	}
	return nil
}

// serviceNames are the host names of a services network's containers,
// which jobs reach directly rather than through the proxy.
func (d *Docker) serviceNames(net string) []string {
	d.egressMu.Lock()
	defer d.egressMu.Unlock()
	names := append([]string(nil), d.svcNames[net]...)
	sort.Strings(names)
	return names
}

func (d *Docker) setServiceNames(net string, names []string) {
	d.egressMu.Lock()
	defer d.egressMu.Unlock()
	if d.svcNames == nil {
		d.svcNames = map[string][]string{}
	}
	if names == nil {
		delete(d.svcNames, net)
		return
	}
	d.svcNames[net] = names
}
