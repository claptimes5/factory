package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A tripped breaker shows on the overview, and resets only behind the
// password or from this machine.
func TestAPIAutoMergeBreaker(t *testing.T) {
	base, _, fa, s := apiSetup(t)
	s.Breakers = func(context.Context) []APIBreaker {
		return []APIBreaker{
			{Repo: "o/r", On: true, Tripped: true, Reason: "2 factory PRs in 7 days", Strikes: 2, Threshold: 2, WindowHours: 168},
			{Repo: "o/quiet"}, // auto-merge off and never tripped: not shown
		}
	}
	var o APIOverview
	getJSON(t, base+"/api/v1/overview", &o)
	if len(o.AutoMerge) != 1 || !o.AutoMerge[0].Tripped || o.AutoMerge[0].CanReset {
		t.Fatalf("overview = %+v", o.AutoMerge)
	}
	if code, body := postJSON(t, base+"/api/v1/auto-merge/reset", "", `{"repo":"o/r"}`); code != http.StatusForbidden || !strings.Contains(body, "FACTORY_WEB_PASSWORD") {
		t.Fatalf("reset = %d %s", code, body)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("calls = %v", fa.calls)
	}
	s.Listen = "127.0.0.1:8080"
	if getJSON(t, base+"/api/v1/overview", &o); !o.AutoMerge[0].CanReset {
		t.Fatalf("overview = %+v", o.AutoMerge)
	}
	if code, _ := postJSON(t, base+"/api/v1/auto-merge/reset", "", `{}`); code != http.StatusBadRequest {
		t.Fatalf("reset without a repo = %d", code)
	}
	if code, body := postJSON(t, base+"/api/v1/auto-merge/reset", "", `{"repo":"o/r"}`); code != http.StatusOK || !strings.Contains(body, "back on") {
		t.Fatalf("reset = %d %s", code, body)
	}
	if len(fa.calls) != 1 || fa.calls[0] != "reset-auto-merge:o/r:the dashboard" {
		t.Fatalf("calls = %v", fa.calls)
	}
}
