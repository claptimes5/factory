package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListIssuesSkipsPRsAndSendsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("labels") != "factory:ready" {
			t.Errorf("unexpected %s", r.URL)
		}
		w.Write([]byte(`[{"number":1,"title":"a","labels":[{"name":"factory:ready"}]},{"number":2,"title":"pr","pull_request":{}}]`))
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	is, err := c.ListIssues(context.Background(), "factory:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 1 || is[0].Number != 1 || is[0].Labels[0] != "factory:ready" {
		t.Fatalf("issues = %+v", is)
	}
}

func TestRemoveMissingLabelIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if err := c.RemoveLabel(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if sha, err := c.BranchSHA(context.Background(), "nope"); err != nil || sha != "" {
		t.Fatalf("missing branch: %q %v", sha, err)
	}
}

func TestAppTokenLooksUpInstallation(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	installed := map[string]bool{"/app/installations/7/access_tokens": true}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/repos/o/r/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7})
		case installed[r.URL.Path]:
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := &AppTokens{AppID: "1", Key: key, Repo: "o/r", BaseURL: srv.URL}
	if tok, err := a.Token(context.Background()); err != nil || tok != "ghs_x" {
		t.Fatalf("%q %v", tok, err)
	}
	// Uninstalled since: the next try looks the installation up again.
	a.token, installed = "", nil
	if _, err := a.Token(context.Background()); err == nil {
		t.Fatal("want an error once uninstalled")
	}
	if a.InstallationID != "" {
		t.Fatalf("installation %s kept after a 404", a.InstallationID)
	}
	a2 := &AppTokens{AppID: "1", Key: key, Repo: "o/none", BaseURL: srv.URL}
	if _, err := a2.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed on o/none") {
		t.Fatalf("want not installed, got %v", err)
	}
}

