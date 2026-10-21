package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/github"
)

// A token without leave to open merge requests is found out with empty
// requests, as GitLab answers them: a 403 for what it may not do, a 400
// or 404 for what it may.
func TestMissingAccess(t *testing.T) {
	t.Parallel()
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = append(posted, r.Method+" "+r.URL.EscapedPath())
		switch p := r.URL.EscapedPath(); {
		case r.Method == "POST" && strings.HasSuffix(p, "/merge_requests"):
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"insufficient_granular_scope","error_description":"Access denied: This operation requires [Merge Request: Create]."}`))
		case strings.HasSuffix(p, "/0"):
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"404 Not found"}`))
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"title is missing"}`))
		}
	}))
	defer srv.Close()
	cl := &Client{Project: "team/api", Host: "gitlab.example", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	got, err := cl.MissingAccess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "pr" || got[0].What != "open merge requests" || !strings.Contains(got[0].Says, "Merge Request: Create") {
		t.Fatalf("missing = %+v", got)
	}
	if len(posted) != 6 {
		t.Fatalf("asked %v", posted)
	}
}
