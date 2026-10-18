package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/github"
)

// fake is a GitLab API for one project, team/sub/web.
type fake struct {
	calls []string
	puts  []map[string]any
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	const p = "/projects/team%2Fsub%2Fweb"
	path := r.URL.EscapedPath()
	j := func(v any) { json.NewEncoder(w).Encode(v) }
	var in map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	switch {
	case r.Method == "GET" && path == p+"/issues":
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("labels") != "factory:ready" {
			j([]any{})
			return
		}
		j([]map[string]any{{"iid": 3, "title": "Fix login", "description": "It breaks", "state": "opened", "labels": []string{"factory:ready"}}})
	case r.Method == "GET" && path == p+"/issues/9":
		j(map[string]any{"iid": 9, "title": "Old", "state": "closed"})
	case r.Method == "PUT" && (path == p+"/issues/3" || path == p+"/merge_requests/5"):
		f.puts = append(f.puts, in)
		j(map[string]any{})
	case r.Method == "POST" && path == p+"/issues/3/notes":
		j(map[string]any{})
	case r.Method == "POST" && path == p+"/issues":
		j(map[string]any{"iid": 12})
	case r.Method == "GET" && path == p+"/merge_requests":
		if r.URL.Query().Get("source_branch") != "factory/issue-3" {
			j([]any{})
			return
		}
		j([]map[string]any{
			{"iid": 4, "state": "opened", "source_project_id": 99, "target_project_id": 1}, // from a fork
			{"iid": 5, "web_url": "https://gitlab.example/team/sub/web/-/merge_requests/5", "state": "opened", "draft": true, "sha": "abc", "source_project_id": 1, "target_project_id": 1},
		})
	case r.Method == "GET" && path == p+"/merge_requests/5":
		j(map[string]any{"iid": 5, "state": "merged", "sha": "def", "title": "Fix login", "description": "Closes #3",
			"merge_commit_sha": nil, "squash_commit_sha": "sq1", "diff_refs": map[string]any{"base_sha": "b1"}})
	case r.Method == "POST" && path == p+"/merge_requests":
		if in["title"] != "Draft: Fix login (#3)" || in["source_branch"] != "factory/issue-3" || in["target_branch"] != "main" {
			w.WriteHeader(400)
			return
		}
		j(map[string]any{"iid": 6, "state": "opened", "draft": true})
	case r.Method == "GET" && path == p:
		j(map[string]any{"default_branch": "trunk"})
	case r.Method == "GET" && path == p+"/repository/branches/main":
		j(map[string]any{"commit": map[string]any{"id": "c0ffee"}})
	case r.Method == "GET" && path == p+"/repository/tree":
		if r.URL.Query().Get("ref") != "trunk" {
			w.WriteHeader(400)
			return
		}
		j([]map[string]any{{"name": "src", "type": "tree"}, {"name": "package.json", "type": "blob"}})
	case r.Method == "GET" && path == p+"/repository/files/src%2Fapp.ts":
		j(map[string]any{"content": base64.StdEncoding.EncodeToString([]byte("export {}")), "encoding": "base64"})
	case r.Method == "GET" && path == p+"/repository/commits/abc/statuses":
		j([]map[string]any{{"name": "test", "status": "failed", "target_url": "https://ci/1"}, {"name": "lint", "status": "running"}, {"name": "audit", "status": "failed", "allow_failure": true}})
	case r.Method == "GET" && strings.HasPrefix(path, p+"/labels/"):
		w.WriteHeader(404)
	case r.Method == "POST" && path == p+"/labels":
		f.puts = append(f.puts, in)
		j(map[string]any{})
	default:
		w.WriteHeader(404)
	}
}

