// Package runner starts one fresh, isolated process per job: a Docker
// container with CPU, memory and wall-clock limits (or, for development,
// a plain host process). It knows nothing about agents.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrTimeout is returned when a job hits its wall-clock limit and is killed.
var ErrTimeout = errors.New("job timed out")

// Mount is an extra bind mount.
type Mount struct {
	Host, Container string
	ReadOnly        bool
}

// Spec describes one job process.
type Spec struct {
	Name     string // container name, e.g. factory-job-42
	Image    string
	Workdir  string // host path of the job's checkout
	OutDir   string // host path for verdict.json and other outputs
	ReadOnly bool   // mount the checkout read-only (reviewers)
	Mounts   []Mount
	Env      map[string]string
	Cmd      []string
	Stdin    io.Reader
	// Stderr, when set, gets a copy of the process's stderr.
	Stderr  io.Writer
	Timeout time.Duration
	// NoNetwork runs without any network (hidden scenarios, so code under
	// test cannot send them anywhere).
	NoNetwork bool
	// Services is the network of the job's services (from StartServices),
	// joined as well as the usual one, or alone with NoNetwork: it has no
	// way out, so the services are all it reaches.
	Services string
}

// Service is a container a job's commands need, such as a database. Its
// name is its host name on the services network.
type Service struct {
	Name  string
	Image string
	Env   map[string]string
	// Ready is a command run in the service container until it succeeds,
	// e.g. pg_isready; empty means ready once started.
	Ready string
}

// ServiceRunner starts and stops a job's services. Only Docker is one.
type ServiceRunner interface {
	// StartServices starts svcs on a new network named for job and waits
	// until each is ready, returning the network for Spec.Services.
	StartServices(ctx context.Context, job string, svcs []Service, wait time.Duration) (string, error)
	// StopServices removes the job's services and their network.
	StopServices(ctx context.Context, job string) error
}

// Paths are the locations a command should use for the checkout, the
// output directory and the agent's home directory, as seen by the process.
type Paths struct{ Work, Out, Home string }

// Runner runs a Spec to completion, streaming stdout to w.
type Runner interface {
	Paths(s Spec) Paths
	Run(ctx context.Context, s Spec, stdout io.Writer) (exitCode int, err error)
	Alive(ctx context.Context, name string) bool
	Kill(ctx context.Context, name string) error
	// KillAll removes every container the daemon's own pipeline started
	// (not those of a concurrent factory eval or compare).
	KillAll(ctx context.Context) error
}

// Docker runs each job in a fresh `docker run --rm` container.
type Docker struct {
	CPUs, Memory, Network string
	Bin                   string // docker binary, default "docker"
	UID, GID              int    // run as this user so files in the mounts stay owned by the daemon
	// Egress, when set, sends jobs' traffic through the egress proxy.
	Egress *Egress

	egressMu sync.Mutex
	egressAt time.Time // when the proxy was last seen up with egressFP
	egressFP string
	svcNames map[string][]string // services network -> its host names
}

const (
	dockerWork = "/work"
	dockerOut  = "/out"
	dockerHome = "/home/agent"
)

func (d *Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d *Docker) Paths(Spec) Paths { return Paths{dockerWork, dockerOut, dockerHome} }

// Args builds the docker run argument list. Env values are not on the
// command line: each is passed as `-e KEY` and set on the docker process.
func (d *Docker) Args(s Spec) []string {
	a := []string{"run", "--rm", "-i", "--name", s.Name,
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--pids-limit", "1024",
		"--label", "factory.job=" + s.Name,
		"--user", fmt.Sprintf("%d:%d", d.UID, d.GID),
		"--tmpfs", fmt.Sprintf("%s:rw,exec,uid=%d,gid=%d", dockerHome, d.UID, d.GID),
		"-e", "HOME=" + dockerHome,
		"-w", dockerWork,
	}
	if d.CPUs != "" {
		a = append(a, "--cpus", d.CPUs)
	}
	if d.Memory != "" {
		a = append(a, "--memory", d.Memory)
	}
	switch {
	case s.Services != "":
		// Joined first: the usual network is connected after create.
		a = append(a, "--network", s.Services)
	case s.NoNetwork:
		a = append(a, "--network", "none")
	case d.Egress != nil:
		a = append(a, "--network", EgressNet)
	case d.Network != "":
		a = append(a, "--network", d.Network)
	}
	ro := ""
	if s.ReadOnly {
		ro = ":ro"
	}
	a = append(a, "-v", s.Workdir+":"+dockerWork+ro, "-v", s.OutDir+":"+dockerOut)
	for _, m := range s.Mounts {
		v := m.Host + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		a = append(a, "-v", v)
	}
	for _, k := range sortedKeys(s.Env) {
		a = append(a, "-e", k)
	}
	a = append(a, s.Image)
	return append(a, s.Cmd...)
}

