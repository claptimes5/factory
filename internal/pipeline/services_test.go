package pipeline

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// svcRunner runs commands on the host and records services.
type svcRunner struct {
	runner.Local
	mu      sync.Mutex
	started []string
	stopped []string
	specs   []runner.Spec
}

func (r *svcRunner) Run(ctx context.Context, s runner.Spec, w io.Writer) (int, error) {
	r.mu.Lock()
	r.specs = append(r.specs, s)
	r.mu.Unlock()
	return r.Local.Run(ctx, s, w)
}

func (r *svcRunner) StartServices(_ context.Context, job string, svcs []runner.Service, _ time.Duration) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range svcs {
		r.started = append(r.started, job+":"+s.Name+"="+s.Image)
	}
	return job + "-svc", nil
}

func (r *svcRunner) StopServices(_ context.Context, job string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, job)
	return nil
}

// A repo's services run for the whole implement job: the builder and
// every check get their network, and they stop when the job ends.
// Reviewers don't get them.
func TestServicesRunForImplementJobs(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	r := &svcRunner{}
	e.p.Runner, e.p.GateExec = r, nil
	e.p.Cfg.Services = map[string]config.Service{"db": {Image: "postgres:16"}, "cache": {Image: "redis:7"}}
	e.p.Cfg.Repos[0].Services = nil
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 1).ID, Kind: "implement"})
	if len(jobs) != 1 {
		t.Fatalf("implement jobs = %d", len(jobs))
	}
	job := e.p.containerName(jobs[0].ID)
	if got := strings.Join(r.started, ","); got != job+":cache=redis:7,"+job+":db=postgres:16" {
		t.Fatalf("started %s", got)
	}
	if len(r.stopped) != 1 || r.stopped[0] != job {
		t.Fatalf("stopped %v", r.stopped)
	}
	if n := e.claude.nets["implement"]; len(n) != 1 || n[0] != job+"-svc" {
		t.Fatalf("builder services = %v", n)
	}
	if n := e.codex.nets["review"]; len(n) != 1 || n[0] != "" {
		t.Fatalf("reviewer services = %v", n)
	}
	if len(r.specs) == 0 {
		t.Fatal("no gate ran")
	}
	for _, s := range r.specs {
		if s.Services != job+"-svc" {
			t.Fatalf("gate %s ran with services %q", s.Name, s.Services)
		}
	}
	if e.p.servicesOf(jobs[0].ID) != "" {
		t.Fatal("the job's services are still registered")
	}
}
