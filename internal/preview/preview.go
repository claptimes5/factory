// Package preview plans and collects a web app's screenshots: which pages
// at which sizes, the command that runs the app and captures them in the
// job image, and the manifest a build's screenshots are listed in. The
// pipeline runs the command; the dashboard and the PR comment read the
// manifest.
package preview

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/config"
)

// Dir is the folder of a job's directory that holds its screenshots.
const Dir = "preview"

const manifestFile = "manifest.json"

// OutDir is where the capture writes, inside the export it runs in.
const OutDir = ".factory-preview"

// Limits on what a capture may hand back: the app under test runs in the
// same container and can write anything there.
const (
	MaxShotBytes = 5 << 20
	MaxHeight    = 4000 // pixels: a long page is cut here
)

// Exit codes of the capture command.
const (
	ExitNoBrowser = 90 // the job image has no Playwright
	ExitNotReady  = 91 // the app never answered
)

// Sides of a change.
const (
	After  = "after"  // the change
	Before = "before" // its base
)

// Shot is one screenshot.
type Shot struct {
	File     string `json:"file"`
	Page     string `json:"page"`
	Viewport string `json:"viewport"`
	Side     string `json:"side"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Manifest lists a build's screenshots.
type Manifest struct {
	Head  string `json:"head"`
	Base  string `json:"base,omitempty"`
	Shots []Shot `json:"shots"`
	// Notes say what couldn't be captured, and why. They quote the app
	// and the browser, so they are untrusted text.
	Notes []string `json:"notes,omitempty"`
	// Summary says the same in the factory's own words, for the PR.
	Summary []string `json:"summary,omitempty"`
	At      string   `json:"at"`
}

// Load reads the manifest in a job's preview folder; nil without one.
func Load(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Save writes the manifest into dir.
func (m *Manifest) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if m.Shots == nil {
		m.Shots = []Shot{}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, manifestFile), b, 0o644)
}

// Has says whether the manifest lists a file by that name.
func (m *Manifest) Has(name string) bool {
	for _, s := range m.Shots {
		if s.File == name {
			return true
		}
	}
	return false
}

// Pages are the configured pages, then the builder's, without repeats and
// at most max. A builder's page that isn't a plain path is dropped: it is
// model output.
func Pages(configured, builder []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); len(out) < max && !seen[p] && config.CheckPage(p) == nil {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range configured {
		add(p)
	}
	for _, p := range builder {
		add(p)
	}
	return out
}

// Target is one screenshot to take.
type Target struct {
	Page     string `json:"page"`
	Viewport string `json:"viewport"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Mobile   bool   `json:"mobile"`
	File     string `json:"file"`
}

// Plan is one capture: one side of the change.
type Plan struct {
	Side   string        `json:"-"`
	Start  string        `json:"-"`
	Port   int           `json:"port"`
	Ready  string        `json:"ready"`
	Wait   time.Duration `json:"-"`
	WaitMS int64         `json:"wait_ms"`
	Out    string        `json:"out"`
	Height int           `json:"max_height"`
	// TargetMS bounds one page and BudgetMS the whole script, waiting
	// for the app included: a page that hangs costs its own time, and the
	// rest of the plan gets what's left of previews.timeout.
	TargetMS int64    `json:"target_ms"`
	BudgetMS int64    `json:"budget_ms"`
	Targets  []Target `json:"targets"`
}

// TargetTimeout is as long as one page gets to load and be taken.
const TargetTimeout = 45 * time.Second

// budget is the script's share of a capture's timeout: the rest is for
// starting the browser, stopping the app and the shell around it.
func budget(timeout time.Duration) time.Duration {
	return max(timeout-20*time.Second, 10*time.Second)
}

