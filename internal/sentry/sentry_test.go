package sentry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			w.Write([]byte(`{"detail":"Invalid token"}`))
			return
		}
		switch r.URL.Path {
		case "/api/0/projects/acme/api/issues/":
			if q := r.URL.Query(); q.Get("query") != "is:unresolved" || q.Get("sort") != "freq" {
				t.Errorf("query %v", q)
			}
			w.Write([]byte(`[{"id":"7","shortId":"API-7","title":"ValueError: bad","culprit":"app.views in index","permalink":"https://acme.sentry.io/issues/7/","status":"unresolved","count":"120","userCount":4,"firstSeen":"2026-10-01T00:00:00Z","lastSeen":"2026-10-18T00:00:00Z"}]`))
		case "/api/0/organizations/acme/issues/7/":
			w.Write([]byte(`{"id":"7","status":"resolved"}`))
		case "/api/0/organizations/acme/issues/7/events/latest/":
			w.Write([]byte(`{"eventID":"e1","entries":[{"type":"breadcrumbs","data":{}},{"type":"exception","data":{"values":[{"type":"ValueError","value":"bad","stacktrace":{"frames":[
				{"filename":"lib/x.py","function":"f","lineNo":3,"inApp":false},{"filename":"app/views.py","function":"index","lineNo":12,"inApp":true}]}}]}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{BaseURL: srv.URL + "/", Token: "tok"}
	is, err := c.Issues(ctx, "acme", "api", "freq")
	if err != nil || len(is) != 1 || is[0].Events() != 120 || is[0].UserCount != 4 || is[0].ShortID != "API-7" {
		t.Fatalf("issues %+v %v", is, err)
	}
	one, err := c.Issue(ctx, "acme", "7")
	if err != nil || one.Status != "resolved" {
		t.Fatalf("issue %+v %v", one, err)
	}
	ev, err := c.LatestEvent(ctx, "acme", "7")
	if err != nil || len(ev.Exceptions) != 1 || len(ev.Exceptions[0].Frames) != 2 || !ev.Exceptions[0].Frames[1].InApp {
		t.Fatalf("event %+v %v", ev, err)
	}
	_, err = (&Client{BaseURL: srv.URL, Token: "wrong"}).Issues(ctx, "acme", "api", "freq")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad token: %v", err)
	}
}
