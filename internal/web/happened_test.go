package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

// endJob runs one job of the item to its end.
func endJob(t *testing.T, st *store.Store, item int64, kind string, attempt int, end state.JobState, why string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.EnqueueJob(ctx, store.Job{ItemID: &item, Kind: kind, Harness: "claude", Model: "sonnet", BudgetUSD: 5, Attempt: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if end == state.Cancelled {
		if err := st.CancelQueued(ctx, id, "human", "cancelled from the control room"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	st.ClaimJob(ctx)
	if err := st.FinishJob(ctx, id, store.Result{State: end, Error: why}); err != nil {
		t.Fatal(err)
	}
	return id
}

func happenedServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	return &Server{Store: st, JobsDir: t.TempDir(), Chat: &fakeChatter{}}, st
}

func (s *Server) happenedFor(t *testing.T, id int64) (*APIHappened, *APISplit) {
	t.Helper()
	ctx := context.Background()
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.sortedJobs(ctx, id)
	evs, _ := s.Store.Events(ctx, id)
	h := s.whatHappened(it, jobs, evs)
	return h, s.splitOffer(ctx, it, jobs, h)
}

// A failed build and a cancelled retry read as both, not just the last.
func TestWhatHappenedTellsEachStop(t *testing.T) {
	s, st := happenedServer(t)
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 7, "Add SSO sign-in")
	endJob(t, st, it.ID, "triage", 1, state.Succeeded, "")
	ran := endJob(t, st, it.ID, "implement", 1, state.Failed, "Stopped: it ran out of turns (80 of 80) before it finished.")
	os.MkdirAll(filepath.Join(s.JobsDir, itoa(ran)), 0o755)
	os.WriteFile(filepath.Join(s.JobsDir, itoa(ran), "stop.json"), []byte(`{"reason":"turns","turns":80,"max_turns":80}`), 0o644)
	retry := endJob(t, st, it.ID, "implement", 2, state.Cancelled, "")
	st.SetStage(ctx, it.ID, state.Parked, "daemon", "Job was cancelled from the dashboard.")
	h, split := s.happenedFor(t, it.ID)
	want := "The build ran out of turns (job #" + itoa(ran) + "), then its retry was cancelled by you (job #" + itoa(retry) + ")."
	if h == nil || h.Summary != want {
		t.Fatalf("happened = %+v, want %q", h, want)
	}
	if len(h.Steps) != 2 || *h.Steps[0].JobID != ran || *h.Steps[1].JobID != retry || h.Steps[0].Text != "The build ran out of turns" {
		t.Fatalf("steps = %+v", h.Steps)
	}
	if split == nil || split.Unavailable != "" {
		t.Fatalf("split = %+v", split)
	}
	// The prompt names the item, its issue and the run to read.
	for _, want := range []string{"Split item #" + itoa(it.ID) + " (o/r#7: Add SSO sign-in)", "Its build stopped: " + want, "log (job #" + itoa(ran) + ")", "each with parent " + itoa(it.ID)} {
		if !strings.Contains(split.Prompt, want) {
			t.Errorf("prompt %q lacks %q", split.Prompt, want)
		}
	}
	s.Chat = &fakeChatter{refuse: "Today's orchestration budget is spent."}
	if _, split := s.happenedFor(t, it.ID); split == nil || split.Unavailable != "Today's orchestration budget is spent." {
		t.Fatalf("split = %+v", split)
	}
}

func TestWhatHappenedOtherStops(t *testing.T) {
	s, st := happenedServer(t)
	ctx := context.Background()
	// Parked on a question, with no job to blame.
	q, _, _ := st.UpsertItem(ctx, "o/r", 8, "Support anchors")
	st.SetStage(ctx, q.ID, state.Parked, "daemon", "Should anchors merge across files?\nThe issue does not say.")
	if h, split := s.happenedFor(t, q.ID); h == nil || h.Summary != "It was parked by the factory: Should anchors merge across files?" {
		t.Fatalf("happened = %+v", h)
	} else if split == nil || strings.Contains(split.Prompt, "job #") {
		t.Fatalf("split = %+v", split)
	}
	// A build that failed its gates and was then retried one tier up and timed out.
	f, _, _ := st.UpsertItem(ctx, "o/r", 9, "Rate-limit search")
	a := endJob(t, st, f.ID, "implement", 1, state.Failed, "gates failed: test\nlong output")
	b := endJob(t, st, f.ID, "implement", 2, state.TimedOut, "")
	h, split := s.happenedFor(t, f.ID)
	if h == nil || h.Summary != "The build failed: gates failed: test (job #"+itoa(a)+"), then its retry timed out (job #"+itoa(b)+")." {
		t.Fatalf("happened = %+v", h)
	}
	if split == nil || !strings.Contains(split.Prompt, "job #"+itoa(b)) {
		t.Fatalf("split = %+v", split)
	}
	// Running again, or done: nothing to tell.
	endJob(t, st, f.ID, "implement", 3, state.Succeeded, "")
	if h, split := s.happenedFor(t, f.ID); h != nil || split != nil {
		t.Fatalf("happened = %+v, split %+v", h, split)
	}
}
