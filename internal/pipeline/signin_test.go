package pipeline

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// gatedHarness is a fake Codex on a ChatGPT plan.
type gatedHarness struct {
	*fakeHarness
	mu       sync.Mutex
	one      bool   // runs one job at a time
	why      string // why it can't sign in
	signOuts int    // runs to fail as signed out
}

func (g *gatedHarness) OneAtATime() bool { return g.one }
func (g *gatedHarness) Unavailable() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.why
}

func (g *gatedHarness) Run(ctx context.Context, req harness.RunRequest, ev chan<- harness.Event) (*harness.Result, error) {
	g.mu.Lock()
	if g.signOuts > 0 {
		g.signOuts--
		g.why = "Codex's ChatGPT sign-in has expired. Run `factory codex-login` again."
		g.mu.Unlock()
		return &harness.Result{IsError: true, SignedOut: true, Error: "codex: its ChatGPT sign-in was refused: refresh token was already used"}, nil
	}
	g.mu.Unlock()
	return g.fakeHarness.Run(ctx, req, ev)
}

func TestOneCodexJobAtATimeOnAChatGPTPlan(t *testing.T) {
	ctx := context.Background()
	for _, one := range []bool{true, false} { // the auth file; an API key or access token
		e := setup(t)
		e.p.Harnesses["codex"] = &gatedHarness{fakeHarness: e.codex, one: one}
		for range 2 {
			if _, err := e.p.Store.EnqueueJob(ctx, store.Job{Kind: "review", Harness: "codex", Model: "gpt-test"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.p.Store.EnqueueJob(ctx, store.Job{Kind: "triage", Harness: "claude", Model: "haiku"}); err != nil {
			t.Fatal(err)
		}
		first, err := e.p.claimNext(ctx)
		if err != nil || first == nil || first.Harness != "codex" {
			t.Fatalf("first claim %+v %v", first, err)
		}
		next, err := e.p.claimNext(ctx)
		if err != nil || next == nil {
			t.Fatalf("second claim %+v %v", next, err)
		}
		if want := map[bool]string{true: "claude", false: "codex"}[one]; next.Harness != want {
			t.Fatalf("one at a time %v: claimed %s next, want %s", one, next.Harness, want)
		}
	}
}

func TestASignedOutCodexJobWaitsWithoutUsingARetry(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	g := &gatedHarness{fakeHarness: e.codex, one: true, signOuts: 1}
	e.p.Harnesses["codex"] = g
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage == state.Parked || it.PRNumber != 0 {
		t.Fatalf("item = %+v", it)
	}
	reviews, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "review"})
	if len(reviews) != 2 || reviews[0].State != state.Queued || reviews[0].Attempt != 1 || reviews[1].State != state.Failed ||
		!strings.Contains(reviews[1].Error, "refresh token was already used") {
		t.Fatalf("reviews = %+v", reviews)
	}
	signIn := func(n string) bool { return strings.Contains(n, "factory codex-login") }
	if c := len(slices.DeleteFunc(slices.Clone(notes), func(n string) bool { return !signIn(n) })); c != 1 {
		t.Fatalf("sign-in notes %d: %q", c, notes)
	}
	// Drained again while still signed out: no second note, nothing runs.
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if c := len(slices.DeleteFunc(slices.Clone(notes), func(n string) bool { return !signIn(n) })); c != 1 {
		t.Fatalf("sign-in notes %d: %q", c, notes)
	}
	// Signed in again: the review runs and the PR opens.
	g.mu.Lock()
	g.why = ""
	g.mu.Unlock()
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.PRNumber == 0 {
		t.Fatalf("no PR after signing in: %s", it.Stage)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "codex can sign in again") }) {
		t.Fatalf("notes = %q", notes)
	}
}
