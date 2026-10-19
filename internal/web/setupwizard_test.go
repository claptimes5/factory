package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/store/storetest"
)

// fakeWizard records what reached it.
type fakeWizard struct {
	mu      sync.Mutex
	did     []string
	in      []WizardInput
	started []string // callback URLs
	live    []bool
	state   string
}

func (f *fakeWizard) State(_ context.Context, live bool) APIWizard {
	f.mu.Lock()
	f.live = append(f.live, live)
	f.mu.Unlock()
	return APIWizard{Steps: []APIWizardStep{{Key: "machine", Title: "This machine", State: "done"}}, Notify: APIWizardSecret{Set: true, Masked: "http…abcd"}}
}

func (f *fakeWizard) Do(_ context.Context, action string, in WizardInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Key == "bad" {
		return "", errors.New("that doesn't look like a key")
	}
	f.did, f.in = append(f.did, action), append(f.in, in)
	return "did " + action, nil
}

func (f *fakeWizard) StartGitHubApp(_ context.Context, callback string) (APIWizardManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, callback)
	f.state = "s1"
	return APIWizardManifest{URL: "https://github.example/settings/apps/new?state=s1", State: "s1", Manifest: "{}"}, nil
}

func (f *fakeWizard) FinishGitHubApp(_ context.Context, code, state string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "" || state != f.state {
		return "", errors.New("that link is from another setup")
	}
	f.state = ""
	return "Created the app " + code, nil
}

func wizardServer(t *testing.T, s *Server) string {
	t.Helper()
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWizardAPI(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	fw := &fakeWizard{}
	base := wizardServer(t, &Server{Store: st, Actions: &fakeActions{}, Repos: []string{"o/r"}, Wizard: fw, Listen: "127.0.0.1:8080"})

	var w APIWizard
	getJSON(t, base+"/api/v1/setup", &w)
	if w.Mode != "normal" || !w.Writable || len(w.Steps) != 1 || w.Notify.Masked != "http…abcd" {
		t.Fatalf("state = %+v", w)
	}
	getJSON(t, base+"/api/v1/setup?live=1", &w)
	if fmt.Sprint(fw.live) != "[false true]" {
		t.Fatalf("live = %v", fw.live)
	}
	code, body := postJSON(t, base+"/api/v1/setup/claude", "", `{"key":"sk-ant-x","anyway":true}`)
	var res APIWizardResult
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil || res.Message != "did claude" || res.Wizard.Mode != "normal" {
		t.Fatalf("claude = %d %s", code, body)
	}
	if in := fw.in[0]; in.Key != "sk-ant-x" || !in.Anyway {
		t.Fatalf("input = %+v", in)
	}
	if code, body := postJSON(t, base+"/api/v1/setup/claude", "", `{"key":"bad"}`); code != 400 || !strings.Contains(body, "doesn't look like a key") {
		t.Fatalf("a refused key = %d %s", code, body)
	}
	if code, _ := postJSON(t, base+"/api/v1/setup/nope", "", `{}`); code != 404 {
		t.Fatalf("unknown action = %d", code)
	}
	// A cross-site POST is refused before it reaches the wizard.
	if code, _ := postJSON(t, base+"/api/v1/setup/finish", "https://evil.example", `{}`); code != 403 {
		t.Fatalf("cross-origin = %d", code)
	}
	if strings.Join(fw.did, ",") != "claude" {
		t.Fatalf("did %v", fw.did)
	}

	// The app flow: GitHub comes back to this server, from where the page was.
	code, body = postJSON(t, base+"/api/v1/setup/github/app", "", `{}`)
	if code != 200 || !strings.Contains(body, `"state":"s1"`) || fw.started[0] != base+"/api/v1/setup/github/callback" {
		t.Fatalf("start = %d %s, callback %v", code, body, fw.started)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	back := func(q string) *url.URL {
		resp, err := noFollow.Get(base + "/api/v1/setup/github/callback?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
		u, _ := url.Parse(resp.Header.Get("Location"))
		return u
	}
	if u := back("code=C1&state=s1"); u.Path != "/app/get-started" || u.Query().Get("github") != "Created the app C1" {
		t.Fatalf("back to %s", u)
	}
	// Each state works once.
	if u := back("code=C1&state=s1"); u.Query().Get("github_error") == "" {
		t.Fatalf("a used state went to %s", u)
	}
}

func TestWizardNeedsThePasswordOrThisMachine(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	fw := &fakeWizard{}
	// Listening beyond this machine without a password: read only.
	base := wizardServer(t, &Server{Store: st, Actions: &fakeActions{}, Wizard: fw, Listen: "100.64.0.1:8080", Hosts: []string{"127.0.0.1"}})
	var w APIWizard
	getJSON(t, base+"/api/v1/setup", &w)
	if w.Writable || w.Reason == "" {
		t.Fatalf("state = %+v", w)
	}
	if code, _ := postJSON(t, base+"/api/v1/setup/notify", "", `{"url":""}`); code != 403 {
		t.Fatalf("save = %d", code)
	}
	if code, _ := postJSON(t, base+"/api/v1/setup/github/app", "", `{}`); code != 403 {
		t.Fatalf("app = %d", code)
	}
	if len(fw.did) > 0 || len(fw.started) > 0 {
		t.Fatalf("reached the wizard: %v %v", fw.did, fw.started)
	}
}

func TestSetupModeServesOnlyTheWizard(t *testing.T) {
	t.Parallel()
	fw := &fakeWizard{}
	base := wizardServer(t, &Server{Wizard: fw, SetupMode: true, Listen: "127.0.0.1:8080"})
	var w APIWizard
	getJSON(t, base+"/api/v1/setup", &w)
	if w.Mode != "setup" {
		t.Fatalf("mode = %q", w.Mode)
	}
	for _, p := range []string{"/api/v1/overview", "/api/v1/board", "/api/v1/stream", "/board"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "/app/get-started") {
			t.Fatalf("%s = %d %s", p, resp.StatusCode, b)
		}
	}
	if code, body := postJSON(t, base+"/api/v1/setup/finish", "", `{}`); code != 200 || !strings.Contains(body, "did finish") {
		t.Fatalf("finish = %d %s", code, body)
	}
	// The app itself loads, on any of its routes.
	if page := get(t, base+"/app/get-started"); !strings.Contains(page, "<div id=\"root\">") {
		t.Fatalf("app page:\n%.300s", page)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != "/app/get-started" {
		t.Fatalf("/ goes to %q", resp.Header.Get("Location"))
	}
}

func TestSetupModeKeepsThePassword(t *testing.T) {
	t.Parallel()
	base := wizardServer(t, &Server{Wizard: &fakeWizard{}, SetupMode: true, Listen: "0.0.0.0:8080", Login: &Login{Password: "correct horse battery", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	resp, err := http.Get(base + "/api/v1/setup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out = %d", resp.StatusCode)
	}
}
