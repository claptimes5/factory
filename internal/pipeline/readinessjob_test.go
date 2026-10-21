package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A readiness check is a job: queued once per repo, run by the daemon,
// its steps and what it did kept in its folder, and a failure on the job.
func TestReadinessCheckIsAJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	fail := false
	e.p.Readiness = func(_ context.Context, repo string, out io.Writer) (string, error) {
		fmt.Fprintln(out, "running `bundle install`")
		fmt.Fprintln(out, "passed `bundle install` in 2m14s")
		if fail {
			return "", errors.New("cloning " + repo + ": no access")
		}
		return "Checked main at abc1234.", nil
	}
	if ok, err := e.p.QueueReadiness(ctx, "o/r", "main moved from 1a2b3c4 to 5d6e7f8"); !ok || err != nil {
		t.Fatalf("queued %v, %v", ok, err)
	}
	if ok, _ := e.p.QueueReadiness(ctx, "o/r", ""); ok {
		t.Fatal("a second check of the same repo was queued")
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: KindReadiness})
	if len(jobs) != 1 || jobs[0].State != state.Succeeded {
		t.Fatalf("jobs %+v", jobs)
	}
	dir := e.p.jobDir(jobs[0].ID)
	steps := ReadProgress(dir)
	if len(steps) != 2 || steps[1].Text != "passed `bundle install` in 2m14s" || steps[0].At == "" || ReadResult(dir) != "Checked main at abc1234." {
		t.Fatalf("steps %+v, result %q", steps, ReadResult(dir))
	}

	fail = true
	e.p.QueueReadiness(ctx, "o/r", "")
	e.p.Drain(ctx)
	jobs, _ = e.p.Store.ListJobs(ctx, store.JobFilter{Kind: KindReadiness})
	if jobs[0].State != state.Failed || !strings.Contains(jobs[0].Error, "no access") {
		t.Fatalf("failed check = %+v", jobs[0])
	}
}