func TestClient(t *testing.T) {
	f := &fake{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{Project: "team/sub/web", Host: "gitlab.example", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	ctx := context.Background()

	is, err := c.ListIssues(ctx, "factory:ready")
	if err != nil || len(is) != 1 || is[0].Number != 3 || is[0].Body != "It breaks" || is[0].State != "open" || is[0].Labels[0] != "factory:ready" {
		t.Fatalf("issues %+v %v", is, err)
	}
	if old, err := c.GetIssue(ctx, 9); err != nil || old.State != "closed" || old.StateReason != "" {
		t.Fatalf("closed %+v %v", old, err)
	}
	if err := c.AddLabels(ctx, 3, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLabel(ctx, 3, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.LabelPR(ctx, 5, "factory:review"); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(f.puts); string(got) != `[{"add_labels":"a,b"},{"remove_labels":"a"},{"add_labels":"factory:review"}]` {
		t.Fatalf("puts %s", got)
	}
	if err := c.Comment(ctx, 3, "hi"); err != nil {
		t.Fatal(err)
	}
	if n, err := c.CreateIssue(ctx, "T", "B", []string{"x"}); err != nil || n != 12 {
		t.Fatalf("create %d %v", n, err)
	}

	pr, err := c.FindPR(ctx, "factory/issue-3")
	if err != nil || pr == nil || pr.Number != 5 || !pr.Draft || pr.State != "open" || pr.Head.SHA != "abc" || !strings.HasSuffix(pr.URL, "/merge_requests/5") {
		t.Fatalf("find %+v %v", pr, err)
	}
	if none, err := c.FindPR(ctx, "other"); err != nil || none != nil {
		t.Fatalf("find other %+v %v", none, err)
	}
	// A squash merge's commit stands in for the merge commit.
	if pr, err = c.GetPR(ctx, 5); err != nil || !pr.Merged || pr.State != "closed" || pr.MergeCommit != "sq1" || pr.Base.SHA != "b1" || pr.Body != "Closes #3" || pr.Title != "Fix login" {
		t.Fatalf("get %+v %v", pr, err)
	}
	if pr, err = c.CreatePR(ctx, "Fix login (#3)", "body", "factory/issue-3", "main", true); err != nil || pr.Number != 6 {
		t.Fatalf("create pr %+v %v", pr, err)
	}

	if sha, err := c.BranchSHA(ctx, "main"); err != nil || sha != "c0ffee" {
		t.Fatalf("branch %s %v", sha, err)
	}
	if sha, err := c.BranchSHA(ctx, "gone"); err != nil || sha != "" {
		t.Fatalf("missing branch %s %v", sha, err)
	}
	if names, err := c.ListDir(ctx, ""); err != nil || strings.Join(names, ",") != "src/,package.json" {
		t.Fatalf("list %v %v", names, err)
	}
	if body, err := c.ReadFile(ctx, "src/app.ts"); err != nil || body != "export {}" {
		t.Fatalf("read %q %v", body, err)
	}
	if _, err := c.ReadFile(ctx, "nope"); !github.IsNotFound(err) {
		t.Fatalf("want a 404, got %v", err)
	}
	checks, err := c.CheckRuns(ctx, "abc")
	if err != nil || len(checks) != 3 || checks[0].Conclusion != "failure" || checks[1].Status != "in_progress" || checks[2].Conclusion != "neutral" {
		t.Fatalf("checks %+v %v", checks, err)
	}
	if created, err := c.EnsureLabel(ctx, "factory:ready", "0e8a16", "go"); err != nil || !created || f.puts[len(f.puts)-1]["color"] != "#0e8a16" {
		t.Fatalf("label %v %v %v", created, err, f.puts)
	}

	bad := &Client{Project: "team/sub/web", BaseURL: srv.URL, Tokens: github.StaticToken("wrong")}
	if _, err := bad.DefaultBranch(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401, got %v", err)
	}
}

func TestUndraft(t *testing.T) {
	for in, want := range map[string]string{"Draft: Fix it (#3)": "Fix it (#3)", "[WIP] x": "x", "Fix drafts": "Fix drafts"} {
		if got, _ := undraft(in); got != want {
			t.Errorf("undraft(%q) = %q", in, got)
		}
	}
}

func TestAutoMerge(t *testing.T) {
	for _, c := range []struct {
		pipeline string
		want     string
		err      bool
	}{
		{`{"sha":"abc","status":"running"}`, `PUT title=Fix it|PUT merge {"auto_merge":true,"merge_when_pipeline_succeeds":true,"sha":"abc","squash":true}`, false},
		{`null`, ``, true},
		{`{"sha":"old","status":"running"}`, ``, true},
		// Finished red: GitLab would merge it at once.
		{`{"sha":"abc","status":"failed"}`, ``, true},
		{`{"sha":"abc","status":"skipped"}`, ``, true},
	} {
		var calls []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			switch p := r.URL.EscapedPath(); {
			case r.Method == "GET" && strings.HasSuffix(p, "/merge_requests/4"):
				w.Write([]byte(`{"iid":4,"title":"Draft: Fix it","head_pipeline":` + c.pipeline + `}`))
			case r.Method == "PUT" && strings.HasSuffix(p, "/merge_requests/4"):
				var in map[string]string
				json.Unmarshal(b, &in)
				calls = append(calls, "PUT title="+in["title"])
				w.Write([]byte(`{}`))
			case r.Method == "PUT" && strings.HasSuffix(p, "/merge_requests/4/merge"):
				calls = append(calls, "PUT merge "+strings.TrimSpace(string(b)))
				w.Write([]byte(`{}`))
			default:
				w.WriteHeader(404)
			}
		}))
		cl := &Client{Project: "team/web", Host: "gitlab.example", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
		err := cl.AutoMerge(context.Background(), 4, "abc", "squash")
		srv.Close()
		if got := strings.Join(calls, "|"); (err != nil) != c.err || got != c.want {
			t.Errorf("pipeline %s: %v\ngot  %s\nwant %s", c.pipeline, err, got, c.want)
		}
	}
}
