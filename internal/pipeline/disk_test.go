package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
)

// diskRunner is the local runner with a Docker disk the test controls.
type diskRunner struct {
	runner.Local
	free uint64
	err  error
}

func (d *diskRunner) Disks(context.Context, []string) ([]runner.Disk, error) {
	if d.err != nil {
		return nil, d.err
	}
	return []runner.Disk{{Where: "Docker's disk", Free: d.free}}, nil
}

// No job starts without room on disk: a drain says why, you hear about it
// once, and again when there's room with some to spare.
func TestNoJobsWhenDiskIsLow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.freeAtFn = func(string) (uint64, error) { return 1e12, nil }
	dr := &diskRunner{free: 3e9}
	e.p.Runner = dr
	var said []string
	e.p.Notify = func(_ context.Context, m string) { said = append(said, m) }
	e.p.Cfg.Budgets.MinFreeDiskGB = 10
	e.addIssue(1)
	again := func() { e.p.diskAt = e.p.diskAt.Add(-DiskEvery) }
	for range 2 {
		if err := e.p.Drain(ctx); !errors.Is(err, ErrDiskLow) {
			t.Fatalf("drain: %v", err)
		}
		again()
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, "triage"); n != 1 {
		t.Fatalf("queued triage = %d", n)
	}
	if len(said) != 1 || !strings.Contains(said[0], "Docker's disk has 3.0 GB free") || !strings.Contains(e.p.DiskLow(), "Low on disk") {
		t.Fatalf("notified %q", said)
	}
	// Docker can't be measured: still low, not "room again".
	dr.err = errors.New("docker info: timed out")
	if err := e.p.Drain(ctx); !errors.Is(err, ErrDiskLow) {
		t.Fatalf("drain with Docker unmeasured: %v", err)
	}
	again()
	// Just over the limit isn't enough to resume.
	dr.err, dr.free = nil, 10.5e9
	if err := e.p.Drain(ctx); !errors.Is(err, ErrDiskLow) {
		t.Fatalf("drain at the limit: %v", err)
	}
	again()
	dr.free = 50e9
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.p.Store.ItemByIssue(ctx, "o/r", 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	if len(said) < 2 || !strings.Contains(said[1], "room on disk again") || e.p.DiskLow() != "" {
		t.Fatalf("notified %q", said)
	}
	// Off: never measured.
	e.p.Cfg.Budgets.MinFreeDiskGB, dr.free = -1, 0
	again()
	if e.p.diskLow(ctx) != "" {
		t.Fatal("a negative minimum should turn the check off")
	}
}