func TestAppTokenScopedAndCached(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.URL.Path != "/app/installations/9/access_tokens" {
			t.Errorf("bad request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "r" {
			t.Errorf("token must be scoped to one repo, got %v", body.Repositories)
		}
		// Every permission asked for must be one docs/setup.md tells you to grant,
		// or GitHub refuses the token outright.
		want := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read", "checks": "read"}
		if !maps.Equal(body.Permissions, want) {
			t.Errorf("token permissions = %v, want %v", body.Permissions, want)
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	a := &AppTokens{AppID: "1", InstallationID: "9", Key: key, Repo: "o/r", BaseURL: srv.URL}
	for i := 0; i < 3; i++ {
		tok, err := a.Token(context.Background())
		if err != nil || tok != "ghs_x" {
			t.Fatalf("%q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Fatalf("token should be cached; minted %d times", calls)
	}
	jwt, _ := a.JWT(time.Now())
	if strings.Count(jwt, ".") != 2 {
		t.Fatalf("jwt = %s", jwt)
	}
}

func TestAppTokenWithoutChecksPermission(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var asked []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body.Permissions)
		if _, ok := body.Permissions["checks"]; ok {
			// What GitHub does when the app was never granted a permission.
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"The permissions requested are not granted to this installation."}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_y", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	a := &AppTokens{AppID: "1", InstallationID: "9", Key: key, Repo: "o/r", BaseURL: srv.URL}
	tok, err := a.Token(context.Background())
	if err != nil || tok != "ghs_y" {
		t.Fatalf("an app without Checks should still get a token: %q %v", tok, err)
	}
	if len(asked) != 2 || asked[1]["contents"] != "write" {
		t.Fatalf("requests = %v", asked)
	}
}

func TestCheckHistoryListsReruns(t *testing.T) {
	var closed map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/commits/abc/check-runs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("filter = %q, want all (reruns)", r.URL.Query().Get("filter"))
			}
			w.Write([]byte(`{"check_runs":[
				{"id":9,"name":"build","status":"completed","conclusion":"success","html_url":"https://ci/9","app":{"slug":"github-actions"},"check_suite":{"id":1},"output":{"title":"ok","summary":""}},
				{"id":5,"name":"build","status":"completed","conclusion":"failure","html_url":"https://ci/5","app":{"slug":"github-actions"},"check_suite":{"id":1},"output":{"title":"1 failed","summary":"TestX timed out"}},
				{"id":6,"name":"build","status":"completed","conclusion":"success","app":{"slug":"github-actions"},"check_suite":{"id":2}},
				{"id":7,"name":"build","status":"completed","conclusion":"success","app":{"slug":"buildkite"},"check_suite":{"id":3}}]}`))
		case r.URL.Path == "/repos/o/r/actions/runs":
			if r.URL.Query().Get("head_sha") != "abc" {
				t.Errorf("runs for %q", r.URL.Query().Get("head_sha"))
			}
			w.Write([]byte(`{"workflow_runs":[{"name":"CI","check_suite_id":1},{"name":"Release","check_suite_id":2}]}`))
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/4":
			json.NewDecoder(r.Body).Decode(&closed)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	runs, err := c.CheckHistory(context.Background(), "abc")
	if err != nil || len(runs) != 4 || runs[0].ID != 5 || runs[0].Conclusion != "failure" || runs[3].ID != 9 {
		t.Fatalf("runs = %+v %v", runs, err)
	}
	// Two workflows' "build" jobs, and another app's, are three checks.
	keys := map[int64]string{}
	for _, r := range runs {
		keys[r.ID] = r.Key + "|" + r.Group
	}
	if keys[5] != "CI / build|suite 1" || keys[9] != "CI / build|suite 1" || keys[6] != "Release / build|suite 2" || keys[7] != "buildkite: build|suite 3" {
		t.Fatalf("keys %v", keys)
	}
	if log, _ := c.CheckLog(context.Background(), runs[0]); log != "1 failed\nTestX timed out" {
		t.Fatalf("log = %q", log)
	}
	if err := c.CloseIssue(context.Background(), 4); err != nil || closed["state"] != "closed" {
		t.Fatalf("close: %v %v", closed, err)
	}
}

func TestRateLimitIsNotDenied(t *testing.T) {
	for _, c := range []struct {
		status  int
		header  string
		body    string
		limited bool
	}{
		{403, "", `{"message":"Resource not accessible by integration"}`, false},
		{403, "0", `{"message":"API rate limit exceeded"}`, true},
		{403, "", `{"message":"You have exceeded a secondary rate limit"}`, true},
		{429, "", ``, true},
		{404, "", ``, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.header != "" {
				w.Header().Set("X-RateLimit-Remaining", c.header)
			}
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		_, err := (&Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}).GetIssue(context.Background(), 1)
		srv.Close()
		var he *HTTPError
		if !errors.As(err, &he) || he.RateLimit() != c.limited {
			t.Errorf("%d %q: %v, limited want %v", c.status, c.body, err, c.limited)
		}
	}
}

func TestCheckHistoryRefusesAPartialList(t *testing.T) {
	page := `{"check_runs":[` + strings.TrimSuffix(strings.Repeat(`{"id":1,"name":"x","status":"completed","conclusion":"success"},`, 100), ",") + `]}`
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Write([]byte(page))
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if _, err := c.CheckHistory(context.Background(), "abc"); err == nil || pages != 10 {
		t.Fatalf("%d pages, %v: want an error after 10 full pages", pages, err)
	}
}

func TestDependabot(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/dependabot/alerts":
			if r.URL.Query().Get("after") == "c2" {
				w.Write([]byte(`[{"number":4,"state":"open"}]`))
				return
			}
			if q := r.URL.Query(); q.Get("state") != "open" || q.Get("severity") != "high,critical" {
				t.Errorf("query %v", q)
			}
			w.Header().Set("Link", `<`+base+`/repos/o/r/dependabot/alerts?after=c2>; rel="next", <https://elsewhere.example/x>; rel="last"`)
			w.Write([]byte(`[{"number":3,"state":"open","html_url":"https://github.com/o/r/security/dependabot/3",
				"dependency":{"package":{"ecosystem":"npm","name":"lodash"},"manifest_path":"web/package-lock.json"},
				"security_advisory":{"ghsa_id":"GHSA-xxxx-yyyy-zzzz","cve_id":"CVE-2026-1","summary":"Prototype pollution","severity":"high"},
				"security_vulnerability":{"severity":"high","vulnerable_version_range":"< 4.17.21","first_patched_version":{"identifier":"4.17.21"}}}]`))
		case "/repos/o/r/dependabot/alerts/3":
			w.Write([]byte(`{"number":3,"state":"fixed"}`))
		case "/repos/o/r/pulls":
			w.Write([]byte(`[{"number":9,"title":"Bump lodash from 4.17.20 to 4.17.21 in /web","html_url":"https://github.com/o/r/pull/9","user":{"login":"dependabot[bot]"},"head":{"ref":"dependabot/npm_and_yarn/web/lodash-4.17.21"}},
				{"number":10,"title":"Bump lodash","user":{"login":"someone"},"head":{"ref":"dependabot/npm_and_yarn/lodash-1"}},
				{"number":11,"title":"Bump lodash","user":{"login":"dependabot[bot]"},"head":{"ref":"update-lodash"}}]`))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	base = srv.URL
	ctx := context.Background()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	as, err := c.DependabotAlerts(ctx, []string{"high", "critical"})
	if err != nil || len(as) != 2 || as[1].Number != 4 {
		t.Fatalf("alerts %+v %v", as, err)
	}
	if nextLink(`<https://elsewhere.example/repos/o/r/dependabot/alerts?after=x>; rel="next"`, srv.URL) != "" {
		t.Fatal("followed a next link off the API's host")
	}
	if a := as[0]; a.Package != "lodash" || a.Manifest != "web/package-lock.json" || a.GHSA != "GHSA-xxxx-yyyy-zzzz" || a.Severity != "high" || a.Patched != "4.17.21" || a.VulnerableRange != "< 4.17.21" {
		t.Fatalf("alert %+v", a)
	}
	if a, err := c.DependabotAlert(ctx, 3); err != nil || a.State != "fixed" {
		t.Fatalf("one %+v %v", a, err)
	}
	if prs, err := c.DependabotPRs(ctx); err != nil || len(prs) != 1 || prs[0].Number != 9 {
		t.Fatalf("prs %+v %v", prs, err)
	}
}

func TestIssueAuthorAndNotPlanned(t *testing.T) {
	var closed map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/4":
			w.Write([]byte(`{"number":4,"title":"t","state":"open","user":{"login":"factory-app[bot]"}}`))
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/4":
			json.NewDecoder(r.Body).Decode(&closed)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if is, err := c.GetIssue(context.Background(), 4); err != nil || is.Author != "factory-app[bot]" {
		t.Fatalf("issue %+v %v", is, err)
	}
	if err := c.CloseNotPlanned(context.Background(), 4); err != nil || closed["state_reason"] != "not_planned" {
		t.Fatalf("close %v %v", closed, err)
	}
	if err := c.CloseDuplicate(context.Background(), 4); err != nil || closed["state_reason"] != "duplicate" || closed["state"] != "closed" {
		t.Fatalf("close as duplicate %v %v", closed, err)
	}
}

func TestPrivate(t *testing.T) {
	reply := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	for _, tc := range []struct {
		reply   map[string]any
		private bool
		err     bool
	}{
		{map[string]any{"private": true, "visibility": "private"}, true, false},
		{map[string]any{"private": true, "visibility": "internal"}, false, false}, // an enterprise's: not private
		{map[string]any{"private": false, "visibility": "public"}, false, false},
		{map[string]any{"private": true}, false, true}, // no visibility: unknown
	} {
		reply = tc.reply
		got, err := c.Private(context.Background())
		if got != tc.private || (err != nil) != tc.err {
			t.Errorf("%v: %v %v", tc.reply, got, err)
		}
	}
	c.Repo = "o/gone"
	if got, err := c.Private(context.Background()); got || err == nil {
		t.Fatalf("missing repo: %v %v", got, err)
	}
}

func TestListLabelsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/labels" {
			t.Errorf("unexpected %s", r.URL)
		}
		var page []map[string]string
		if r.URL.Query().Get("page") == "1" {
			for i := 0; i < 100; i++ {
				page = append(page, map[string]string{"name": fmt.Sprintf("l%d", i)})
			}
		} else {
			page = append(page, map[string]string{"name": "factory:ready"})
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	names, err := c.ListLabels(context.Background())
	if err != nil || len(names) != 101 || names[100] != "factory:ready" {
		t.Fatalf("labels = %d %v", len(names), err)
	}
}

func TestDeleteLabel(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.EscapedPath())
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/locked") {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Must have admin rights"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	ctx := context.Background()
	if err := c.DeleteLabel(ctx, "factory:split?"); err != nil {
		t.Fatal(err)
	}
	// One already deleted is what was asked for.
	if err := c.DeleteLabel(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLabel(ctx, "locked"); err == nil {
		t.Fatal("a refused delete passed")
	}
	if got[0] != "DELETE /repos/o/r/labels/factory:split%3F" {
		t.Fatalf("requests = %q", got)
	}
}

func TestCountOpenLabeled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/o/r/issues" || q.Get("labels") != "factory:parked" || q.Get("state") != "open" {
			t.Errorf("unexpected %s", r.URL)
		}
		// Issues and PRs both count.
		n := 3
		if q.Get("page") == "1" {
			n = 100
		}
		var page []map[string]any
		for i := range n {
			page = append(page, map[string]any{"number": i, "pull_request": map[string]any{}})
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	n, err := c.CountOpenLabeled(context.Background(), "factory:parked")
	if err != nil || n != 103 {
		t.Fatalf("count = %d %v", n, err)
	}
}

func TestCountOpenLabeledStopsAtItsCap(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := make([]map[string]int, 100)
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	n, err := c.CountOpenLabeled(context.Background(), "factory:ready")
	if err != nil || n != LabelCountCap || pages != 10 {
		t.Fatalf("count = %d after %d pages, %v", n, pages, err)
	}
}
