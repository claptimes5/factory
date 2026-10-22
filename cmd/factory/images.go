package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
)

// imageLabel marks every image the factory builds, so pruning old copies
// never touches anyone else's images.
const imageLabel = "io.factory.built"

// buildWait is the longest one image's build may take.
var buildWait = 60 * time.Minute

// pruneEvery is how often serve removes old copies of the factory's images.
var pruneEvery = time.Hour

// imageBuilder builds the job images, one build at a time. serve's start
// and the get-started page share one, so an image is never built twice at
// once, and jobs that need an image wait while it builds.
type imageBuilder struct {
	// tokens reach a repo to clone it, for an image built from its own
	// Dockerfile; nil is the credentials in the environment.
	tokens   func(*config.Config, string) (github.TokenSource, error)
	mu       sync.Mutex
	running  bool
	building []string // tags this build still has to make
	log      *tailBuffer
	result   string
	failed   bool
	after    []func() // run after every build, whoever started it
	done     sync.WaitGroup
}

// wait returns once no build is running, its clean-up and callbacks included.
func (b *imageBuilder) wait() { b.done.Wait() }

// onDone runs f after every build ends.
func (b *imageBuilder) onDone(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.after = append(b.after, f)
}

// errBuildRunning refuses a second build while one runs.
var errBuildRunning = errors.New("a build is already running")

// Building names the images being built now.
func (b *imageBuilder) Building() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.building)
}

// status is the running or last build, for the page.
func (b *imageBuilder) status() (running bool, log, result string, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.log != nil {
		log = b.log.String()
	}
	return b.running, log, b.result, b.failed
}

// missingImages are the job images the factory builds that aren't built.
func missingImages(cfg *config.Config) []jobImage {
	var out []jobImage
	for _, im := range jobImages(cfg) {
		if !im.isBuilt() {
			out = append(out, im)
		}
	}
	return out
}

// start builds the missing images in the background, then calls done
// (which may be nil) with how it went and what it built. It reports how
// many it is building; none when every one is built.
func (b *imageBuilder) start(cfg *config.Config, done func(err error, built []string)) (int, error) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return 0, errBuildRunning
	}
	b.running = true // held while missingImages looks, so two starts can't race
	b.mu.Unlock()
	missing := missingImages(cfg)
	b.mu.Lock()
	if len(missing) == 0 {
		b.running = false
		b.mu.Unlock()
		return 0, nil
	}
	b.building, b.log, b.result, b.failed = nil, &tailBuffer{max: 16 << 10}, "", false
	for _, im := range missing {
		b.building = append(b.building, im.tag)
	}
	log := b.log
	b.mu.Unlock()
	b.done.Add(1)
	go func() {
		defer b.done.Done()
		built, err := b.build(cfg, missing, log)
		b.mu.Lock()
		b.running, b.building = false, nil
		if err != nil {
			b.result, b.failed = err.Error(), true
		} else {
			b.result = "The job images are built."
		}
		after := slices.Clone(b.after)
		b.mu.Unlock()
		if len(built) > 0 {
			pruneImages(cfg)
		}
		if done != nil {
			done(err, built)
		}
		for _, f := range after {
			f()
		}
	}()
	return len(missing), nil
}

func (b *imageBuilder) build(cfg *config.Config, images []jobImage, out io.Writer) ([]string, error) {
	dir := cfg.Path("deploy/job-image")
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return nil, fmt.Errorf("can't find %s next to the config; run the factory from its checkout", dir)
	}
	var built []string
	for _, im := range images {
		slog.Info("building job image", "image", im.tag, "for", joinAnd(im.repos))
		fmt.Fprintf(out, "Building %s for %s...\n", im.tag, joinAnd(im.repos))
		// A hung build mustn't hold its jobs, and a restart, for ever.
		ctx, cancel := context.WithTimeout(context.Background(), buildWait)
		tok := b.tokens
		if tok == nil {
			tok = tokens
		}
		err := im.make(ctx, out, cfg, dir, tok, runCmdContext)
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut {
			return built, fmt.Errorf("building %s: it took over %s, so it was stopped", im.tag, buildWait)
		}
		if err != nil {
			return built, fmt.Errorf("building %s: %w", im.tag, err)
		}
		slog.Info("built job image", "image", im.tag)
		built = append(built, im.tag)
		b.mu.Lock()
		b.building = slices.DeleteFunc(b.building, func(t string) bool { return t == im.tag })
		b.mu.Unlock()
	}
	return built, nil
}

// dockerUp says whether Docker answers this user.
func dockerUp() bool {
	return runCmd(io.Discard, "docker", "version", "--format", "{{.Server.Version}}") == nil
}

// autoBuild builds the job images that are missing as serve starts, so
// the first jobs wait for them instead of failing. A failure is logged
// and sent to your phone; jobs then fail as they would without it.
func autoBuild(ctx context.Context, cfg *config.Config, b *imageBuilder, notify func(context.Context, string)) {
	if cfg.Runner.Kind != "docker" || !dockerUp() {
		return
	}
	n, err := b.start(cfg, func(err error, built []string) {
		if err != nil {
			msg := "Couldn't build a job image: " + err.Error() + ". Jobs that need it fail until it's built; build it from the dashboard's get-started page."
			slog.Error(msg)
			if notify != nil {
				notify(ctx, msg)
			}
			return
		}
		slog.Info("job images built; waiting jobs can start", "images", strings.Join(built, ","))
	})
	if err != nil {
		slog.Warn("job images", "err", err)
	} else if n > 0 {
		slog.Info("building missing job images in the background; jobs that need them wait", "count", n)
	}
}

// pruneImages removes the factory's images that no tag or container uses
// any more: old copies of rebuilt images. Only images with imageLabel
// are touched. It does nothing without Docker, and only logs a failure.
func pruneImages(cfg *config.Config) {
	if cfg.Runner.Kind != "docker" || !dockerUp() {
		return
	}
	var out bytes.Buffer
	if err := runCmd(&out, "docker", "image", "prune", "-f", "--filter", "label="+imageLabel+"=1"); err != nil {
		slog.Warn("removing old job images", "err", err, "output", strings.TrimSpace(out.String()))
		return
	}
	if l := lastLine(out.String()); l != "" {
		slog.Info("removed old job images", "result", l)
	}
}

// pruneLoop prunes now and every pruneEvery until ctx ends.
func pruneLoop(ctx context.Context, cfg *config.Config) {
	pruneImages(cfg)
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pruneImages(cfg)
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
