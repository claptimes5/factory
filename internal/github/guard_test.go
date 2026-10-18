package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBranchCommitsPagesWithCare(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/commits" || r.URL.Query().Get("sha") != "main" || r.URL.Query().Get("since") != "2026-10-01T00:00:00Z" {
			w.WriteHeader(404)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		var out []map[string]any
		n := 100
		if page == "3" {
			n = 5
		}
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"sha": fmt.Sprintf("p%s-%d", page, i), "commit": map[string]any{"message": "m", "committer": map[string]any{"date": "2026-10-02T00:00:00Z"}}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got, more, err := c.BranchCommits(context.Background(), "main", since, 150)
	if err != nil || len(got) != 150 || !more || strings.Join(pages, ",") != "1,2" || got[0].SHA != "p1-0" || got[0].At.Day() != 2 {
		t.Fatalf("%d commits, more %v, pages %v, %v", len(got), more, pages, err)
	}
	pages = nil
	got, more, err = c.BranchCommits(context.Background(), "main", since, 300)
	if err != nil || len(got) != 205 || more || strings.Join(pages, ",") != "1,2,3" {
		t.Fatalf("%d commits, more %v, pages %v, %v", len(got), more, pages, err)
	}
}

func TestMergedPRsStopsAtOlderUpdates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/o/r/pulls" || q.Get("state") != "closed" || q.Get("base") != "main" || q.Get("sort") != "updated" || q.Get("direction") != "desc" {
			w.WriteHeader(404)
			return
		}
		calls++
		w.Write([]byte(`[
 {"number": 9, "title": "Revert \"x\"", "body": "Reverts o/r#5", "merged_at": "2026-10-05T00:00:00Z", "updated_at": "2026-10-05T00:00:00Z", "merge_commit_sha": "abc", "head": {"ref": "revert-5-x"}},
 {"number": 8, "title": "closed unmerged", "merged_at": null, "updated_at": "2026-10-04T00:00:00Z"},
 {"number": 7, "title": "merged before", "merged_at": "2026-09-01T00:00:00Z", "updated_at": "2026-10-03T00:00:00Z"},
 {"number": 6, "title": "too old", "merged_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-01T00:00:00Z"}
]`))
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	got, err := c.MergedPRs(context.Background(), "main", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 50)
	if err != nil || len(got) != 1 || got[0].Number != 9 || got[0].HeadRef != "revert-5-x" || got[0].Body != "Reverts o/r#5" || calls != 1 {
		t.Fatalf("%+v %v (%d calls)", got, err, calls)
	}
}

func TestMergeQueueAndQueueMerge(t *testing.T) {
	var calls []string
	queue := true
	checks := `[{"name":"test","status":"completed","conclusion":"success"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/repos/o/r/commits/abc/check-runs":
			w.Write([]byte(`{"check_runs":` + checks + `}`))
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/7":
			w.Write([]byte(`{"node_id":"PR_x","draft":false}`))
		case r.URL.Path == "/graphql":
			var q struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			json.Unmarshal(b, &q)
			switch {
			case strings.Contains(q.Query, "mergeQueue(branch"):
				calls = append(calls, "ask "+q.Variables["branch"].(string))
				if queue {
					w.Write([]byte(`{"data":{"repository":{"mergeQueue":{"id":"MQ_1"}}}}`))
				} else {
					w.Write([]byte(`{"data":{"repository":{"mergeQueue":null}}}`))
				}
			case strings.Contains(q.Query, "enablePullRequestAutoMerge"):
				if strings.Contains(q.Query, "mergeMethod") {
					t.Error("a merge queue sets its own method")
				}
				calls = append(calls, "auto "+q.Variables["sha"].(string))
				w.Write([]byte(`{"errors":[{"message":"Pull request is in clean status"}]}`))
			case strings.Contains(q.Query, "enqueuePullRequest"):
				calls = append(calls, "enqueue "+q.Variables["sha"].(string))
				w.Write([]byte(`{"data":{}}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	ctx := context.Background()
	if q, err := c.MergeQueue(ctx, "main"); !q || err != nil {
		t.Fatalf("queue %v %v", q, err)
	}
	queue = false
	if q, err := c.MergeQueue(ctx, "main"); q || err != nil {
		t.Fatalf("no queue %v %v", q, err)
	}
	// Nothing to wait for: it joins the queue at once, pinned to the commit.
	if err := c.QueueMerge(ctx, 7, "abc", "squash"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, "|"); got != "ask main|ask main|auto abc|enqueue abc" {
		t.Fatalf("calls %s", got)
	}
	// Nothing to wait for, but its checks haven't all passed: it stays
	// out of the queue, as AutoMerge doesn't merge it.
	calls, checks = nil, `[{"name":"test","status":"completed","conclusion":"failure"}]`
	if err := c.QueueMerge(ctx, 7, "abc", "squash"); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("queued with a failed check: %v", err)
	}
	if got := strings.Join(calls, "|"); got != "auto abc" {
		t.Fatalf("calls %s", got)
	}
}

func TestFirstParent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/m1":
			w.Write([]byte(`{"sha":"m1","parents":[{"sha":"p1"},{"sha":"p2"}]}`))
		case "/repos/o/r/commits/root":
			w.Write([]byte(`{"sha":"root","parents":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if p, err := c.FirstParent(context.Background(), "m1"); p != "p1" || err != nil {
		t.Fatalf("%q %v", p, err)
	}
	if p, err := c.FirstParent(context.Background(), "root"); p != "" || err != nil {
		t.Fatalf("root %q %v", p, err)
	}
}
