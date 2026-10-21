package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
)

// What a check found: fine, worth knowing (optional or degraded), or
// something to fix before jobs can run.
const (
	pass = iota
	warn
	fail
)

type finding struct {
	status int
	what   string // "Docker 29.1.3", "Docker isn't running"
	fix    string // a command to run or a doc to read; may be several lines
}

// check is one row of `factory doctor`. system checks are about this
// machine alone, not factory.yaml or factory.env, so setup shows them
// before it has asked for anything.
type check struct {
	system bool
	run    func(d *doctor, ctx context.Context) []finding
}

// doctorChecks are run in order; the Docker one goes first of those that
// use Docker, which skip themselves while it's down.
var doctorChecks = []check{
	{system: true, run: (*doctor).git},
	{system: true, run: func(d *doctor, ctx context.Context) []finding { return one(d.docker(ctx)) }},
	{run: (*doctor).dockerVersion},
	{run: (*doctor).images},
	{run: (*doctor).disk},
	{run: (*doctor).claude},
	{run: (*doctor).codex},
	{run: (*doctor).forges},
	{system: true, run: (*doctor).ttyd},
	{run: (*doctor).port},
}

// doctor runs the checks for one config. Everything it asks of the
// machine goes through the fields, which tests replace.
type doctor struct {
	cfg      *config.Config
	run      func(ctx context.Context, name string, args ...string) (string, error) // combined output
	lookPath func(string) (string, error)
	disks    func(ctx context.Context) ([]runner.Disk, error)
	listen   func(addr string) error
	plat     platform
	serving  bool            // at serve's start: the port is ours and the pipeline watches the disk
	building func() []string // images being built now; nil when none are

	dockerChecked bool
	dockerUp      bool
	dockerVer     string
}

func newDoctor(cfg *config.Config) *doctor {
	d := &doctor{cfg: cfg, lookPath: lookPath, plat: detectPlatform()}
	d.run = func(ctx context.Context, name string, args ...string) (string, error) {
		// A wedged Docker Desktop can leave docker hanging.
		ctx, cancel := context.WithTimeout(ctx, runner.DiskWait)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return string(out), err
	}
	d.listen = func(addr string) error {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
		}
		return err
	}
	d.disks = func(ctx context.Context) ([]runner.Disk, error) {
		data := cfg.Path(cfg.DataDir)
		for data != "/" && data != "." {
			if _, err := os.Stat(data); err == nil {
				break
			}
			data = filepath.Dir(data) // not made yet: where it will be
		}
		free, err := runner.FreeAt(data)
		if err != nil {
			return nil, err
		}
		disks := []runner.Disk{{Where: "the data directory (" + data + ")", Free: free}}
		if d.dockerUp {
			var images []string
			for _, im := range jobImages(cfg) {
				images = append(images, im.tag)
			}
			ds, err := (&runner.Docker{}).Disks(ctx, images)
			return append(disks, ds...), err
		}
		return disks, nil
	}
	return d
}

// lookPath finds a command; tests replace it.
var lookPath = exec.LookPath

// setupWait bounds each of setup's checks, so a hung Docker can't stop
// setup at its first screen.
var setupWait = 15 * time.Second

// runOutput runs a command through runCmd, which setup's tests replace,
// giving up on it after setupWait.
func runOutput(_ context.Context, name string, args ...string) (string, error) {
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var b bytes.Buffer
		err := runCmd(&b, name, args...)
		done <- result{b.String(), err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(setupWait):
		return "", fmt.Errorf("%s didn't answer within %s", name, setupWait)
	}
}

// checkAll runs the checks, only the system ones if system is set.
func (d *doctor) checkAll(ctx context.Context, system bool) []finding {
	var out []finding
	for _, c := range doctorChecks {
		if !system || c.system {
			out = append(out, c.run(d, ctx)...)
		}
	}
	return out
}

func one(f finding) []finding { return []finding{f} }

// printFindings writes the checklist, each fix indented under its line.
func printFindings(w io.Writer, fs []finding) {
	for _, f := range fs {
		fmt.Fprintf(w, "  %s %s\n", [...]string{"✓", "!", "✗"}[f.status], f.what)
		if f.status != pass && f.fix != "" {
			for _, l := range strings.Split(f.fix, "\n") {
				fmt.Fprintf(w, "      %s\n", l)
			}
		}
	}
}

