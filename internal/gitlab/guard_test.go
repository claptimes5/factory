package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

func TestRevertWatchReads(t *testing.T) {
	project := `{"merge_trains_enabled":true,"merge_pipelines_enabled":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const p = "/projects/g%2Fp"
		q := r.URL.Query()
		switch {
		case r.URL.EscapedPath() == p+"/repository/commits" && q.Get("ref_name") == "main" && q.Get("since") == "2026-10-01T00:00:00Z":
			w.Write([]byte(`[{"id":"c2","message":"Revert \"x\"\n\nThis reverts merge request !5","committed_date":"2026-10-03T00:00:00Z"},{"id":"c1","message":"m","committed_date":"2026-10-02T00:00:00Z"}]`))
		case r.URL.EscapedPath() == p+"/merge_requests" && q.Get("state") == "merged" && q.Get("target_branch") == "main" && q.Get("updated_after") == "2026-10-01T00:00:00Z":
			w.Write([]byte(`[{"iid":9,"title":"Revert \"x\"","description":"This reverts merge request !5","source_branch":"revert-abc","squash_commit_sha":"s9","merged_at":"2026-10-03T00:00:00Z"},{"iid":8,"merged_at":"2026-09-03T00:00:00Z"}]`))
		case r.URL.EscapedPath() == p:
			w.Write([]byte(project))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Project: "g/p", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	ctx := context.Background()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cs, more, err := c.BranchCommits(ctx, "main", since, 300)
	if err != nil || more || len(cs) != 2 || cs[0].SHA != "c2" || cs[0].At.Day() != 3 {
		t.Fatalf("commits %+v %v %v", cs, more, err)
	}
	prs, err := c.MergedPRs(ctx, "main", since, 50)
	if err != nil || len(prs) != 1 || prs[0].Number != 9 || prs[0].MergeCommit != "s9" || prs[0].HeadRef != "revert-abc" {
		t.Fatalf("merged %+v %v", prs, err)
	}
	if trains, err := c.MergeQueue(ctx, "main"); !trains || err != nil {
		t.Fatalf("trains %v %v", trains, err)
	}
	// Trains on without merged results pipelines: GitLab runs no train,
	// and pipelines run on the branch, so the factory syncs it itself.
	project = `{"merge_trains_enabled":true,"merge_pipelines_enabled":false}`
	if trains, err := c.MergeQueue(ctx, "main"); trains || err != nil {
		t.Fatalf("trains without merged results %v %v", trains, err)
	}
	// Merged results pipelines without trains: each tests the request
	// merged with its target, and none runs on the branch's own commit.
	project = `{"merge_trains_enabled":false,"merge_pipelines_enabled":true}`
	if tested, err := c.MergeQueue(ctx, "main"); !tested || err != nil {
		t.Fatalf("merged results %v %v", tested, err)
	}
}

func TestSquashAndMergeCommitsBothKept(t *testing.T) {
	pr := rawMR{IID: 3, State: "merged", Merge: "mc", Squash: "sq"}.pr()
	if pr.MergeCommit != "mc" || pr.SquashCommit != "sq" {
		t.Fatalf("%+v", pr)
	}
	pr = rawMR{IID: 3, State: "merged", Squash: "sq"}.pr()
	if pr.MergeCommit != "sq" || pr.SquashCommit != "" {
		t.Fatalf("fast-forward squash %+v", pr)
	}
}

func TestFirstParent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/projects/g%2Fp/repository/commits/m1" {
			w.Write([]byte(`{"id":"m1","parent_ids":["p1","p2"]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := &Client{Project: "g/p", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	if p, err := c.FirstParent(context.Background(), "m1"); p != "p1" || err != nil {
		t.Fatalf("%q %v", p, err)
	}
}
