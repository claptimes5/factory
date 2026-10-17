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
	"strings"
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
	Timeout  time.Duration
	// NoNetwork runs without any network (hidden scenarios, so code under
	// test cannot send them anywhere).
	NoNetwork bool
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
	if s.NoNetwork {
		a = append(a, "--network", "none")
	} else if d.Network != "" {
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
	cmd := exec.Command(d.bin(), d.Args(s)...)
	cmd.Env = append(os.Environ(), envList(s.Env)...)
	cmd.Stdin = s.Stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(err, stderr.String())
	case <-ctx.Done():
		_ = d.Kill(context.Background(), s.Name)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return -1, ErrTimeout
		}
		return -1, ctx.Err()
	}
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
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	return exec.CommandContext(ctx, d.bin(), append([]string{"rm", "-f"}, ids...)...).Run()
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
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return -1, ErrTimeout
	}
	return exitCode(err, stderr.String())
}

func (Local) Alive(context.Context, string) bool { return false }
func (Local) Kill(context.Context, string) error { return nil }
func (Local) KillAll(context.Context) error      { return nil }

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
