package preview

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
)

func TestPages(t *testing.T) {
	got := Pages([]string{"/", "/settings"}, []string{"/settings", "http://evil.example/", "//evil", "/a b", " /billing ", "/x", "/y"}, 4)
	if strings.Join(got, ",") != "/,/settings,/billing,/x" {
		t.Fatalf("pages = %q", got)
	}
}

func TestPlansCap(t *testing.T) {
	p := config.Preview{Start: "x", Port: 3000, Ready: "/", Pages: []string{"/", "/a"}, Viewports: []string{"desktop", "phone"}, Before: true}
	p.Wait.Duration = time.Second
	pls := Plans(p, []string{"/b", "/c", "/d"}, 12, time.Minute)
	// 12 in all, 6 a side, 3 pages at two sizes: the builder's /c and /d are dropped.
	if len(pls) != 2 || pls[0].Side != After || pls[1].Side != Before || len(pls[0].Targets) != 6 || len(pls[1].Targets) != 6 {
		t.Fatalf("plans = %+v", pls)
	}
	if tg := pls[0].Targets[5]; tg.Page != "/b" || tg.Viewport != "phone" || tg.File != "after-03-phone.png" || !tg.Mobile {
		t.Fatalf("last target %+v", tg)
	}
	if pls := Plans(p, nil, 1, time.Minute); pls != nil {
		t.Fatalf("one screenshot can't cover two sides: %+v", pls)
	}
	p.Before = false
	if pls := Plans(p, nil, 1, time.Minute); len(pls) != 1 || len(pls[0].Targets) != 1 || pls[0].Targets[0].Viewport != "desktop" {
		t.Fatalf("cap of one: %+v", pls)
	}
}

func TestNotes(t *testing.T) {
	out := "noise\nfactory-preview: not ready: ECONNREFUSED\nmore\nfactory-preview: " + strings.Repeat("x", 500) + "\n"
	n := Notes(out)
	if len(n) != 2 || n[0] != "not ready: ECONNREFUSED" || len(n[1]) > 310 {
		t.Fatalf("notes = %q", n)
	}
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// Only regular PNG files the plan named come back: the app shares the
// container and can leave anything in the folder.
func TestCollectTakesOnlyPlannedPNGs(t *testing.T) {
	work, dest := t.TempDir(), t.TempDir()
	out := filepath.Join(work, OutDir)
	os.MkdirAll(out, 0o755)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, pngBytes(10, 10), 0o644)
	os.WriteFile(filepath.Join(out, "after-01-desktop.png"), pngBytes(1280, 900), 0o644)
	os.Symlink(secret, filepath.Join(out, "after-01-phone.png"))
	os.WriteFile(filepath.Join(out, "after-02-desktop.png"), []byte("<html>not a png"), 0o644)
	os.WriteFile(filepath.Join(out, "after-02-phone.png"), pngBytes(390, 5000), 0o644)
	os.WriteFile(filepath.Join(out, "extra.png"), pngBytes(10, 10), 0o644)
	// A good header and a cut-off image: only decoding all of it tells.
	whole := pngBytes(200, 200)
	os.WriteFile(filepath.Join(out, "after-03-phone.png"), whole[:len(whole)-40], 0o644)
	pl := Plan{Side: After, Targets: []Target{
		{Page: "/", Viewport: "desktop", File: "after-01-desktop.png"}, {Page: "/", Viewport: "phone", File: "after-01-phone.png"},
		{Page: "/a", Viewport: "desktop", File: "after-02-desktop.png"}, {Page: "/a", Viewport: "phone", File: "after-02-phone.png"},
		{Page: "/b", Viewport: "desktop", File: "after-03-desktop.png"}, {Page: "/b", Viewport: "phone", File: "after-03-phone.png"},
	}}
	shots, notes := Collect(work, pl, dest)
	if len(shots) != 1 || shots[0].File != "after-01-desktop.png" || shots[0].Height != 900 || shots[0].Side != After {
		t.Fatalf("shots = %+v", shots)
	}
	if len(notes) != 4 || !strings.Contains(notes[3], "/b at phone: a broken PNG") {
		t.Fatalf("notes = %q", notes)
	}
	ents, _ := os.ReadDir(dest)
	if len(ents) != 1 {
		t.Fatalf("dest has %d files", len(ents))
	}
	// The folder itself may not be a link out of the export.
	work2 := t.TempDir()
	os.Symlink(out, filepath.Join(work2, OutDir))
	if shots, _ := Collect(work2, pl, t.TempDir()); len(shots) != 0 {
		t.Fatalf("followed a linked folder: %+v", shots)
	}
}

