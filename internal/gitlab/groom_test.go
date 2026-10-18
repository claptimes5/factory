package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/claptimes5/factory/internal/github"
)

func TestOpenIssuesAndNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/team%2Fweb/issues":
			if q := r.URL.Query(); q.Get("state") != "opened" || q.Get("labels") != "" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode([]map[string]any{{"iid": 4, "title": "t", "description": "d", "state": "opened", "labels": []string{"bug"},
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-03-01T00:00:00Z", "author": map[string]any{"username": "bob"}, "merge_requests_count": 2}})
		case "/projects/team%2Fweb/issues/4/notes":
			json.NewEncoder(w).Encode([]map[string]any{{"body": "added label", "system": true}, {"body": "hello", "author": map[string]any{"username": "bot"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Project: "team/web", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	is, err := c.OpenIssues(context.Background(), 50)
	if err != nil || len(is) != 1 || is[0].Number != 4 || is[0].Body != "d" || is[0].Author != "bob" || is[0].LinkedPRs != 2 || is[0].UpdatedAt.Month() != 3 {
		t.Fatalf("%+v %v", is, err)
	}
	cs, err := c.IssueComments(context.Background(), 4)
	if err != nil || len(cs) != 1 || cs[0].Body != "hello" || cs[0].Author != "bot" {
		t.Fatalf("notes %q %v", cs, err)
	}
}
