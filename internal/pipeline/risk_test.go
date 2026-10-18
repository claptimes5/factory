package pipeline

import (
	"context"
	"testing"

	"github.com/claptimes5/factory/internal/store"
)

// The PR names the feature that raised the risk only when it did so in
// this run: after a retry and a new triage, an old run's feature isn't
// why. Events saved before they carried data still name it.
func TestRiskWhy(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 5, "x")
	job := func(kind string) int64 {
		id, err := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: kind, Harness: "claude", Model: "m", Attempt: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	raise := func(jobID int64, data, reason string) {
		if err := e.p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "risk_raised", Reason: reason, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	it.Risk = "high"
	job("triage")
	impl := job("implement")
	raise(impl, `{"feature":"Billing","from":"low","to":"high"}`, "low -> high: touches Billing")
	if got, want := e.p.riskWhy(ctx, it), "it touches Billing, a high-risk feature in the feature map"; got != want {
		t.Fatalf("riskWhy = %q, want %q", got, want)
	}
	// Retried: triage rates it high itself and the change misses Billing.
	job("triage")
	job("implement")
	if got := e.p.riskWhy(ctx, it); got != "triage rated it high" {
		t.Fatalf("after a new triage, riskWhy = %q", got)
	}
	// An event from before events carried data.
	raise(job("implement"), "", "low -> high: touches Auth")
	if got, want := e.p.riskWhy(ctx, it), "it touches Auth, a high-risk feature in the feature map"; got != want {
		t.Fatalf("legacy event: riskWhy = %q, want %q", got, want)
	}
}