func TestTruncateKeepsRunesWhole(t *testing.T) {
	if got := truncate("ab€", 3); got != "ab…" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestManifest(t *testing.T) {
	dir := t.TempDir()
	if m, err := Load(dir); m != nil || err != nil {
		t.Fatalf("no manifest: %v %v", m, err)
	}
	m := &Manifest{Head: "abc", Shots: []Shot{{File: "after-01-desktop.png"}}}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || got.Head != "abc" || !got.Has("after-01-desktop.png") || got.Has("../x") {
		t.Fatalf("loaded %+v %v", got, err)
	}
}

// The command against a real app and browser. Skipped where Playwright
// isn't installed globally, as the job image installs it.
func TestCommandCapturesARealApp(t *testing.T) {
	root, err := exec.Command("npm", "root", "-g").Output()
	if err != nil {
		t.Skip("npm not installed")
	}
	nodePath := strings.TrimSpace(string(root))
	if _, err := os.Stat(filepath.Join(nodePath, "playwright")); err != nil {
		t.Skip("playwright not installed globally")
	}
	t.Setenv("NODE_PATH", nodePath)
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "server.js"), []byte(`require('http').createServer((q, s) => {
  if (q.url === '/hang') return;
  if (q.url === '/broken') { s.writeHead(500); s.end('no'); return; }
  s.writeHead(200, {'content-type': 'text/html'}); s.end('<h1 style="height:1500px">Hello ' + q.url + '</h1>');
}).listen(process.env.PORT, '127.0.0.1');`), 0o644)
	p := config.Preview{Start: "node server.js", Port: 18000 + os.Getpid()%1000, Ready: "/", Pages: []string{"/", "/settings"}, Viewports: []string{"desktop", "phone"}}
	p.Wait.Duration = 20 * time.Second
	pl := Plans(p, nil, 12, time.Minute)[0]
	var out strings.Builder
	code, err := runner.Local{}.Run(context.Background(), runner.Spec{Workdir: work, Cmd: []string{"sh", "-c", pl.Command()}, Timeout: time.Minute}, &out)
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v\n%s", code, err, out.String())
	}
	shots, notes := Collect(work, pl, t.TempDir())
	if len(shots) != 4 || len(notes) != 0 {
		t.Fatalf("shots %+v notes %q\n%s", shots, notes, out.String())
	}
	if s := shots[0]; s.Width != 1280 || s.Height < 1500 {
		t.Fatalf("desktop shot is %dx%d, want the full page", s.Width, s.Height)
	}

	// A page that never finishes costs its own time, not the rest's, and
	// the whole capture keeps within previews.timeout.
	p.Pages, p.Viewports = []string{"/hang", "/", "/a", "/b"}, []string{"desktop"}
	pl = Plans(p, nil, 12, 30*time.Second)[0]
	pl.TargetMS = 3000
	out.Reset()
	start := time.Now()
	code, err = runner.Local{}.Run(context.Background(), runner.Spec{Workdir: work, Cmd: []string{"sh", "-c", pl.Command()}, Timeout: time.Minute}, &out)
	if err != nil || code != 0 || time.Since(start) > 25*time.Second {
		t.Fatalf("exit %d %v after %s\n%s", code, err, time.Since(start), out.String())
	}
	shots, _ = Collect(work, pl, t.TempDir())
	if len(shots) != 3 || !strings.Contains(out.String(), "/hang at desktop") {
		t.Fatalf("shots %+v\n%s", shots, out.String())
	}
	// Out of budget, the pages left are skipped and said so.
	pl.TargetMS, pl.BudgetMS = 8000, 5000
	out.Reset()
	start = time.Now()
	code, _ = runner.Local{}.Run(context.Background(), runner.Spec{Workdir: work, Cmd: []string{"sh", "-c", pl.Command()}, Timeout: time.Minute}, &out)
	if code != 0 || time.Since(start) > 20*time.Second || !strings.Contains(out.String(), "out of time (previews.timeout): 3 screenshots skipped") {
		t.Fatalf("exit %d after %s\n%s", code, time.Since(start), out.String())
	}

	// An app that never answers is reported as not ready.
	p.Start, p.Wait.Duration = "sleep 30", time.Second
	pl = Plans(p, nil, 12, time.Minute)[0]
	out.Reset()
	code, _ = runner.Local{}.Run(context.Background(), runner.Spec{Workdir: work, Cmd: []string{"sh", "-c", pl.Command()}, Timeout: time.Minute}, &out)
	if code != ExitNotReady || len(Notes(out.String())) != 1 {
		t.Fatalf("not ready: exit %d\n%s", code, out.String())
	}
}