func (d *Docker) Run(ctx context.Context, s Spec, stdout io.Writer) (int, error) {
	ctx, cancel := withTimeout(ctx, s.Timeout)
	defer cancel()
	if d.Egress != nil && !s.NoNetwork {
		if err := d.ensureEgress(ctx); err != nil {
			return -1, err
		}
		s.Env = d.Egress.proxyEnv(s.Env, d.serviceNames(s.Services))
	}
	args := d.Args(s)
	// Env values reach the container from the docker command's own
	// environment (-e NAME), at create time.
	env := append(os.Environ(), envList(s.Env)...)
	// A container created but never started isn't removed by --rm.
	remove := func() { _ = d.Kill(context.Background(), s.Name) }
	if s.Services != "" {
		// docker run joins one network; the usual one (for the agent's
		// API and installs) is connected between create and start.
		args[0] = "create"
		create := exec.CommandContext(ctx, d.bin(), args...)
		create.Env = env
		if out, err := create.CombinedOutput(); err != nil {
			remove() // the daemon may have made it before the CLI was stopped
			return -1, fmt.Errorf("docker create: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if !s.NoNetwork {
			net := d.Network
			switch {
			case d.Egress != nil:
				net = EgressNet
			case net == "":
				net = "bridge"
			}
			if out, err := exec.CommandContext(ctx, d.bin(), "network", "connect", net, s.Name).CombinedOutput(); err != nil {
				remove()
				return -1, fmt.Errorf("docker network connect: %v: %s", err, strings.TrimSpace(string(out)))
			}
		}
		args = []string{"start", "-a", "-i", s.Name}
	}
	cmd := exec.Command(d.bin(), args...)
	cmd.Env = env
	cmd.Stdin = s.Stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = stderrTo(&stderr, s.Stderr)
	if err := cmd.Start(); err != nil {
		if s.Services != "" {
			remove()
		}
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code, err := exitCode(err, stderr.String())
		if err == nil && code == 125 {
			err = dockerStartError(stderr.String())
		}
		return code, err
	case <-ctx.Done():
		_ = d.Kill(context.Background(), s.Name)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return -1, ErrTimeout
		}
		return -1, ctx.Err()
	}
}

// dockerStartError explains exit code 125, which is Docker failing to
// start the container (a missing image, a bad mount) before the job ran.
func dockerStartError(stderr string) error {
	msg := tail(stderr, 1000)
	if msg == "" {
		msg = "no message from Docker"
	}
	hint := ""
	for _, s := range []string{"Unable to find image", "No such image", "pull access denied", "repository does not exist"} {
		if strings.Contains(stderr, s) {
			hint = "\nThe job image isn't built: run ./factory images (./factory doctor lists what's missing)."
			break
		}
	}
	return fmt.Errorf("docker couldn't start the job container: %s%s", msg, hint)
}

// serviceNet is the network of a job's services.
func serviceNet(job string) string { return job + "-svc" }

// ServiceArgs builds the docker run arguments for one service. Services
// keep their default capabilities (database images switch users and chown
// their data directory) but get no new privileges, the job's limits and
// only the internal services network.
func (d *Docker) ServiceArgs(job string, s Service) []string {
	// No --rm: a service that dies on start keeps its logs for the error;
	// StopServices removes it.
	a := []string{"run", "-d", "--name", job + "-svc-" + s.Name,
		"--network", serviceNet(job), "--network-alias", s.Name,
		"--security-opt", "no-new-privileges", "--cap-drop", "NET_RAW", "--pids-limit", "1024",
		"--label", "factory.job=" + job,
	}
	if d.CPUs != "" {
		a = append(a, "--cpus", d.CPUs)
	}
	if d.Memory != "" {
		a = append(a, "--memory", d.Memory)
	}
	for _, k := range sortedKeys(s.Env) {
		a = append(a, "-e", k+"="+s.Env[k])
	}
	return append(a, s.Image)
}

