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
						{"action": "add", "label": map[string]any{"name": "factory:ready"}, "user": map[string]any{"id": u[0], "username": u[1]}},
						{"action": "remove", "label": map[string]any{"name": "factory:ready"}, "user": map[string]any{"id": 9, "username": "x"}},
					})
					return
				}
			}
			w.Write([]byte(`[]`))
		case levels[path] != 0:
			json.NewEncoder(w).Encode(map[string]any{"access_level": levels[path]})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Project: "team/web", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	for n, want := range map[int]struct {
		who string
		ok  bool
	}{1: {"dev", true}, 2: {"reporter", false}, 3: {"outsider", false}, 4: {"factory-bot", true}, 5: {"", false}} {
		who, ok, err := c.Approver(context.Background(), n, "factory:ready")
		if err != nil || who != want.who || ok != want.ok {
			t.Errorf("issue %d: %q %v %v, want %+v", n, who, ok, err, want)
		}
	}
}
