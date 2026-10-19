package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

// A harness on a plan shows its windows, five-hour first, or says when a
// reading will come; one billed by API key isn't listed.
func TestPlanUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	later := time.Now().Add(2 * time.Hour).UTC().Format(store.TimeFormat)
	st.SetLimit(ctx, store.Limit{Harness: "claude", Window: "weekly", UsedPercent: 18})
	st.SetLimit(ctx, store.Limit{Harness: "claude", Window: "weekly_opus", UsedPercent: 30})
	st.SetLimit(ctx, store.Limit{Harness: "claude", Window: "5h", UsedPercent: 42, ResetsAt: later})
	s := &Server{Store: st, Actions: &fakeActions{}, Repos: []string{"o/r"}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	base := httpServer(t, h)
	var got APIPlanUsage
	if getJSON(t, base+"/api/v1/usage", &got); len(got.Plans) != 0 {
		t.Fatalf("API keys only: %+v", got)
	}
	s.Plans, s.PauseAtUsage = []string{"claude", "codex"}, 95
	getJSON(t, base+"/api/v1/usage", &got)
	if len(got.Plans) != 2 {
		t.Fatalf("plans %+v", got.Plans)
	}
	c, x := got.Plans[0], got.Plans[1]
	if c.Name != "Claude" || c.NoReading != "" || len(c.Windows) != 3 || c.PauseAt != 95 {
		t.Fatalf("claude %+v", c)
	}
	if c.Windows[0].Window != "5h" || c.Windows[0].UsedPercent != 42 || c.Windows[0].ResetsAt != later || c.Windows[1].Window != "weekly" || c.Windows[2].Window != "weekly_opus" {
		t.Fatalf("windows %+v", c.Windows)
	}
	if x.Plan != "ChatGPT plan" || len(x.Windows) != 0 || x.NoReading == "" {
		t.Fatalf("codex %+v", x)
	}
}
