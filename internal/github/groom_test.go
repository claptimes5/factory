package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenIssues(t *testing.T) {
	type ev = map[string]any
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			var q struct {
				Variables struct {
					After *string `json:"after"`
				} `json:"variables"`
			}
			json.NewDecoder(r.Body).Decode(&q)
			pages++
			node := func(n int, linked int) ev {
				return ev{"number": n, "title": "t", "body": "b", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-02-01T00:00:00Z",
					"author": ev{"login": "alice"}, "labels": ev{"nodes": []ev{{"name": "bug"}}}, "closedByPullRequestsReferences": ev{"totalCount": linked}}
			}
			if q.Variables.After == nil {
				json.NewEncoder(w).Encode(ev{"data": ev{"repository": ev{"issues": ev{"pageInfo": ev{"hasNextPage": true, "endCursor": "c1"}, "nodes": []ev{node(3, 0)}}}}})
				return
			}
			json.NewEncoder(w).Encode(ev{"data": ev{"repository": ev{"issues": ev{"pageInfo": ev{"hasNextPage": false}, "nodes": []ev{node(2, 1), node(1, 0)}}}}})
		case "/repos/o/r/issues/3/comments":
			json.NewEncoder(w).Encode([]ev{{"body": "one", "user": ev{"login": "alice"}}, {"body": "two", "user": ev{"login": "app[bot]"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	is, err := c.OpenIssues(context.Background(), 2)
	if err != nil || len(is) != 2 || pages != 2 {
		t.Fatalf("%+v %v (%d pages)", is, err, pages)
	}
	if is[0].Number != 3 || is[0].Author != "alice" || is[0].Labels[0] != "bug" || is[0].UpdatedAt.Month() != 2 || is[1].LinkedPRs != 1 || is[1].State != "open" {
		t.Fatalf("issues %+v", is)
	}
	cs, err := c.IssueComments(context.Background(), 3)
	if err != nil || len(cs) != 2 || cs[1].Body != "two" || cs[1].Author != "app[bot]" {
		t.Fatalf("comments %q %v", cs, err)
	}
}

func TestIssueDetail(t *testing.T) {
	type ev = map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Variables struct {
				N int `json:"n"`
			} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		if q.Variables.N != 5 {
			json.NewEncoder(w).Encode(ev{"data": ev{"repository": ev{"issue": nil}}, "errors": []ev{{"message": "Could not resolve to an Issue"}}})
			return
		}
		json.NewEncoder(w).Encode(ev{"data": ev{"repository": ev{"issue": ev{"number": 5, "title": "t", "body": "b", "state": "CLOSED",
			"author": ev{"__typename": "Bot", "login": "app"}, "closedByPullRequestsReferences": ev{"totalCount": 2}}}}})
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	is, err := c.IssueDetail(context.Background(), 5)
	if err != nil || is.State != "closed" || is.Author != "app[bot]" || is.LinkedPRs != 2 {
		t.Fatalf("%+v %v", is, err)
	}
	if _, err := c.IssueDetail(context.Background(), 6); !IsNotFound(err) {
		t.Fatalf("missing issue: %v", err)
	}
}

// GitHub refuses "labels": null with a 422.
func TestCreateIssueWithoutLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if l, ok := in["labels"]; ok && l == nil {
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Invalid request.\n\nFor 'properties/labels', nil is not an array."}`))
			return
		}
		w.Write([]byte(`{"number":9}`))
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if n, err := c.CreateIssue(context.Background(), "t", "b", nil); err != nil || n != 9 {
		t.Fatalf("%d %v", n, err)
	}
}
