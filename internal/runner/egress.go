package runner

import (
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
	Image   string   // an image to run the proxy in; this binary is mounted into it
	Binary  string   // this binary, on the host
}

const (
	// EgressNet is the jobs' network: internal, its gateway off the host.
	EgressNet = "factory-egress"
	// EgressProxy is the proxy container, by that name on EgressNet.
	EgressProxy = "factory-egress-proxy"
	egressPort  = "3128"
)

// EgressWait bounds the wait for a fresh proxy to listen.
var EgressWait = 20 * time.Second

func (e *Egress) proxyURL() string { return "http://" + EgressProxy + ":" + egressPort }

// fingerprint changes whenever the proxy should be recreated: other
// rules, image or binary.
func (e *Egress) fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%q %q %q %q", e.Allow, e.Private, e.Image, e.Binary)
	if fi, err := os.Stat(e.Binary); err == nil {
		fmt.Fprintf(h, " %d %d", fi.Size(), fi.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// proxyEnv is what points a job's tools at the proxy. Most read the lower
// or upper case names; Node's own fetch needs NODE_USE_ENV_PROXY.
func (e *Egress) proxyEnv(env map[string]string, local []string) map[string]string {
	out := make(map[string]string, len(env)+7)
	for k, v := range env {
		out[k] = v
	}
	u := e.proxyURL()
	noProxy := strings.Join(append([]string{"localhost", "127.0.0.1", "::1"}, local...), ",")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		out[k] = u
	}
	out["NO_PROXY"], out["no_proxy"] = noProxy, noProxy
	out["NODE_USE_ENV_PROXY"] = "1"
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
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", `{{.State.Running}} {{index .Config.Labels "factory.egress"}}`, EgressProxy).Output()
	if err == nil && strings.TrimSpace(string(out)) == "true "+fp {
		d.egressAt, d.egressFP = time.Now(), fp
		return nil
	}
	if err := d.isolatedGateways(ctx); err != nil {
		return fmt.Errorf("the egress allowlist: %w (or set runner.egress.mode: open)", err)
	}
	_ = exec.CommandContext(ctx, d.bin(), "rm", "-f", EgressProxy).Run()
	if exec.CommandContext(ctx, d.bin(), "network", "inspect", EgressNet).Run() != nil {
		// Internal: no route out. Isolated gateways: the host takes no
		// address on it, so jobs can't reach the host's ports either.
		if out, err := exec.CommandContext(ctx, d.bin(), "network", "create", "--internal",
			"-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "-o", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
			"--label", "factory.egress", EgressNet).CombinedOutput(); err != nil {
			return fmt.Errorf("docker network create %s: %v: %s", EgressNet, err, strings.TrimSpace(string(out)))
		}
	}
	args := []string{"create", "--name", EgressProxy, "--label", "factory.egress=" + fp, "--restart", "unless-stopped",
		"--network", EgressNet, "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--memory", "256m", "--pids-limit", "512",
		"-v", e.Binary + ":/opt/factory/factory:ro", "--entrypoint", "/opt/factory/factory",
		"-e", "FACTORY_EGRESS_ALLOW=" + strings.Join(e.Allow, ","), "-e", "FACTORY_EGRESS_PRIVATE=" + strings.Join(e.Private, ","),
		e.Image, "egress-proxy", "-listen", ":" + egressPort}
	if out, err := exec.CommandContext(ctx, d.bin(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("creating the egress proxy: %v: %s", err, strings.TrimSpace(string(out)))
	}
	outer := d.Network
	if outer == "" {
		outer = "bridge"
	}
	fail := func(what string, err error, out []byte) error {
		_ = exec.Command(d.bin(), "rm", "-f", EgressProxy).Run()
		return fmt.Errorf("%s: %v: %s", what, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, d.bin(), "network", "connect", outer, EgressProxy).CombinedOutput(); err != nil {
		return fail("connecting the egress proxy to "+outer, err, out)
	}
	if out, err := exec.CommandContext(ctx, d.bin(), "start", EgressProxy).CombinedOutput(); err != nil {
		return fail("starting the egress proxy", err, out)
	}
	deadline := time.Now().Add(EgressWait)
	for {
		logs, _ := exec.CommandContext(ctx, d.bin(), "logs", EgressProxy).CombinedOutput()
		if strings.Contains(string(logs), "egress proxy listening") {
			break
		}
		if !d.Alive(ctx, EgressProxy) || time.Now().After(deadline) || ctx.Err() != nil {
			return fail("the egress proxy didn't start", fmt.Errorf("not listening"), []byte(tail(string(logs), 1500)))
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.egressAt, d.egressFP = time.Now(), fp
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
