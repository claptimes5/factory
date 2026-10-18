package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
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
				{"id":9,"name":"test","status":"completed","conclusion":"success","html_url":"https://ci/9","output":{"title":"ok","summary":""}},
				{"id":5,"name":"test","status":"completed","conclusion":"failure","html_url":"https://ci/5","output":{"title":"1 failed","summary":"TestX timed out"}}]}`))
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/4":
			json.NewDecoder(r.Body).Decode(&closed)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	runs, err := c.CheckHistory(context.Background(), "abc")
	if err != nil || len(runs) != 2 || runs[0].ID != 5 || runs[0].Conclusion != "failure" || runs[1].ID != 9 {
		t.Fatalf("runs = %+v %v", runs, err)
	}
	if log, _ := c.CheckLog(context.Background(), runs[0]); log != "1 failed\nTestX timed out" {
		t.Fatalf("log = %q", log)
	}
	if err := c.CloseIssue(context.Background(), 4); err != nil || closed["state"] != "closed" {
		t.Fatalf("close: %v %v", closed, err)
	}
}
