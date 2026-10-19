package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// An item whose plan waits shows the plan, needs you, says what to do,
// and can be approved or sent back only behind the password or from
// this machine.
func TestAPIPlanWaiting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, st, fa, s := apiSetup(t)
	it, _, _ := st.UpsertItem(ctx, "o/r", 3, "Rewrite the scheduler")
	tr, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "triage", Harness: "claude", Model: "haiku"})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, tr, store.Result{State: state.Succeeded})
	pj, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "plan", Harness: "claude", Model: "opus", Input: `{"round":1}`})
	st.ClaimJob(ctx)
	st.FinishJob(ctx, pj, store.Result{State: state.Succeeded,
		Verdict: `{"approach":"Split the scheduler in two.","files":["sched.go"],"risks":["timing"],"tests":"go test ./sched"}`})
	st.SetStage(ctx, it.ID, state.Ready, "daemon", "triaged")
	st.SetStage(ctx, it.ID, state.Blocked, "daemon", "Waiting for your OK on the plan.")
	st.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &pj, Type: store.EvPlanWaiting})

	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/3", &d)
	p := d.Plan
	if p == nil || p.JobID != pj || p.Approach != "Split the scheduler in two." || len(p.Files) != 1 || len(p.Risks) != 1 || p.Tests != "go test ./sched" ||
		!p.Waiting || p.CanApprove || p.ReplanOf != "" || p.ApprovedBy != "" {
		t.Fatalf("plan = %+v", d.Plan)
	}
	if !d.Item.PlanWaiting || !d.Item.NeedsYou || !strings.Contains(d.Item.BlockedNote, "build it or ask for another plan") {
		t.Fatalf("item = %+v", d.Item)
	}
	var b APIBoard
	getJSON(t, base+"/api/v1/board", &b)
	found := false
	for _, c := range b.Columns {
		for _, k := range c.Cards {
			found = found || k.ID == it.ID && k.PlanWaiting && k.NeedsYou
		}
	}
	if !found {
		t.Fatalf("board = %+v", b)
	}
	var n APINotifications
	getJSON(t, base+"/api/v1/notifications", &n)
	found = false
	for _, x := range n.Notifications {
		found = found || strings.Contains(x.Title, "waits for your OK on its plan") && strings.Contains(x.Detail, "build it")
	}
	if !found {
		t.Fatalf("notifications = %+v", n)
	}

	// Without a password, only from this machine; the refusal names the labels.
	if code, body := postJSON(t, base+"/api/v1/items/3/replan", "", `{"ask":"Keep one file"}`); code != http.StatusForbidden || !strings.Contains(body, "factory:replan") {
		t.Fatalf("replan = %d %s", code, body)
	}
	if code, body := postJSON(t, base+"/api/v1/items/3/approve-estimate", "", `{}`); code != http.StatusForbidden || !strings.Contains(body, "factory:go") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("calls = %v", fa.calls)
	}
	s.Listen = "127.0.0.1:8080"
	d = APIItemDetail{}
	if getJSON(t, base+"/api/v1/items/3", &d); d.Plan == nil || !d.Plan.CanApprove {
		t.Fatalf("plan = %+v", d.Plan)
	}
	if code, body := postJSON(t, base+"/api/v1/items/3/replan", "", `{"ask":"  "}`); code != http.StatusBadRequest || !strings.Contains(body, "what to change") {
		t.Fatalf("empty replan = %d %s", code, body)
	}
	if code, body := postJSON(t, base+"/api/v1/items/3/replan", "", `{"ask":"Keep one file"}`); code != http.StatusOK || !strings.Contains(body, "another plan") {
		t.Fatalf("replan = %d %s", code, body)
	}
	if code, body := postJSON(t, base+"/api/v1/items/3/approve-estimate", "", `{}`); code != http.StatusOK || !strings.Contains(body, "Approved") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if strings.Join(fa.calls, "|") != "replan:Keep one file|approve-estimate" {
		t.Fatalf("calls = %v", fa.calls)
	}

	// Once approved it no longer waits, and says how it was approved.
	st.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, JobID: &pj, Type: store.EvPlanApproved, Reason: "from the dashboard"})
	st.SetStage(ctx, it.ID, state.Ready, "human", "plan approved")
	d = APIItemDetail{}
	getJSON(t, base+"/api/v1/items/3", &d)
	if p := d.Plan; p == nil || p.Waiting || p.CanApprove || p.ApprovedBy != "from the dashboard" || d.Item.PlanWaiting {
		t.Fatalf("approved plan = %+v, item = %+v", d.Plan, d.Item)
	}
}
