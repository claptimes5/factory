package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPreview(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	y := strings.Replace(string(b), "repo: claptimes5/factory", "repos:\n  - name: o/web\n    preview: {start: npm run dev -- --port $PORT}\n  - o/api", 1)
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Previews
	if !g.On() || g.Timeout.Duration != 5*time.Minute || g.Wait.Duration != time.Minute || g.MaxScreenshots != 12 ||
		strings.Join(g.Viewports, ",") != "desktop,phone" || g.KeepFor.Duration != 336*time.Hour {
		t.Fatalf("defaults: %+v", g)
	}
	p, ok := c.PreviewFor("o/web")
	if !ok || p.Port != 3000 || p.Ready != "/" || p.Wait.Duration != time.Minute || strings.Join(p.Pages, ",") != "/" || strings.Join(p.Viewports, ",") != "desktop,phone" {
		t.Fatalf("resolved: %+v %v", p, ok)
	}
	if _, ok := c.PreviewFor("o/api"); ok {
		t.Fatal("a repo without preview runs one")
	}
	c.Previews.Off()
	if _, ok := c.PreviewFor("o/web"); ok {
		t.Fatal("previews.enabled false still runs one")
	}
	if v, err := ParseViewport("1440x900"); err != nil || v.Width != 1440 || v.Height != 900 || v.Mobile {
		t.Fatalf("viewport %+v %v", v, err)
	}
	set := strings.Replace(y, "preview: {start: npm run dev -- --port $PORT}",
		"preview: {start: bin/dev, port: 8080, ready: /health, wait: 2m, pages: [/, /settings], viewports: [tablet, 1440x900], before: true, url: 'https://pr-{pr}.example.com'}", 1)
	c, err = Parse([]byte(set + "previews: {timeout: 10m, max_screenshots: 4, keep_for: 24h}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ = c.PreviewFor("o/web")
	if p.Port != 8080 || p.Ready != "/health" || p.Wait.Duration != 2*time.Minute || len(p.Pages) != 2 || !p.Before || p.PreviewURL(7, "f/x", "abc") != "https://pr-7.example.com" {
		t.Fatalf("set: %+v", p)
	}
	for name, bad := range map[string]string{
		"no start":          "preview: {port: 3000}",
		"bad port":          "preview: {start: x, port: 70000}",
		"page not a path":   "preview: {start: x, pages: [settings]}",
		"protocol-relative": "preview: {start: x, pages: ['//evil.example']}",
		"page with space":   "preview: {start: x, pages: ['/a b']}",
		"bad ready":         "preview: {start: x, ready: 'http://x/'}",
		"bad viewport":      "preview: {start: x, viewports: [watch]}",
		"tiny viewport":     "preview: {start: x, viewports: [100x100]}",
		"twice":             "preview: {start: x, viewports: [phone, phone]}",
		"bad url":           "preview: {start: x, url: 'ftp://x'}",
		"wait too long":     "preview: {start: x, wait: 1h}",
	} {
		in := strings.Replace(y, "preview: {start: npm run dev -- --port $PORT}", bad, 1)
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
	for name, bad := range map[string]string{
		"short timeout":   "previews: {timeout: 10s}",
		"wait > timeout":  "previews: {timeout: 1m, wait: 2m}",
		"too many":        "previews: {max_screenshots: 100}",
		"short keep":      "previews: {keep_for: 10m}",
		"empty viewports": "previews: {viewports: []}",
	} {
		if _, err := Parse([]byte(y + bad + "\n")); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
	ws := y + "workspaces: {shop: [o/web, o/api]}\n"
	if _, err := Parse([]byte(ws)); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("preview in a workspace: %v", err)
	}
}

// A duration writes back as it reads, so setup can rewrite a repo with a
// preview wait.
func TestDurationRoundTrips(t *testing.T) {
	r := Repo{Name: "o/web", Preview: &Preview{Start: "npm start", Wait: Duration{2 * time.Minute}}}
	b, err := yaml.Marshal([]Repo{r})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "wait: 2m0s") {
		t.Fatalf("yaml:\n%s", b)
	}
	var back []Repo
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatalf("read back: %v\n%s", err, b)
	}
	if len(back) != 1 || back[0].Preview == nil || back[0].Preview.Wait.Duration != 2*time.Minute {
		t.Fatalf("read back %+v", back)
	}
}
