package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
)

func TestVersion(t *testing.T) {
	for in, want := range map[string]string{
		"28.0.0-rc.1":                          "28.0.0",
		"Docker version 27.5.1, build 9f9e405": "27.5.1",
		"go version go1.26.3 linux/amd64":      "1.26.3",
		"go version go1.26rc1 linux/amd64":     "1.26",
		"v22.12.0\n":                           "22.12.0",
		"git version 2.43.0":                   "2.43.0",
		"Docker version 27.3.1, build 2.fc41":  "27.3.1",
		"Cannot connect to the Docker daemon":  "",
		"":                                     "",
	} {
		if got := version(in); got != want {
			t.Errorf("version(%q) = %q, want %q", in, got, want)
		}
	}
	if got := versionParts("28.0.0-rc.1"); !slices.Equal(got, []int{28, 0, 0}) {
		t.Errorf("versionParts = %v", got)
	}
	if versionParts("nope") != nil {
		t.Error("no version should be nil")
	}
}

func TestOSFamily(t *testing.T) {
	for in, want := range map[string]string{
		"ID=ubuntu\nID_LIKE=debian\n":                        "debian",
		`ID="rocky"` + "\n" + `ID_LIKE="rhel centos fedora"`: "fedora",
		"ID=fedora\n":                    "fedora",
		"ID=arch\n":                      "arch",
		"ID=endeavouros\nID_LIKE=arch\n": "arch",
		"ID=alpine\n":                    "",
	} {
		if got := osFamily(in); got != want {
			t.Errorf("osFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeMachine answers the doctor's commands from a table, keyed by the
// command line; a missing entry fails like a missing command.
type fakeMachine struct {
	out     map[string]string
	fail    map[string]bool
	missing map[string]bool // not on PATH
	disks   []runner.Disk
	busy    bool
}

func (m *fakeMachine) doctor(cfg *config.Config) *doctor {
	return &doctor{cfg: cfg, plat: platform{family: "debian"},
		run: func(_ context.Context, name string, args ...string) (string, error) {
			k := strings.Join(append([]string{name}, args...), " ")
			if m.fail[k] {
				return m.out[k], errors.New("exit status 1")
			}
			return m.out[k], nil
		},
		lookPath: func(name string) (string, error) {
			if m.missing[name] {
				return "", exec.ErrNotFound
			}
			return "/usr/bin/" + name, nil
		},
		disks: func(context.Context) ([]runner.Disk, error) { return m.disks, nil },
		listen: func(string) error {
			if m.busy {
				return errors.New("address already in use")
			}
			return nil
		},
	}
}

const dockerVersion = "docker version --format {{.Server.Version}}"

func healthy() *fakeMachine {
	return &fakeMachine{
		out:  map[string]string{"git --version": "git version 2.43.0\n", dockerVersion: "29.1.3\n"},
		fail: map[string]bool{}, missing: map[string]bool{},
		disks: []runner.Disk{{Where: "the data directory (/d)", Free: 200e9}},
	}
}

func clearCreds(t *testing.T) {
	for _, k := range append(envKeys, "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX") {
		t.Setenv(k, "")
	}
}

func TestDoctorChecks(t *testing.T) {
	setCreds := func(t *testing.T) {
		clearCreds(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-x")
		t.Setenv("OPENAI_API_KEY", "sk-x")
		t.Setenv("GITHUB_TOKEN", "ghp_x")
	}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, m *fakeMachine, cfg *config.Config)
		status int
		what   string // in the line that has status
		fix    string // in its fix
	}{
		{"git missing", func(_ *testing.T, m *fakeMachine, _ *config.Config) { m.missing["git"] = true },
			fail, "git isn't installed", "sudo apt install git"},
		{"docker missing", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.missing["docker"], m.fail[dockerVersion] = true, true
		}, fail, "Docker isn't installed", "get.docker.com"},
		{"docker down", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.fail[dockerVersion] = true
			m.out[dockerVersion] = "\nCannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"
		}, fail, "Docker isn't running: Cannot connect", "sudo systemctl start docker"},
		{"docker permission", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.fail[dockerVersion] = true
			m.out[dockerVersion] = "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"
		}, fail, "permission denied", "sudo usermod -aG docker $USER && newgrp docker"},
		{"docker 27 with the allowlist", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.out[dockerVersion] = "27.5.1\n"
		}, fail, "Docker 27.5.1 is too old for the egress allowlist", "runner.egress.mode: open"},
		{"docker 27 with services", func(_ *testing.T, m *fakeMachine, cfg *config.Config) {
			m.out[dockerVersion] = "27.5.1\n"
			cfg.Runner.Egress.Mode = "open"
			cfg.Services = map[string]config.Service{"db": {Image: "postgres"}}
		}, fail, "too old for services", "upgrade Docker"},
		{"image missing", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.fail["docker image inspect factory-job:latest"] = true
		}, fail, "Job image factory-job:latest (for o/r) isn't built", "./factory images"},
		{"orchestrator image missing", func(_ *testing.T, m *fakeMachine, cfg *config.Config) {
			cfg.Repos[0].ImageFrom = "ruby:3.4-bookworm"
			cfg.Repos[0].Image = "factory-job-o__r:latest"
			m.fail["docker image inspect factory-job:latest"] = true
		}, fail, "Job image factory-job:latest (for the orchestrator) isn't built", "./factory images"},
		{"low disk", func(_ *testing.T, m *fakeMachine, _ *config.Config) {
			m.disks = []runner.Disk{{Where: "Docker's storage (/var/lib/docker)", Free: 3e9}}
		}, fail, "Low on disk: Docker's storage (/var/lib/docker) has 3 GB free", "budgets.min_free_disk_gb"},
		{"no claude", func(t *testing.T, _ *fakeMachine, _ *config.Config) { t.Setenv("ANTHROPIC_API_KEY", "") },
			fail, "No Claude credential", "./factory setup"},
		{"no openai", func(t *testing.T, _ *fakeMachine, _ *config.Config) { t.Setenv("OPENAI_API_KEY", "") },
			fail, "routes review to Codex", "docs/configuration.md#claude-only-review"},
		{"auth token with docker", func(t *testing.T, _ *fakeMachine, _ *config.Config) {
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "x")
		}, fail, "don't reach docker jobs", "CLAUDE_CODE_OAUTH_TOKEN"},
		{"docker version unreadable", func(_ *testing.T, m *fakeMachine, _ *config.Config) { m.out[dockerVersion] = "" },
			fail, "Couldn't read Docker's version", "docker version"},
		{"chatgpt not signed in", func(t *testing.T, _ *fakeMachine, _ *config.Config) { t.Setenv("FACTORY_CODEX_AUTH", "chatgpt") },
			fail, "isn't signed in", "./factory codex-login"},
		{"no github", func(t *testing.T, _ *fakeMachine, _ *config.Config) { t.Setenv("GITHUB_TOKEN", "") },
			fail, "GitHub: no GitHub credentials", "./factory setup"},
		{"bad app key", func(t *testing.T, _ *fakeMachine, _ *config.Config) {
			t.Setenv("FACTORY_GITHUB_APP_ID", "1")
			t.Setenv("FACTORY_GITHUB_APP_KEY", filepath.Join(t.TempDir(), "none.pem"))
		}, fail, "FACTORY_GITHUB_APP_KEY", "./factory setup"},
		{"no ttyd", func(_ *testing.T, m *fakeMachine, _ *config.Config) { m.missing["ttyd"] = true },
			warn, "ttyd isn't installed", "sudo apt install ttyd"},
		{"port taken", func(_ *testing.T, m *fakeMachine, _ *config.Config) { m.busy = true },
			warn, "is taken", "ss -ltnp | grep :8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCreds(t)
			cfg, err := config.Load(testConfig(t, "o/r"))
			if err != nil {
				t.Fatal(err)
			}
			m := healthy()
			tc.change(t, m, cfg)
			fs := m.doctor(cfg).checkAll(context.Background(), false)
			var got *finding
			for i, f := range fs {
				if f.status == tc.status && strings.Contains(f.what, tc.what) {
					got = &fs[i]
				} else if f.status == fail {
					t.Errorf("another failure: %+v", f)
				}
			}
			if got == nil {
				t.Fatalf("no %d line with %q in %+v", tc.status, tc.what, fs)
			}
			if !strings.Contains(got.fix, tc.fix) {
				t.Errorf("fix = %q, want it to contain %q", got.fix, tc.fix)
			}
		})
	}
}