// Plans are the captures for a change: the change's pages at each
// viewport, and the base's too with before. Together they take at most
// limit screenshots; builder pages are dropped first. timeout is
// previews.timeout, which bounds each capture.
func Plans(p config.Preview, builder []string, limit int, timeout time.Duration) []Plan {
	sides := []string{After}
	if p.Before {
		sides = append(sides, Before)
	}
	var vps []config.Viewport
	for _, v := range p.Viewports {
		if vp, err := config.ParseViewport(v); err == nil {
			vps = append(vps, vp)
		}
	}
	perSide := limit / len(sides)
	if perSide < 1 || len(vps) == 0 {
		return nil
	}
	if len(vps) > perSide {
		vps = vps[:perSide]
	}
	pages := Pages(p.Pages, builder, perSide/len(vps))
	var out []Plan
	for _, side := range sides {
		pl := Plan{Side: side, Start: p.Start, Port: p.Port, Ready: p.Ready, Wait: p.Wait.Duration, WaitMS: p.Wait.Milliseconds(), Out: OutDir, Height: MaxHeight,
			TargetMS: TargetTimeout.Milliseconds(), BudgetMS: budget(timeout).Milliseconds()}
		for i, pg := range pages {
			for _, vp := range vps {
				pl.Targets = append(pl.Targets, Target{Page: pg, Viewport: vp.Name, Width: vp.Width, Height: vp.Height, Mobile: vp.Mobile,
					File: fmt.Sprintf("%s-%02d-%s.png", side, i+1, vp.Name)})
			}
		}
		out = append(out, pl)
	}
	return out
}

// Command is the shell command that runs the app and takes the plan's
// screenshots into OutDir, run in the export like a gate. Playwright is
// the job image's own, by absolute path: the repo needn't depend on it,
// and a node_modules/playwright the change commits isn't the one that
// runs.
func (pl Plan) Command() string {
	plan, _ := json.Marshal(pl)
	return fmt.Sprintf(`O=%[1]s
rm -rf "$O" && mkdir -p "$O" || exit 93
export PORT=%[2]d
S=; command -v setsid >/dev/null 2>&1 && S=setsid
$S sh -c %[3]s >"$O/app.log" 2>&1 </dev/null &
APP=$!
FACTORY_PLAN=%[4]s node -e %[5]s
code=$?
kill -TERM -"$APP" 2>/dev/null || kill -TERM "$APP" 2>/dev/null
if [ "$code" -ne 0 ]; then echo "--- the app's log:"; tail -c 3000 "$O/app.log"; fi
exit "$code"
`, OutDir, pl.Port, shellQuote(pl.Start), base64.StdEncoding.EncodeToString(plan), shellQuote(captureJS))
}

