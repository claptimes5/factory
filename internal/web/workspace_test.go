package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// The dashboard scopes to a workspace: the overview lists them with their
// budgets, the feeds keep a workspace's items and its orchestrator's
// runs, the Inbox says each one's workspace, and an unknown one is refused.
func TestScreensScopeToAWorkspace(t *testing.T) {
	t.Parallel()
	_, st, _, s := apiSetup(t)
	ctx := context.Background()
	s.Repos = []string{"o/r", "o/docs"}
	s.Workspaces = []Workspace{{Name: "app", Repos: []string{"o/r"}, DailyCap: 5}, {Name: "docs", Repos: []string{"o/docs"}}}
	doc, _, _ := st.UpsertItem(ctx, "o/docs", 3, "Docs change")
	st.SetStage(ctx, doc.ID, state.Parked, "daemon", "needs a human")
	st.AddEvent(ctx, store.Event{Actor: "orchestrator", ItemID: &doc.ID, Type: "set_priority", Reason: "docs first"})
	run, _ := st.EnqueueJob(ctx, store.Job{Kind: "orchestrate", Harness: "claude", Model: "opus", BudgetUSD: 1, Input: `{"workspace":"docs","reasons":["scheduled run"]}`})
	st.AddEvent(ctx, store.Event{Actor: "orchestrator", JobID: &run, Type: "run_summary", Reason: "looked at the docs"})
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	base := httpServer(t, h)

	var o APIOverview
	getJSON(t, base+"/api/v1/overview", &o)
	if len(o.Workspaces) != 2 || o.Workspaces[0].Name != "app" || o.Workspaces[0].DailyCapUSD != 5 || o.Workspaces[0].SpentTodayUSD != 1.75 {
		t.Fatalf("workspaces = %+v", o.Workspaces)
	}
	var feed APIFeed
	getJSON(t, base+"/api/v1/orchestrator?workspace=app", &feed)
	for _, e := range feed.Events {
		if strings.Contains(e.Reason, "docs") {
			t.Fatalf("app's decisions show docs': %+v", feed.Events)
		}
	}
	getJSON(t, base+"/api/v1/orchestrator?workspace=docs", &feed)
	if len(feed.Events) != 2 {
		t.Fatalf("docs' decisions = %+v", feed.Events)
	}
	var act APIActivity
	getJSON(t, base+"/api/v1/activity?workspace=app", &act)
	for _, e := range act.Entries {
		if strings.Contains(e.Text, "docs") || e.ItemID != nil && *e.ItemID == doc.ID {
			t.Fatalf("app's activity shows docs': %+v", e)
		}
	}
	var ns APINotifications
	getJSON(t, base+"/api/v1/notifications", &ns)
	got := map[string]bool{}
	for _, n := range ns.Notifications {
		got[n.Workspace] = true
	}
	if !got["docs"] || !got[""] {
		t.Fatalf("notifications' workspaces = %v", got)
	}
	for _, u := range []string{"/api/v1/activity?workspace=nope", "/api/v1/orchestrator?workspace=nope", "/api/v1/chats?workspace=nope"} {
		if resp, err := http.Get(base + u); err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %v %v", u, resp.StatusCode, err)
		}
	}
}