// runDoctor is `factory doctor`: it prints the checklist and fails if
// anything required does.
func runDoctor(ctx context.Context, w io.Writer, cfgPath string, cfg *config.Config, envPath string) error {
	fmt.Fprintf(w, "Checking this machine, %s and %s:\n", cfgPath, envPath)
	fs := newDoctor(cfg).checkAll(ctx, false)
	printFindings(w, fs)
	n := 0
	for _, f := range fs {
		if f.status == fail {
			n++
		}
	}
	if n > 0 {
		fmt.Fprintln(w, "\nFix the ✗ lines and run ./factory doctor again. More in docs/setup.md#checking-your-setup.")
		if n == 1 {
			return errors.New("1 required check failed")
		}
		return fmt.Errorf("%d required checks failed", n)
	}
	fmt.Fprintln(w, "\nEverything the factory needs is here.")
	return nil
}

// healthCheck runs the doctor's checks as serve starts, so a missing
// image, credential or too old a Docker is logged and sent to your phone
// before a job fails on it. Serve starts either way.
func healthCheck(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline, b *imageBuilder) {
	d := newDoctor(cfg)
	d.serving = true
	d.building = b.Building
	reportHealth(ctx, d, p.Notify)
}

// reportHealth logs what the checks found, sending the failures on.
func reportHealth(ctx context.Context, d *doctor, notify func(context.Context, string)) {
	for _, f := range d.checkAll(ctx, false) {
		msg := f.what
		if f.fix != "" {
			msg += ". Fix: " + strings.ReplaceAll(f.fix, "\n", " ")
		}
		switch f.status {
		case fail:
			slog.Error(msg)
			if notify != nil {
				notify(ctx, msg)
			}
		case warn:
			slog.Warn(msg)
		}
	}
}

func (d *doctor) git(ctx context.Context) []finding {
	if _, err := d.lookPath("git"); err != nil {
		return one(finding{fail, "git isn't installed", d.plat.install("git")})
	}
	out, err := d.run(ctx, "git", "--version")
	if err != nil {
		return one(finding{fail, "git doesn't run: " + firstLine(out, err), d.plat.install("git")})
	}
	return one(finding{pass, "git " + version(out), ""})
}

// docker checks that Docker answers this user, once per doctor.
func (d *doctor) docker(ctx context.Context) finding {
	if d.cfg.Runner.Kind != "docker" {
		return finding{pass, "Docker not needed (runner.kind: " + d.cfg.Runner.Kind + ")", ""}
	}
	out, err := d.run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	d.dockerChecked, d.dockerUp = true, err == nil
	switch {
	case err == nil:
		d.dockerVer = version(out)
		if d.dockerVer == "" {
			return finding{pass, "Docker works", ""}
		}
		return finding{pass, "Docker " + d.dockerVer, ""}
	case func() bool { _, err := d.lookPath("docker"); return err != nil }():
		return finding{fail, "Docker isn't installed", d.plat.installDocker()}
	case strings.Contains(strings.ToLower(out), "permission denied"):
		return finding{fail, "Docker refuses you: permission denied on its socket",
			"sudo usermod -aG docker $USER && newgrp docker\n(or log out and back in; on WSL, close and reopen the terminal)"}
	}
	return finding{fail, "Docker isn't running: " + firstLine(out, err), d.plat.startDocker()}
}

// dockerVersion checks Docker is new enough for what factory.yaml uses:
// the egress allowlist and services both need 28, to keep a network's
// gateway off the host.
func (d *doctor) dockerVersion(ctx context.Context) []finding {
	if !d.dockerChecked {
		d.docker(ctx)
	}
	if !d.dockerUp {
		return nil
	}
	var needs []string
	if d.cfg.EgressAllowlist() {
		needs = append(needs, "the egress allowlist")
	}
	if usesServices(d.cfg) {
		needs = append(needs, "services")
	}
	if len(needs) == 0 {
		return nil
	}
	need := strings.Join(needs, " and ")
	v := versionParts(d.dockerVer)
	if v == nil {
		return one(finding{fail, "Couldn't read Docker's version, and " + need + " needs Docker 28 or later", "docker version --format '{{.Server.Version}}'   (should print 28 or later)"})
	}
	if v[0] >= 28 {
		return nil
	}
	fix := d.plat.upgradeDocker()
	if !usesServices(d.cfg) {
		fix += "\nor set runner.egress.mode: open in factory.yaml, which lets jobs reach the whole internet\n(docs/security.md#network-the-egress-allowlist)"
	}
	verb := "needs"
	if len(needs) > 1 {
		verb = "need"
	}
	return one(finding{fail, fmt.Sprintf("Docker %s is too old for %s, which %s 28 or later", d.dockerVer, need, verb), fix})
}

