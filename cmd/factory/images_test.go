package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/web"
)

// fakeDocker records docker commands. An image counts as built once a
// build of it ran; a build waits for release when gate is set.
type fakeDocker struct {
	mu       sync.Mutex
	cmds     []string
	built    map[string]bool
	labels   map[string]string // the source label each build gave
	down     bool
	failTag  string
	gate     chan struct{}
	building chan string
}

func (d *fakeDocker) run(out io.Writer, name string, args ...string) error {
	d.mu.Lock()
	d.cmds = append(d.cmds, strings.Join(append([]string{name}, args...), " "))
	d.mu.Unlock()
	if name != "docker" {
		return nil
	}
	if d.down {
		return errors.New("Cannot connect to the Docker daemon")
	}
	switch {
	case len(args) > 2 && args[0] == "image" && args[1] == "inspect":
		d.mu.Lock()
		defer d.mu.Unlock()
		if tag := args[len(args)-1]; d.built[tag] {
			io.WriteString(out, d.labels[tag])
			return nil
		}
		return errors.New("no such image")
	case len(args) > 2 && args[0] == "build":
		tag := args[2]
		if d.building != nil {
			d.building <- tag
		}
		if d.gate != nil {
			<-d.gate
		}
		if tag == d.failTag {
			io.WriteString(out, "ERROR: apt-get failed\n")
			return errors.New("exit status 1")
		}
		d.mu.Lock()
		d.built[tag] = true
		if d.labels == nil {
			d.labels = map[string]string{}
		}
		for i, a := range args {
			if l, ok := strings.CutPrefix(a, sourceLabel+"="); ok && args[i-1] == "--label" {
				d.labels[tag] = l
			}
		}
		d.mu.Unlock()
	case len(args) > 1 && args[0] == "image" && args[1] == "prune":
		io.WriteString(out, "Total reclaimed space: 1.2GB\n")
	}
	return nil
}

