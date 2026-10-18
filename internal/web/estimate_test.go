package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// An item waiting on its estimate shows it, needs you, and can be
// approved only behind the password or from this machine.
func TestAPIEstimate(t *testing.T) {
	ctx := context.Background()
	base, st, fa, s := apiSetup(t)
	// Item 2 was built: its estimate is compared with what it cost ($1.25).
	size := "S"
	st.UpdateItem(ctx, 2, store.ItemUpdate{Complexity: &size})
	st.SetEstimate(ctx, store.Estimate{ItemID: 2, Size: "S", MedianUSD: 1, P75USD: 2, TotalMedianUSD: 1, TotalP75USD: 2, Basis: "repo", Samples: 6, Detail: "based on 6 past S items in o/r", Status: store.EstimateUnder})
	it, _, _ := st.UpsertItem(ctx, "o/r", 3, "Rewrite the scheduler")
	st.SetStage(ctx, it.ID, state.Ready, "daemon", "triaged")
	st.SetStage(ctx, it.ID, state.Blocked, "daemon", "Waiting for your OK on the cost")
	st.SetEstimate(ctx, store.Estimate{ItemID: it.ID, Size: "L", MedianUSD: 8, P75USD: 14, TotalMedianUSD: 8, TotalP75USD: 14, Basis: "default", Detail: "from your budgets", ThresholdUSD: 5, Status: store.EstimateWaiting})

	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/3", &d)
	if e := d.Estimate; e == nil || e.Range != "$8.00–$14.00" || e.Status != "waiting" || e.ThresholdUSD != 5 || e.Finished || e.CanApprove {
		t.Fatalf("estimate = %+v", d.Estimate)
	}
	if !d.Item.EstimateWaiting || !d.Item.NeedsYou || d.Item.Estimate != "$8.00–$14.00" {
		t.Fatalf("item = %+v", d.Item)
	}
	getJSON(t, base+"/api/v1/items/2", &d)
	if e := d.Estimate; e == nil || e.ActualUSD != 1.25 || !e.Finished || d.Item.EstimateWaiting {
		t.Fatalf("built item's estimate = %+v", d.Estimate)
	}
	// Only the jobs after the triage the estimate came from count.
	jobs, _ := st.ListJobs(ctx, store.JobFilter{ItemID: 2})
	var last int64
	for _, j := range jobs {
		last = max(last, j.ID)
	}
	st.SetEstimate(ctx, store.Estimate{ItemID: 2, TriageJob: last, Size: "S", MedianUSD: 1, P75USD: 2, TotalMedianUSD: 1, TotalP75USD: 2, Basis: "repo", Samples: 6, Detail: "d", Status: store.EstimateUnder})
	d = APIItemDetail{}
	getJSON(t, base+"/api/v1/items/2", &d)
	if e := d.Estimate; e == nil || e.ActualUSD != 0 {
		t.Fatalf("an earlier cycle counted: %+v", d.Estimate)
	}
	getJSON(t, base+"/api/v1/items/1", &d)
	if d.Estimate != nil {
		t.Fatalf("no estimate yet: %+v", d.Estimate)
	}

	var sp APISpend
	getJSON(t, base+"/api/v1/spend", &sp)
	if e := sp.Estimates; e.Compared != 1 || e.Within != 1 || e.MedianMissUSD != 0.25 {
		t.Fatalf("estimates = %+v", e)
	}
	var n APINotifications
	getJSON(t, base+"/api/v1/notifications", &n)
	found := false
	for _, x := range n.Notifications {
		found = found || strings.Contains(x.Title, "waits for your OK on its cost") && strings.Contains(x.Detail, "over the $5.00 limit")
	}
	if !found {
		t.Fatalf("notifications = %+v", n)
	}

	// Without a password, only from this machine.
	if code, body := postJSON(t, base+"/api/v1/items/3/approve-estimate", "", `{}`); code != http.StatusForbidden || !strings.Contains(body, "factory:go") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("calls = %v", fa.calls)
	}
	s.Listen = "127.0.0.1:8080"
	d = APIItemDetail{}
	if getJSON(t, base+"/api/v1/items/3", &d); d.Estimate == nil || !d.Estimate.CanApprove {
		t.Fatalf("estimate = %+v", d.Estimate)
	}
	if code, body := postJSON(t, base+"/api/v1/items/3/approve-estimate", "", `{}`); code != http.StatusOK || !strings.Contains(body, "Approved") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if len(fa.calls) != 1 || fa.calls[0] != "approve-estimate" {
		t.Fatalf("calls = %v", fa.calls)
	}
}
