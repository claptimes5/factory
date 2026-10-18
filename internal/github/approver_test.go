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
	type ev = map[string]any
	labeled := func(at, who, typ string) ev {
		return ev{"__typename": "LabeledEvent", "createdAt": at, "label": ev{"name": "Factory:Ready"}, "actor": ev{"__typename": typ, "login": who}}
	}
	issues := map[int]ev{
		1: {"body": "b", "timelineItems": ev{"nodes": []ev{labeled("2026-01-01T00:00:00Z", "earlier", "User"), labeled("2026-01-02T00:00:00Z", "alice", "User")}}},
		2: {"timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "bob", "User")}}},
		3: {"timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "stranger", "User")}}},
		// The token's own user, and a user named like the app's bot.
		4: {"timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "me", "User")}}},
		5: {"timelineItems": ev{"nodes": []ev{}}},
		// Edited after the label: the body, then the title.
		6: {"lastEditedAt": "2026-01-03T00:00:00Z", "timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "alice", "User")}}},
		7: {"lastEditedAt": "2026-01-01T00:00:00Z", "timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "alice", "User"),
			{"__typename": "RenamedTitleEvent", "createdAt": "2026-01-04T00:00:00Z"}}}},
		// A bot whose login matches the factory's without the suffix.
		8: {"timelineItems": ev{"nodes": []ev{labeled("2026-01-02T00:00:00Z", "me", "Bot")}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/user":
			json.NewEncoder(w).Encode(ev{"login": "me"})
		case p == "/graphql":
			var q struct {
				Variables struct {
					N int `json:"n"`
				} `json:"variables"`
			}
			json.NewDecoder(r.Body).Decode(&q)
			is := issues[q.Variables.N]
			is["title"] = "t"
			json.NewEncoder(w).Encode(ev{"data": ev{"repository": ev{"issue": is}}})
		case perms[p] != "":
			json.NewEncoder(w).Encode(ev{"permission": perms[p]})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	for n, want := range map[int]struct {
		who        string
		ok, edited bool
	}{1: {"alice", true, false}, 2: {"bob", false, false}, 3: {"stranger", false, false}, 4: {"me", true, false}, 5: {"", false, false},
		6: {"alice", true, true}, 7: {"alice", true, true}, 8: {"me[bot]", false, false}} {
		a, err := c.Approver(context.Background(), n, "factory:ready")
		if err != nil || a.By != want.who || a.OK != want.ok || a.Edited != want.edited || a.Issue.Title != "t" {
			t.Errorf("issue %d: %+v %v, want %+v", n, a, err, want)
		}
	}
	if a, _ := c.Approver(context.Background(), 1, "factory:ready"); a.Issue.Body != "b" || a.Issue.Number != 1 {
		t.Errorf("issue text = %+v", a.Issue)
	}
}