// captureJS waits for the app, then takes each screenshot, a page that
// fails to load being reported and skipped. Each page has its own
// context and at most target_ms; when budget_ms runs out the pages left
// are skipped and said so, so a hanging page can't use up the capture.
const captureJS = `
const path = require('path');
const plan = JSON.parse(Buffer.from(process.env.FACTORY_PLAN, 'base64').toString());
const np = (process.env.NODE_PATH || '/usr/lib/node_modules').split(':')[0];
const say = (m) => console.log('factory-preview: ' + String(m).split('\n')[0]);
const deadline = Date.now() + plan.budget_ms;
const within = (p, ms, what) => {
  let timer;
  const t = new Promise((_, no) => { timer = setTimeout(() => no(new Error(what + ' took longer than ' + Math.round(ms / 1000) + 's')), ms); });
  return Promise.race([p, t]).finally(() => clearTimeout(timer));
};
let pw;
try { pw = require(path.join(np, 'playwright')); } catch (e) { say('no Playwright in the job image: ' + e.message); process.exit(90); }
const base = 'http://127.0.0.1:' + plan.port;
(async () => {
  const until = Math.min(Date.now() + plan.wait_ms, deadline);
  for (let last = '';;) {
    try {
      const r = await fetch(base + plan.ready, { redirect: 'manual', signal: AbortSignal.timeout(5000) });
      if (r.status < 500) break;
      last = 'HTTP ' + r.status;
    } catch (e) { last = (e.cause && e.cause.code) || e.message; }
    if (Date.now() > until) { say('not ready: ' + last); process.exit(91); }
    await new Promise((r) => setTimeout(r, 500));
  }
  const browser = await within(pw.chromium.launch(), Math.max(deadline - Date.now(), 1000), 'starting the browser');
  for (let i = 0; i < plan.targets.length; i++) {
    const t = plan.targets[i];
    const ms = Math.min(plan.target_ms, deadline - Date.now());
    if (ms < 1000) { say('out of time (previews.timeout): ' + (plan.targets.length - i) + ' screenshots skipped'); break; }
    let ctx;
    try {
      await within((async () => {
        ctx = await browser.newContext({ viewport: { width: t.width, height: t.height }, isMobile: t.mobile, hasTouch: t.mobile, deviceScaleFactor: 1 });
        const page = await ctx.newPage();
        await page.goto(base + t.page, { waitUntil: 'load', timeout: ms });
        await page.waitForLoadState('networkidle', { timeout: Math.min(5000, ms / 4) }).catch(() => {});
        const full = await page.evaluate(() => document.documentElement.scrollHeight);
        const h = Math.min(Math.max(full, t.height), plan.max_height);
        await page.screenshot({ path: path.join(plan.out, t.file), fullPage: true, clip: { x: 0, y: 0, width: t.width, height: h }, timeout: ms });
      })(), ms, t.page);
    } catch (e) { say(t.page + ' at ' + t.viewport + ': ' + e.message); }
    if (ctx) await within(ctx.close(), 5000, 'closing').catch(() => {});
  }
  await within(browser.close(), 5000, 'closing the browser').catch(() => {});
})().then(() => process.exit(0), (e) => { say(e.message); process.exit(92); });
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var noteLine = regexp.MustCompile(`(?m)^factory-preview: (.+)$`)

// Notes are what the capture said went wrong, from its output.
func Notes(out string) []string {
	var notes []string
	for _, m := range noteLine.FindAllStringSubmatch(out, 10) {
		notes = append(notes, truncate(strings.TrimSpace(m[1]), 300))
	}
	return notes
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// Collect moves the plan's screenshots out of the export it ran in into
// dest. Only the files the plan named are taken, and only regular files
// that are PNGs of a sane size: the app could have left a symlink or
// anything else there. It says why each missing one is missing.
func Collect(work string, pl Plan, dest string) ([]Shot, []string) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, []string{err.Error()}
	}
	defer func() { _ = root.Close() }()
	if fi, err := root.Lstat(OutDir); err != nil || !fi.IsDir() {
		return nil, []string{"the capture left no screenshots"}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, []string{err.Error()}
	}
	var shots []Shot
	var notes []string
	for _, t := range pl.Targets {
		rel := OutDir + "/" + t.File
		b, err := readShot(root, rel)
		if errors.Is(err, os.ErrNotExist) {
			continue // the capture said why, or ran out of time
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s at %s: %v", t.Page, t.Viewport, err))
			continue
		}
		cfg, err := checkPNG(b)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s at %s: %v", t.Page, t.Viewport, err))
			continue
		}
		if err := os.WriteFile(filepath.Join(dest, t.File), b, 0o644); err != nil {
			notes = append(notes, err.Error())
			continue
		}
		shots = append(shots, Shot{File: t.File, Page: t.Page, Viewport: t.Viewport, Side: pl.Side, Width: cfg.Width, Height: cfg.Height})
	}
	return shots, notes
}

// checkPNG decodes the whole image, so a truncated or corrupt file is
// refused and not just one with a good header; the size is checked from
// the header first so a huge one isn't decoded.
func checkPNG(b []byte) (image.Config, error) {
	bad := fmt.Errorf("not a PNG of at most 4000×%d", MaxHeight)
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width > 4000 || cfg.Height > MaxHeight || cfg.Width < 1 || cfg.Height < 1 {
		return cfg, bad
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		return cfg, fmt.Errorf("a broken PNG: %v", err)
	}
	return cfg, nil
}

func readShot(root *os.Root, rel string) ([]byte, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > MaxShotBytes {
		return nil, fmt.Errorf("larger than %d MB", MaxShotBytes>>20)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxShotBytes+1))
	if err == nil && len(b) > MaxShotBytes {
		err = fmt.Errorf("larger than %d MB", MaxShotBytes>>20)
	}
	return b, err
}