func usesServices(cfg *config.Config) bool {
	return len(cfg.Services) > 0 || slices.ContainsFunc(cfg.Repos, func(r config.Repo) bool { return len(r.Services) > 0 })
}

// fixBuildImages is the fix for a missing image the factory builds.
const fixBuildImages = "./factory images"

// images checks every repo's job image is built, since without it every
// job fails.
func (d *doctor) images(ctx context.Context) []finding {
	if d.cfg.Runner.Kind != "docker" || !d.dockerUp {
		return nil
	}
	ours := map[string]bool{}
	for _, im := range jobImages(d.cfg) {
		ours[im.tag] = true
	}
	var out []finding
	var built []string
	seen := map[string]bool{}
	var building []string
	if d.building != nil {
		building = d.building()
	}
	check := func(image, n string) {
		if seen[image] {
			return
		}
		seen[image] = true
		if _, err := d.run(ctx, "docker", "image", "inspect", image); err == nil {
			built = append(built, image)
			return
		}
		if slices.Contains(building, image) {
			// Jobs that need it wait for the build, so nothing fails yet.
			out = append(out, finding{warn, fmt.Sprintf("Job image %s (for %s) is being built", image, n), ""})
			return
		}
		fix := fixBuildImages
		if !ours[image] {
			fix = "build it yourself: " + n + "'s image names one the factory doesn't build\n(docs/configuration.md#each-repos-toolchain-and-checks)"
		}
		out = append(out, finding{fail, fmt.Sprintf("Job image %s (for %s) isn't built", image, n), fix})
	}
	for _, n := range d.cfg.RepoNames() {
		r, _ := d.cfg.RepoConf(n)
		check(r.Image, n)
	}
	if usesRunnerImage(d.cfg) {
		check(d.cfg.Runner.Image, runnerImageUsers)
	}
	if len(built) > 0 {
		out = append([]finding{{pass, "Job images built: " + strings.Join(built, ", "), ""}}, out...)
	}
	return out
}

// disk checks there is room to start jobs (budgets.min_free_disk_gb).
// The daemon watches this itself while it runs.
func (d *doctor) disk(ctx context.Context) []finding {
	min := d.cfg.Budgets.MinFreeDiskGB
	if d.serving || min <= 0 {
		return nil
	}
	disks, err := d.disks(ctx)
	var low, fine []string
	for _, k := range disks {
		s := fmt.Sprintf("%s has %s free", k.Where, gb(k.Free))
		if float64(k.Free) < min*1e9 {
			low = append(low, s)
		} else {
			fine = append(fine, gb(k.Free)+" free in "+k.Where)
		}
	}
	switch {
	case len(low) > 0:
		return one(finding{fail, "Low on disk: " + strings.Join(low, "; "),
			fmt.Sprintf("free some space: no job starts with less than %g GB (budgets.min_free_disk_gb)\ndocker system prune removes stopped containers and unused images", min)})
	case err != nil:
		return one(finding{warn, "Couldn't measure the disk: " + err.Error(), ""})
	}
	return one(finding{pass, "Disk space: " + strings.Join(fine, ", "), ""})
}

func gb(b uint64) string { return fmt.Sprintf("%.0f GB", float64(b)/1e9) }

func (d *doctor) claude(context.Context) []finding {
	if !usesHarness(d.cfg, "claude") {
		return nil
	}
	switch {
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return one(finding{pass, "Claude: API key", ""})
	case os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "":
		return one(finding{pass, "Claude: subscription token", ""})
	case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" || os.Getenv("CLAUDE_CODE_USE_BEDROCK") != "" || os.Getenv("CLAUDE_CODE_USE_VERTEX") != "":
		if d.cfg.Runner.Kind != "docker" {
			return one(finding{pass, "Claude: from the environment", ""})
		}
		// Only these two are passed into job containers (see secrets).
		return one(finding{fail, "Claude: ANTHROPIC_AUTH_TOKEN, Bedrock and Vertex sign-ins don't reach docker jobs",
			"set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN; ./factory setup asks for one (docs/setup.md#what-setup-asks-for)"})
	}
	return one(finding{fail, "No Claude credential (ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN)",
		"./factory setup asks for one (docs/setup.md#what-setup-asks-for)"})
}