func (d *Docker) StartServices(ctx context.Context, job string, svcs []Service, wait time.Duration) (string, error) {
	net := serviceNet(job)
	if err := d.isolatedGateways(ctx); err != nil {
		return "", err
	}
	_ = d.StopServices(ctx, job) // left over from a crash
	// Internal: no route out. Isolated gateways: the host takes no address
	// on it either, so nothing on it reaches the host's own ports.
	if out, err := exec.CommandContext(ctx, d.bin(), "network", "create", "--internal",
		"-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "-o", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
		"--label", "factory.job="+job, net).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker network create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fail := func(err error) (string, error) {
		_ = d.StopServices(context.Background(), job)
		return "", err
	}
	for _, s := range svcs {
		if out, err := exec.CommandContext(ctx, d.bin(), d.ServiceArgs(job, s)...).CombinedOutput(); err != nil {
			return fail(fmt.Errorf("service %s: %v: %s", s.Name, err, strings.TrimSpace(string(out))))
		}
	}
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.Name)
	}
	d.setServiceNames(net, names)
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for _, s := range svcs {
		if s.Ready == "" {
			continue
		}
		name := job + "-svc-" + s.Name
		for {
			out, err := exec.CommandContext(rctx, d.bin(), "exec", name, "sh", "-c", s.Ready).CombinedOutput()
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if rctx.Err() != nil || !d.Alive(ctx, name) {
				logs, _ := exec.Command(d.bin(), "logs", "--tail", "30", name).CombinedOutput()
				return fail(fmt.Errorf("service %s wasn't ready within %s: `%s`: %s\n\nIts log:\n%s", s.Name, wait, s.Ready, tail(string(out), 500), tail(string(logs), 2000)))
			}
			time.Sleep(time.Second)
		}
	}
	return net, nil
}

// isolatedGateways checks the Docker engine can keep a network's gateway
// off the host (Docker 28 and later; older ones ignore the option).
func (d *Docker) isolatedGateways(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, d.bin(), "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return fmt.Errorf("docker version: %w", err)
	}
	v := strings.TrimSpace(string(out))
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 28 {
		return fmt.Errorf("services need Docker 28 or later, to keep their network off the host; this is %s", v)
	}
	return nil
}

func (d *Docker) StopServices(ctx context.Context, job string) error {
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-aq", "--filter", "name=^"+job+"-svc-").Output()
	if err != nil {
		return err
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if err := exec.CommandContext(ctx, d.bin(), append([]string{"rm", "-f"}, ids...)...).Run(); err != nil {
			return err
		}
	}
	// Fails when there is no network, which is fine.
	_ = exec.CommandContext(ctx, d.bin(), "network", "rm", serviceNet(job)).Run()
	d.setServiceNames(serviceNet(job), nil)
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func (d *Docker) Alive(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (d *Docker) Kill(ctx context.Context, name string) error {
	return exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run()
}

func (d *Docker) KillAll(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-aq", "--filter", "label=factory.job", "--filter", "name=^factory-job-").Output()
	if err != nil {
		return err
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if err := exec.CommandContext(ctx, d.bin(), append([]string{"rm", "-f"}, ids...)...).Run(); err != nil {
			return err
		}
	}
	// And the services networks those jobs left behind.
	out, err = exec.CommandContext(ctx, d.bin(), "network", "ls", "--filter", "label=factory.job", "--format", "{{.Name}}").Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, "factory-job-") {
			_ = exec.CommandContext(ctx, d.bin(), "network", "rm", name).Run()
		}
	}
	return nil
}

// Local runs the command directly on the host, in the job's workdir. It
// gives no isolation and is meant for development and dry runs only.
type Local struct{}

func (Local) Paths(s Spec) Paths { return Paths{s.Workdir, s.OutDir, os.Getenv("HOME")} }

func (Local) Run(ctx context.Context, s Spec, stdout io.Writer) (int, error) {
	if len(s.Cmd) == 0 {
		return -1, errors.New("empty command")
	}
	ctx, cancel := withTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Cmd[0], s.Cmd[1:]...)
	cmd.Dir = s.Workdir
	cmd.Env = append(os.Environ(), envList(s.Env)...)
	cmd.Stdin = s.Stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = stderrTo(&stderr, s.Stderr)
	// A child the command left running (an app started with &) can hold
	// its output open: past WaitDelay the pipes are closed and Run returns.
	cmd.WaitDelay = WaitDelay
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // the command itself exited 0
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return -1, ErrTimeout
	}
	return exitCode(err, stderr.String())
}

// WaitDelay is how long Local waits for a command's output to close
// once the command has exited or been killed.
const WaitDelay = 5 * time.Second

func (Local) Alive(context.Context, string) bool { return false }
func (Local) Kill(context.Context, string) error { return nil }
func (Local) KillAll(context.Context) error      { return nil }

// stderrTo sends stderr to the runner's own buffer and, when set, the
// caller's writer.
func stderrTo(buf *bytes.Buffer, w io.Writer) io.Writer {
	if w == nil {
		return buf
	}
	return io.MultiWriter(buf, w)
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func exitCode(err error, stderr string) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if stderr != "" {
		return -1, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return -1, err
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
