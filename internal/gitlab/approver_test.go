package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/github"
)

func TestApprover(t *testing.T) {
	const p = "/projects/team%2Fweb"
	levels := map[string]int{p + "/members/all/1": 30, p + "/members/all/2": 20}
	users := map[string][2]any{"/issues/1/": {1, "dev"}, "/issues/2/": {2, "reporter"}, "/issues/3/": {3, "outsider"}, "/issues/4/": {4, "factory-bot"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"username": "factory-bot"})
		case strings.HasSuffix(path, "/resource_label_events"):
			for k, u := range users {
				if strings.Contains(path, k) {
					json.NewEncoder(w).Encode([]map[string]any{
						{"action": "add", "created_at": "2026-01-02T00:00:00Z", "label": map[string]any{"name": "factory:ready"}, "user": map[string]any{"id": u[0], "username": u[1]}},
						{"action": "remove", "label": map[string]any{"name": "factory:ready"}, "user": map[string]any{"id": 9, "username": "x"}},
					})
					return
				}
			}
			w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/notes"):
			notes := []map[string]any{{"system": false, "body": "changed the description", "created_at": "2026-01-05T00:00:00Z"}, // a person's comment
				{"system": true, "body": "added ~1 label", "created_at": "2026-01-03T00:00:00Z"},
				{"system": true, "body": "changed the description", "created_at": "2026-01-01T00:00:00Z"}} // before the label
			if strings.Contains(path, "/issues/1/") {
				notes = append([]map[string]any{{"system": true, "body": "changed title from **a** to **b**", "created_at": "2026-01-04T00:00:00Z"}}, notes...)
			}
			json.NewEncoder(w).Encode(notes)
		case strings.HasSuffix(path, "/issues/1") || strings.HasSuffix(path, "/issues/2") || strings.HasSuffix(path, "/issues/3") || strings.HasSuffix(path, "/issues/4") || strings.HasSuffix(path, "/issues/5"):
			json.NewEncoder(w).Encode(map[string]any{"iid": 1, "title": "t", "description": "d"})
		case levels[path] != 0:
			json.NewEncoder(w).Encode(map[string]any{"access_level": levels[path]})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Project: "team/web", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	for n, want := range map[int]struct {
		who        string
		ok, edited bool
	}{1: {"dev", true, true}, 2: {"reporter", false, false}, 3: {"outsider", false, false}, 4: {"factory-bot", true, false}, 5: {"", false, false}} {
		a, err := c.Approver(context.Background(), n, "factory:ready")
		if err != nil || a.By != want.who || a.OK != want.ok || a.Edited != want.edited || a.Issue.Title != "t" {
			t.Errorf("issue %d: %+v %v, want %+v", n, a, err, want)
		}
	}
}