// codex checks Codex can sign in, only when routing uses it.
func (d *doctor) codex(context.Context) []finding {
	if !usesHarness(d.cfg, "codex") {
		return nil
	}
	mode, err := codexAuth()
	switch {
	case err != nil:
		return one(finding{fail, err.Error(), "fix FACTORY_CODEX_AUTH in factory.env, or run ./factory setup"})
	case mode == codexAccessToken:
		return one(finding{pass, "Codex: CODEX_ACCESS_TOKEN", ""})
	case mode == codexChatGPT:
		if b, err := os.ReadFile(codexAuthFile(d.cfg)); err != nil || !harness.ValidAuth(b) {
			return one(finding{fail, "Codex is set to a ChatGPT plan but isn't signed in", "./factory codex-login (docs/setup.md#codex-on-a-chatgpt-plan)"})
		}
		return one(finding{pass, "Codex: ChatGPT plan, signed in", ""})
	case os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "":
		return one(finding{pass, "Codex: OpenAI API key", ""})
	}
	return one(finding{fail, "factory.yaml routes " + codexStage(d.cfg) + " to Codex, but there's no OpenAI API key or ChatGPT sign-in",
		"./factory setup asks for one; no OpenAI account? see docs/configuration.md#claude-only-review"})
}

// codexStage names a stage routed to Codex: review if it is, else the
// first in sorted order.
func codexStage(cfg *config.Config) string {
	var stages []string
	for k, r := range cfg.Routing {
		if r.Harness == "codex" {
			stages = append(stages, k)
		}
	}
	slices.Sort(stages)
	if slices.Contains(stages, "review") {
		return "review"
	}
	if len(stages) == 0 {
		return "a stage"
	}
	return stages[0]
}

// forges checks the credentials for each host the repos are on, the way
// serve does before it starts; nothing is sent to the host.
func (d *doctor) forges(context.Context) []finding {
	var out []finding
	if d.cfg.NoRepos() {
		return one(finding{fail, "No repositories yet: the factory doesn't know which repos to work on", "./factory setup, or the get-started page of ./factory serve"})
	}
	if repos := githubRepos(d.cfg); len(repos) > 0 {
		switch _, err := tokens(d.cfg, repos[0]); {
		case err != nil:
			out = append(out, finding{fail, "GitHub: " + err.Error(), "./factory setup (docs/setup.md#what-setup-asks-for)"})
		case os.Getenv("FACTORY_GITHUB_APP_ID") != "":
			out = append(out, finding{pass, "GitHub: app " + os.Getenv("FACTORY_GITHUB_APP_ID") + ", key " + os.Getenv("FACTORY_GITHUB_APP_KEY"), ""})
		default:
			out = append(out, finding{warn, "GitHub: a personal token (GITHUB_TOKEN), meant for a quick try",
				"./factory setup can create a GitHub App limited to your repos"})
		}
	}
	if len(gitlabRepos(d.cfg)) > 0 {
		if _, err := gitlabToken(); err != nil {
			out = append(out, finding{fail, "GitLab: " + err.Error(), "./factory setup (docs/setup.md#what-setup-asks-for)"})
		} else {
			out = append(out, finding{pass, "GitLab: token", ""})
		}
	}
	return out
}

// ttyd is optional: without it the dashboard's attach button prints a
// docker exec command instead of opening a browser terminal.
func (d *doctor) ttyd(context.Context) []finding {
	if d.cfg.Runner.Kind != "docker" {
		return nil
	}
	if _, err := d.lookPath("ttyd"); err != nil {
		return one(finding{warn, "ttyd isn't installed (optional: browser terminals into running jobs)", d.plat.install("ttyd")})
	}
	return one(finding{pass, "ttyd", ""})
}

func (d *doctor) port(context.Context) []finding {
	if d.serving {
		return nil
	}
	if err := d.listen(d.cfg.Web.Listen); err != nil {
		_, port, _ := net.SplitHostPort(d.cfg.Web.Listen)
		who := "ss -ltnp | grep :" + port
		if d.plat.family == "mac" {
			who = "lsof -iTCP:" + port + " -sTCP:LISTEN"
		}
		return one(finding{warn, "web.listen " + d.cfg.Web.Listen + " is taken; is ./factory serve already running?",
			who + "   (or change web.listen in factory.yaml)"})
	}
	return one(finding{pass, "Dashboard port " + d.cfg.Web.Listen + " is free", ""})
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return err.Error()
}

var versionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// version finds the version number in a tool's output: "28.0.0" in
// "28.0.0-rc.1", "27.5.1" in "Docker version 27.5.1, build 9f9e405",
// "1.26.3" in "go version go1.26.3 linux/amd64". "" if there is none.
func version(out string) string { return versionRE.FindString(out) }

// versionParts is version split into numbers, nil if there is none.
func versionParts(out string) []int {
	v := version(out)
	if v == "" {
		return nil
	}
	var n []int
	for _, p := range strings.Split(v, ".") {
		i, _ := strconv.Atoi(p)
		n = append(n, i)
	}
	return n
}

// platform says how to install things here, for the fix lines.
type platform struct {
	family string // debian, fedora, arch, mac, or "" if unknown
	wsl    bool
}

func detectPlatform() platform {
	if runtime.GOOS == "darwin" {
		return platform{family: "mac"}
	}
	b, _ := os.ReadFile("/etc/os-release")
	return platform{family: osFamily(string(b)), wsl: runner.WSL()}
}

// osFamily reads /etc/os-release's ID and ID_LIKE.
func osFamily(osRelease string) string {
	var ids []string
	for _, l := range strings.Split(osRelease, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && (k == "ID" || k == "ID_LIKE") {
			ids = append(ids, strings.Fields(strings.Trim(v, `"'`))...)
		}
	}
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "debian"
		case "fedora", "rhel", "centos":
			return "fedora"
		case "arch":
			return "arch"
		}
	}
	return ""
}

func (p platform) install(pkg string) string {
	switch p.family {
	case "debian":
		return "sudo apt install " + pkg
	case "fedora":
		return "sudo dnf install " + pkg
	case "arch":
		return "sudo pacman -S " + pkg
	case "mac":
		return "brew install " + pkg
	}
	return "install " + pkg + " with your system's package manager"
}

const getDocker = "curl -fsSL https://get.docker.com | sh   (Docker's own packages; a distro's may be older than 28)"

func (p platform) installDocker() string {
	switch {
	case p.wsl:
		return "install Docker Desktop for Windows and turn on Settings > Resources > WSL integration for this distro,\nor install Docker inside WSL: " + getDocker + "\n(docs/setup.md#windows-wsl2)"
	case p.family == "mac":
		return "brew install --cask docker-desktop   (jobs need Linux or WSL2, though: docs/setup.md#dependencies)"
	case p.family == "arch":
		return "sudo pacman -S docker && sudo systemctl enable --now docker"
	}
	return getDocker + "\nthen: sudo systemctl enable --now docker"
}

func (p platform) startDocker() string {
	switch {
	case p.wsl:
		return "start Docker Desktop, with WSL integration on for this distro (docs/setup.md#windows-wsl2),\nor, for Docker installed inside WSL: sudo service docker start"
	case p.family == "mac":
		return "open -a Docker"
	}
	return "sudo systemctl start docker"
}

func (p platform) upgradeDocker() string {
	switch {
	case p.wsl || p.family == "mac":
		return "update Docker Desktop (or, inside WSL: " + getDocker + ")"
	case p.family == "arch":
		return "sudo pacman -Syu docker"
	}
	return "upgrade Docker: " + getDocker
}

// checkDocker makes sure Docker works for this user, printing the fix and
// asking to check again until it does or you skip it; then that it is new
// enough for factory.yaml.
func checkDocker(ctx context.Context, p *prompter, cfg *config.Config) error {
	d := newDoctor(cfg)
	d.run = runOutput
	for {
		f := d.docker(ctx)
		if f.status == pass {
			break
		}
		p.say("%s. To fix it:", f.what)
		for _, l := range strings.Split(f.fix, "\n") {
			p.say("    %s", l)
		}
		if !p.yes("Check again?", true) {
			p.say("Skipping; jobs can't run until Docker works. ./factory doctor checks it again.")
			return nil
		}
	}
	p.say("Docker works.")
	printFindings(p.out, d.dockerVersion(ctx))
	return nil
}

// checkMachine is setup's opening checklist: what this machine has,
// before anything is asked.
func checkMachine(ctx context.Context, p *prompter, cfg *config.Config) {
	d := newDoctor(cfg)
	d.run = runOutput
	p.say("\nChecking this machine:")
	printFindings(p.out, d.checkAll(ctx, true))
}
