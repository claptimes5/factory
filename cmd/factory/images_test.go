package main

import (
	"context"
	"errors"
	"io"
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
		if d.built[args[2]] {
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
		d.mu.Unlock()
	case len(args) > 1 && args[0] == "image" && args[1] == "prune":
		io.WriteString(out, "Total reclaimed space: 1.2GB\n")
	}
	return nil
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
	old := runCmd
	runCmd = d.run
	t.Cleanup(func() { runCmd = old })
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
	if a != "build -t x:latest --label io.factory.built=1 --build-arg BASE=ruby:3 dir" {
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