// runContext is run, stopped early when ctx ends.
func (d *fakeDocker) runContext(ctx context.Context, out io.Writer, name string, args ...string) error {
	done := make(chan error, 1)
	go func() { done <- d.run(out, name, args...) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *fakeDocker) ran(prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.cmds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func useDocker(t *testing.T, d *fakeDocker) {
	t.Helper()
	if d.built == nil {
		d.built = map[string]bool{}
	}
	if d.labels == nil {
		d.labels = map[string]string{}
	}
	old, oldCtx := runCmd, runCmdContext
	runCmd = d.run
	runCmdContext = d.runContext
	t.Cleanup(func() { runCmd, runCmdContext = old, oldCtx })
}

func imagesConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildArgsLabelTheImage(t *testing.T) {
	a := strings.Join(jobImage{tag: "x:latest", from: "ruby:3"}.buildArgs("dir"), " ")
	if a != "build -t x:latest --label io.factory.built=1 --label io.factory.source=from=ruby:3 --build-arg BASE=ruby:3 dir" {
		t.Fatal(a)
	}
}

func TestAutoBuildHoldsJobsUntilBuilt(t *testing.T) {
	d := &fakeDocker{gate: make(chan struct{}), building: make(chan string, 1)}
	useDocker(t, d)
	cfg := imagesConfig(t)
	b := &imageBuilder{}
	t.Cleanup(b.wait) // before the fakes are put back
	done := make(chan struct{})
	b.onDone(func() { close(done) })
	var notes []string
	autoBuild(context.Background(), cfg, b, func(_ context.Context, m string) { notes = append(notes, m) })

	tag := <-d.building
	if got := b.Building(); !slices.Equal(got, []string{tag}) || tag != cfg.Runner.Image {
		t.Fatalf("building %v, tag %s", got, tag)
	}
	if _, err := b.start(cfg, nil); !errors.Is(err, errBuildRunning) {
		t.Fatalf("second start: %v", err)
	}
	// The health check says it is being built, not that it is missing.
	m := healthy()
	m.fail["docker image inspect "+tag] = true
	doc := m.doctor(cfg)
	doc.building = b.Building
	doc.dockerUp = true
	fs := doc.images(context.Background())
	if len(fs) != 1 || fs[0].status != warn || !strings.Contains(fs[0].what, "is being built") {
		t.Fatalf("findings %+v", fs)
	}

	close(d.gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("build didn't finish")
	}
	if len(b.Building()) != 0 || len(notes) != 0 {
		t.Fatalf("building %v, notes %v", b.Building(), notes)
	}
	if running, log, result, failed := b.status(); running || failed || !strings.Contains(log, "Building "+tag) || result == "" {
		t.Fatalf("status %v %q %q %v", running, log, result, failed)
	}
	prunes := d.ran("docker image prune")
	if len(prunes) != 1 || prunes[0] != "docker image prune -f --filter label=io.factory.built=1" {
		t.Fatalf("prunes %q", prunes)
	}
	// Built now: a second start has nothing to do.
	if n, err := b.start(cfg, nil); n != 0 || err != nil {
		t.Fatalf("start = %d, %v", n, err)
	}
}

func TestAutoBuildFailureNotifies(t *testing.T) {
	d := &fakeDocker{}
	useDocker(t, d)
	cfg := imagesConfig(t)
	d.failTag = cfg.Runner.Image
	b := &imageBuilder{}
	t.Cleanup(b.wait) // before the fakes are put back
	done := make(chan struct{})
	b.onDone(func() { close(done) })
	notes := make(chan string, 1)
	autoBuild(context.Background(), cfg, b, func(_ context.Context, m string) { notes <- m })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("build didn't finish")
	}
	if n := <-notes; !strings.Contains(n, "Couldn't build a job image") {
		t.Fatal(n)
	}
	// Jobs are no longer held: they fail as they would without the build.
	if len(b.Building()) != 0 {
		t.Fatalf("building %v", b.Building())
	}
	if _, _, result, failed := b.status(); !failed || !strings.Contains(result, "building "+cfg.Runner.Image) {
		t.Fatalf("result %q", result)
	}
	if p := d.ran("docker image prune"); len(p) != 0 {
		t.Fatalf("pruned after a failed build: %q", p)
	}
}

func TestAutoBuildSkipsWithoutDocker(t *testing.T) {
	d := &fakeDocker{down: true}
	useDocker(t, d)
	cfg := imagesConfig(t)
	b := &imageBuilder{}
	t.Cleanup(b.wait) // before the fakes are put back
	autoBuild(context.Background(), cfg, b, nil)
	pruneImages(cfg)
	if c := d.ran("docker build"); len(c) != 0 {
		t.Fatalf("built %q", c)
	}
	if c := d.ran("docker image prune"); len(c) != 0 {
		t.Fatalf("pruned %q", c)
	}

	d.down = false
	before := len(d.ran("docker"))
	cfg.Runner.Kind = "local"
	autoBuild(context.Background(), cfg, b, nil)
	pruneImages(cfg)
	if len(d.ran("docker")) != before {
		t.Fatalf("ran %q", d.ran("docker"))
	}
}

func TestPruneLoopPrunesHourly(t *testing.T) {
	d := &fakeDocker{}
	useDocker(t, d)
	cfg := imagesConfig(t)
	old := pruneEvery
	pruneEvery = 10 * time.Millisecond
	t.Cleanup(func() { pruneEvery = old })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { pruneLoop(ctx, cfg); close(stopped) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(d.ran("docker image prune")) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("didn't prune again")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-stopped
	for _, c := range d.ran("docker image prune") {
		if c != "docker image prune -f --filter label=io.factory.built=1" {
			t.Fatalf("prune %q", c)
		}
	}
}

func TestImagesCommandPrunesAfterBuilding(t *testing.T) {
	d := &fakeDocker{}
	useDocker(t, d)
	cfg := imagesConfig(t)
	if err := buildImages(io.Discard, cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(d.ran("docker build")) != 1 || len(d.ran("docker image prune")) != 1 {
		t.Fatalf("ran %q", d.cmds)
	}
	// Nothing built, nothing pruned.
	if err := buildImages(io.Discard, cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(d.ran("docker image prune")) != 1 {
		t.Fatalf("ran %q", d.cmds)
	}
}

func TestWizardShowsServesBuild(t *testing.T) {
	d := &fakeDocker{gate: make(chan struct{}), building: make(chan string, 1), built: map[string]bool{}}
	w := testWizard(t, &fakeGitHub{}, "o/r", false, d.run)
	b := &imageBuilder{}
	t.Cleanup(b.wait) // before the fakes are put back
	w.builder = b
	autoBuild(context.Background(), w.cfg, b, nil)
	<-d.building
	ctx := context.Background()
	if st := w.State(ctx, false); st.Images == nil || !st.Images.Building {
		t.Fatalf("images = %+v", st.Images)
	}
	// The page's Build button joins nothing: one build at a time.
	if _, err := w.Do(ctx, "images/build", web.WizardInput{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second build: %v", err)
	}
	close(d.gate)
	deadline := time.Now().Add(5 * time.Second)
	for w.State(ctx, false).Images.Building {
		if time.Now().After(deadline) {
			t.Fatal("build didn't finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A hung build is stopped, so its jobs and a restart don't wait for ever.
func TestImageBuildTimesOut(t *testing.T) {
	d := &fakeDocker{gate: make(chan struct{})}
	useDocker(t, d)
	t.Cleanup(func() { close(d.gate) })
	old := buildWait
	buildWait = 50 * time.Millisecond
	t.Cleanup(func() { buildWait = old })
	cfg := imagesConfig(t)
	b := &imageBuilder{}
	if _, err := b.start(cfg, nil); err != nil {
		t.Fatal(err)
	}
	b.wait()
	if running, _, result, failed := b.status(); running || !failed || !strings.Contains(result, "took over") || len(b.Building()) != 0 {
		t.Fatalf("status %v %q %v, building %v", running, result, failed, b.Building())
	}
}

// An image built before factory.yaml named another base counts as missing,
// so serve builds it again; one the factory didn't build is left alone.
func TestImageBuiltFromAnotherBaseIsMissing(t *testing.T) {
	d := &fakeDocker{}
	useDocker(t, d)
	path := testConfig(t, "o/r")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "repo: o/r", "repos:\n  - {name: o/api, image_from: ruby:3.4-bookworm}\n", 1)), 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.On, cfg.Tuning.Every, cfg.Grooming.Every = false, 0, 0
	if err := buildImages(io.Discard, cfg, false); err != nil {
		t.Fatal(err)
	}
	if m := missingImages(cfg); len(m) != 0 {
		t.Fatalf("just built, missing %+v", m)
	}
	cfg.Repos[0].ImageFrom = "ruby:3.3-bookworm"
	if m := missingImages(cfg); len(m) != 1 || m[0].tag != "factory-job-o__api:latest" {
		t.Fatalf("after a new base, missing %+v", m)
	}
	cfg.Repos[0].ImageFrom = "ruby:3.4-bookworm"
	if m := missingImages(cfg); len(m) != 0 {
		t.Fatalf("back to its base, missing %+v", m)
	}
	// Built before images had the label: built again once.
	if im := (jobImage{tag: "x", from: "y"}); im.current("", nil) || !im.current("from=y\n", nil) || im.current("from=y", errors.New("no such image")) {
		t.Fatal("current is wrong")
	}
}

// An image built from a repo's Dockerfile is rebuilt once the Dockerfile
// on the base branch isn't the one it was built from, and its items say
// they wait for it.
func TestDockerfileChangeRebuildsTheImage(t *testing.T) {
	df := "FROM ruby:3.4 AS test\n"
	var refs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/api/contents/Dockerfile" {
			http.NotFound(w, r)
			return
		}
		refs = append(refs, r.URL.Query().Get("ref"))
		fakeContents(w, map[string]string{"Dockerfile": df}, "Dockerfile")
	}))
	t.Cleanup(srv.Close)
	oldAPI := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = oldAPI })
	t.Setenv("FACTORY_GITHUB_APP_ID", "")
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	label := "" // the image's dockerfile label; "-" for no image
	old := runCmd
	runCmd = func(out io.Writer, _ string, args ...string) error {
		if len(args) > 1 && args[1] == "inspect" {
			if label == "-" {
				return errors.New("no such image")
			}
			io.WriteString(out, label+"\n")
		}
		return nil
	}
	t.Cleanup(func() { runCmd = old })

	path := testConfig(t, "o/r")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "repo: o/r", "repos:\n  - {name: o/api, base_branch: release, image_build: {target: test}}\n", 1)), 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	label = dockerfileHash(df)
	if c := dockerfileChanged(ctx, cfg); len(c) != 0 {
		t.Fatalf("unchanged, rebuilds %+v", c)
	}
	if strings.Join(refs, ",") != "release" {
		t.Fatalf("read the Dockerfile at %v, not the base branch", refs)
	}
	df = "FROM ruby:3.4 AS test\nRUN apt-get install -y ffmpeg\n"
	if c := dockerfileChanged(ctx, cfg); len(c) != 1 || c[0].tag != "factory-job-o__api:latest" {
		t.Fatalf("changed, rebuilds %+v", c)
	}
	label = "-" // not built: that's autoBuild's to build
	if c := dockerfileChanged(ctx, cfg); len(c) != 0 {
		t.Fatalf("missing, rebuilds %+v", c)
	}

	bl := &imageBuilder{building: []string{"factory-job-o__api:latest"}, why: "the repo's Dockerfile changed on release"}
	if w := bl.Waiting(cfg, "o/api"); w != "Waits for its job image, which is being built: the repo's Dockerfile changed on release. It starts by itself when the build is done." {
		t.Fatalf("waiting %q", w)
	}
	if w := (&imageBuilder{}).Waiting(cfg, "o/api"); w != "" {
		t.Fatalf("nothing building, waiting %q", w)
	}
}
