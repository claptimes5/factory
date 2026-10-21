package web

import (
	"context"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/state"
)

// Home's catch-up shows what went live since it was last marked seen,
// and nothing before the first time.
func TestHomeCatchUp(t *testing.T) {
	t.Parallel()
	srv, st, _, _ := setup(t)
	ctx := context.Background()
	var h APIHome
	getJSON(t, srv.URL+"/api/v1/home", &h)
	if h.SeenAt != "" || len(h.WentLive) != 0 {
		t.Fatalf("before the first look: %+v", h)
	}
	if code, body := postJSON(t, srv.URL+"/api/v1/home/seen", "", "{}"); code != 200 {
		t.Fatalf("seen: %d %s", code, body)
	}
	time.Sleep(2 * time.Millisecond)
	it, _, _ := st.UpsertItem(ctx, "o/r", 2, "Fix flaky tests")
	for _, s := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done} {
		if err := st.SetStage(ctx, it.ID, s, "daemon", ""); err != nil {
			t.Fatal(s, err)
		}
	}
	getJSON(t, srv.URL+"/api/v1/home", &h)
	if h.SeenAt == "" || len(h.WentLive) != 1 || h.WentLive[0].Title != "Fix flaky tests" {
		t.Fatalf("after one went live: %+v", h)
	}
	postJSON(t, srv.URL+"/api/v1/home/seen", "", "{}")
	getJSON(t, srv.URL+"/api/v1/home", &h)
	if len(h.WentLive) != 0 {
		t.Fatalf("after marking seen: %+v", h.WentLive)
	}
}