func TestDoctorAllWell(t *testing.T) {
	clearCreds(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-x")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	cfg, _ := config.Load(testConfig(t, "o/r"))
	fs := healthy().doctor(cfg).checkAll(context.Background(), false)
	var b strings.Builder
	printFindings(&b, fs)
	for _, want := range []string{"✓ git 2.43.0", "✓ Docker 29.1.3", "✓ Job images built: factory-job:latest", "✓ Disk space: 200 GB free in the data directory", "✓ Claude: API key", "✓ Codex: OpenAI API key", "! GitHub: a personal token"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "✗") {
		t.Errorf("a failure:\n%s", b.String())
	}
}

// Docker down: the image check is skipped rather than failing too, and
// setup's system checklist leaves out what depends on factory.env.
func TestDoctorSkipsWhatDependsOnDocker(t *testing.T) {
	clearCreds(t)
	cfg, _ := config.Load(testConfig(t, "o/r"))
	m := healthy()
	m.fail[dockerVersion] = true
	m.fail["docker image inspect factory-job:latest"] = true
	d := m.doctor(cfg)
	fs := d.checkAll(context.Background(), true)
	if len(fs) != 3 {
		t.Fatalf("system checks: %+v", fs)
	}
	for _, f := range d.checkAll(context.Background(), false) {
		if strings.Contains(f.what, "image") {
			t.Errorf("image check ran with Docker down: %+v", f)
		}
	}
	d = m.doctor(cfg)
	d.serving = true
	for _, f := range d.checkAll(context.Background(), false) {
		if strings.Contains(f.what, "port") || strings.Contains(f.what, "disk") {
			t.Errorf("serve checks its own port or disk: %+v", f)
		}
	}
}

// Setup's Docker step prints the shared fix and asks again until Docker
// answers.
func TestSetupDockerRetry(t *testing.T) {
	cfg, _ := config.Load(testConfig(t, "o/r"))
	tries := 0
	old := runCmd
	runCmd = func(out io.Writer, name string, args ...string) error {
		if name == "docker" && args[0] == "version" {
			if tries++; tries == 1 {
				_, _ = out.Write([]byte("permission denied while trying to connect to the Docker daemon socket"))
				return errors.New("exit status 1")
			}
			_, _ = out.Write([]byte("28.1.0\n"))
		}
		return nil
	}
	oldLook := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { runCmd, lookPath = old, oldLook })
	var b strings.Builder
	p := &prompter{in: bufio.NewReader(strings.NewReader("y\n")), out: &b}
	if err := checkDocker(context.Background(), p, cfg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"usermod -aG docker", "Check again?", "Docker works."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestInstallScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if out, err := exec.Command(sh, "-n", "../../scripts/install.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// install.sh against fake go, git and docker on an Ubuntu: an old Go, or
// one GOTOOLCHAIN=local keeps from fetching the toolchain, each gets a ✗
// and its fix, and an old Docker a warning.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script's fixes depend on the platform")
	}
	bin := t.TempDir()
	fake := map[string]string{
		"go": `case "$*" in
"env GOVERSION") echo "go$FAKE_GO" ;;
"env GOTOOLCHAIN") echo "${GOTOOLCHAIN:-auto}" ;;
esac`,
		"git":    `echo "git version 2.43.0"`,
		"docker": `echo 27.5.1`,
	}
	for name, body := range fake {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	osRelease := filepath.Join(bin, "os-release")
	os.WriteFile(osRelease, []byte("ID=ubuntu\nID_LIKE=debian\n"), 0o644)
	for _, tc := range []struct {
		goVersion, toolchain string
		want                 []string
	}{
		{"1.18.1", "", []string{"✗ Go 1.18.1 is too old", "sudo rm -rf /usr/local/go && curl -fsSL https://go.dev/dl/go", "export PATH=/usr/local/go/bin:$PATH", "sudo apt remove golang-go"}},
		{"1.24.7", "local", []string{"✗ Go 1.24.7 is older than", "GOTOOLCHAIN=local stops Go fetching", "unset GOTOOLCHAIN"}},
	} {
		cmd := exec.Command("sh", "../../scripts/install.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "FAKE_GO="+tc.goVersion, "GOTOOLCHAIN="+tc.toolchain,
			"INSTALL_OS_RELEASE="+osRelease, "INSTALL_NO_BUILD=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("Go %s: exit 0", tc.goVersion)
		}
		for _, w := range append(tc.want, "✓ git 2.43.0", "! Docker 27.5.1", "runner.egress.mode: open") {
			if !strings.Contains(string(out), w) {
				t.Errorf("Go %s: no %q in:\n%s", tc.goVersion, w, out)
			}
		}
		if strings.Contains(string(out), "Building") {
			t.Errorf("Go %s: it built anyway", tc.goVersion)
		}
	}
}

// Serve sends what fails to your phone, Docker too old for the egress
// allowlist among it, and only logs the rest.
func TestReportHealth(t *testing.T) {
	clearCreds(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-x")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	cfg, _ := config.Load(testConfig(t, "o/r"))
	m := healthy()
	m.out[dockerVersion] = "27.5.1"
	m.missing["ttyd"] = true
	d := m.doctor(cfg)
	d.serving = true
	var sent []string
	reportHealth(context.Background(), d, func(_ context.Context, msg string) { sent = append(sent, msg) })
	if len(sent) != 1 || !strings.Contains(sent[0], "Docker 27.5.1 is too old for the egress allowlist") || !strings.Contains(sent[0], "Fix: upgrade Docker") {
		t.Fatalf("sent %q", sent)
	}
}
