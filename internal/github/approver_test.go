package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApprover(t *testing.T) {
	perms := map[string]string{"/repos/o/r/collaborators/alice/permission": "write", "/repos/o/r/collaborators/bob/permission": "read"}
	labeler := map[string]string{"/repos/o/r/issues/1/events": "alice", "/repos/o/r/issues/2/events": "bob",
		"/repos/o/r/issues/3/events": "stranger", "/repos/o/r/issues/4/events": "me"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "me"})
		case labeler[p] != "":
			// An older label from someone else, then the intake label, then another label.
			json.NewEncoder(w).Encode([]map[string]any{
				{"event": "labeled", "label": map[string]any{"name": "factory:ready"}, "actor": map[string]any{"login": "earlier"}},
				{"event": "labeled", "label": map[string]any{"name": "Factory:Ready"}, "actor": map[string]any{"login": labeler[p]}},
				{"event": "labeled", "label": map[string]any{"name": "bug"}, "actor": map[string]any{"login": "alice"}},
			})
		case perms[p] != "":
			json.NewEncoder(w).Encode(map[string]any{"permission": perms[p]})
		case p == "/repos/o/r/issues/5/events":
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	for n, want := range map[int]struct {
		who string
		ok  bool
	}{1: {"alice", true}, 2: {"bob", false}, 3: {"stranger", false}, 4: {"me", true}, 5: {"", false}} {
		who, ok, err := c.Approver(context.Background(), n, "factory:ready")
		if err != nil || who != want.who || ok != want.ok {
			t.Errorf("issue %d: %q %v %v, want %+v", n, who, ok, err, want)
		}
	}
}
