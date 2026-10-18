package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Preview screenshots a repo's web app after a change passes its gates:
// the app runs from the checked export, in the job image, and headless
// Chromium captures the pages, for the PR and its reviewer. Off unless a
// repo sets it.
type Preview struct {
	// Start runs the app, with $PORT set; required. It runs after the
	// install step, like a gate.
	Start string `yaml:"start"`
	// Port is where the app listens; default 3000.
	Port int `yaml:"port,omitempty"`
	// Ready is the path polled until it answers below 500; default /.
	Ready string `yaml:"ready,omitempty"`
	// Wait is how long the app has to be ready; default previews.wait.
	Wait Duration `yaml:"wait,omitempty"`
	// Pages are always captured; default /. The builder adds the ones its
	// change touches, up to previews.max_screenshots in all.
	Pages []string `yaml:"pages,omitempty"`
	// Viewports are desktop, tablet, phone or WIDTHxHEIGHT; default
	// previews.viewports.
	Viewports []string `yaml:"viewports,omitempty"`
	// Before captures the base branch too, so the PR shows before and after.
	Before bool `yaml:"before,omitempty"`
	// URL links a live preview your own hosting deploys for each PR, with
	// {pr}, {branch} and {sha} filled in.
	URL string `yaml:"url,omitempty"`
}

// Previews are the defaults and limits of every repo's preview.
type Previews struct {
	// Enabled is on unless set to false; each repo opts in with preview.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Timeout bounds one capture: starting the app, waiting and every
	// screenshot; default 5m.
	Timeout Duration `yaml:"timeout,omitempty"`
	// Wait is how long an app has to be ready; default 60s.
	Wait Duration `yaml:"wait,omitempty"`
	// MaxScreenshots caps the images per build, before and after
	// together; default 12.
	MaxScreenshots int `yaml:"max_screenshots,omitempty"`
	// Viewports default to desktop and phone.
	Viewports []string `yaml:"viewports,omitempty"`
	// KeepFor is how long a PR's uploaded screenshots stay after the last
	// upload; default 336h (two weeks). A merged or closed PR's go sooner.
	KeepFor Duration `yaml:"keep_for,omitempty"`
}

// Viewport is a browser window size.
type Viewport struct {
	Name          string
	Width, Height int
	Mobile        bool
}

var namedViewports = map[string]Viewport{
	"desktop": {"desktop", 1280, 800, false},
	"tablet":  {"tablet", 820, 1180, true},
	"phone":   {"phone", 390, 844, true},
}

var sizeViewport = regexp.MustCompile(`^([0-9]{3,4})x([0-9]{3,4})$`)

// ParseViewport reads desktop, tablet, phone or WIDTHxHEIGHT.
func ParseViewport(s string) (Viewport, error) {
	if v, ok := namedViewports[s]; ok {
		return v, nil
	}
	m := sizeViewport.FindStringSubmatch(s)
	if m == nil {
		return Viewport{}, fmt.Errorf("viewport %q must be desktop, tablet, phone or WIDTHxHEIGHT", s)
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	if w < 240 || w > 3840 || h < 240 || h > 3840 {
		return Viewport{}, fmt.Errorf("viewport %q: width and height go from 240 to 3840", s)
	}
	return Viewport{Name: s, Width: w, Height: h, Mobile: w < 800}, nil
}

// CheckPage says whether p is a path on the app: it starts with one
// slash and has no spaces, control characters, or characters that would
// break out of the PR comment's markdown.
func CheckPage(p string) error {
	switch {
	case !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//"):
		return fmt.Errorf("page %q must be a path starting with /", p)
	case len(p) > 200:
		return fmt.Errorf("page %q is longer than 200 characters", p)
	case strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune("\\`<>|\"'", r) }):
		return fmt.Errorf("page %q has a space, a control character or one of \\`<>|\"'", p)
	}
	return nil
}

// On says whether previews may run at all.
func (p Previews) On() bool { return p.Enabled == nil || *p.Enabled }

// Off turns previews off, for runs that aren't the real daemon's.
func (p *Previews) Off() { p.Enabled = new(bool) }

func (c *Config) previewDefaults() {
	p := &c.Previews
	if p.Timeout.Duration == 0 {
		p.Timeout.Duration = 5 * time.Minute
	}
	if p.Wait.Duration == 0 {
		p.Wait.Duration = time.Minute
	}
	if p.MaxScreenshots == 0 {
		p.MaxScreenshots = 12
	}
	if p.Viewports == nil {
		p.Viewports = []string{"desktop", "phone"}
	}
	if p.KeepFor.Duration == 0 {
		p.KeepFor.Duration = 14 * 24 * time.Hour
	}
}

// PreviewFor is a repo's preview with the defaults filled in, and whether
// it runs.
func (c *Config) PreviewFor(repo string) (Preview, bool) {
	r, ok := c.RepoConf(repo)
	if !ok || r.Preview == nil || !c.Previews.On() {
		return Preview{}, false
	}
	if n, _ := c.Workspace(repo); n != "" {
		return Preview{}, false
	}
	p := *r.Preview
	if p.Port == 0 {
		p.Port = 3000
	}
	if p.Ready == "" {
		p.Ready = "/"
	}
	if p.Wait.Duration == 0 {
		p.Wait = c.Previews.Wait
	}
	if len(p.Pages) == 0 {
		p.Pages = []string{"/"}
	}
	if len(p.Viewports) == 0 {
		p.Viewports = c.Previews.Viewports
	}
	return p, true
}

// PreviewURL fills in a live preview's link; "" without one.
func (p Preview) PreviewURL(pr int, branch, sha string) string {
	if p.URL == "" {
		return ""
	}
	return strings.NewReplacer("{pr}", strconv.Itoa(pr), "{branch}", url.PathEscape(branch), "{sha}", sha).Replace(p.URL)
}

func (c *Config) checkPreviews() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	g := c.Previews
	if g.Timeout.Duration < 30*time.Second {
		bad("previews.timeout must be at least 30s")
	}
	if g.Wait.Duration < time.Second || g.Wait.Duration > g.Timeout.Duration {
		bad("previews.wait must be at least 1s and no longer than previews.timeout")
	}
	if g.MaxScreenshots < 1 || g.MaxScreenshots > 50 {
		bad("previews.max_screenshots goes from 1 to 50")
	}
	if g.KeepFor.Duration < time.Hour {
		bad("previews.keep_for must be at least 1h")
	}
	if err := checkViewports(g.Viewports); err != nil {
		bad("previews.viewports: %v", err)
	}
	for _, r := range c.Repos {
		p := r.Preview
		if p == nil {
			continue
		}
		where := "repo " + r.Name + ": preview"
		if n, _ := c.Workspace(r.Name); n != "" {
			bad("%s: previews aren't supported for a repo in a workspace yet (%s)", where, n)
		}
		if strings.TrimSpace(p.Start) == "" {
			bad("%s.start is required: the command that runs the app on $PORT", where)
		}
		if p.Port < 0 || p.Port > 65535 {
			bad("%s.port must be a TCP port", where)
		}
		pages := p.Pages
		if p.Ready != "" {
			pages = append([]string{p.Ready}, pages...)
		}
		for _, pg := range pages {
			if err := CheckPage(pg); err != nil {
				bad("%s: %v", where, err)
			}
		}
		if p.Wait.Duration < 0 || p.Wait.Duration > g.Timeout.Duration {
			bad("%s.wait can't be longer than previews.timeout", where)
		}
		if p.Viewports != nil {
			if err := checkViewports(p.Viewports); err != nil {
				bad("%s.viewports: %v", where, err)
			}
		}
		if p.URL != "" {
			u, err := url.Parse(p.PreviewURL(1, "b", "s"))
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
				bad("%s.url must be an http(s) address, such as https://pr-{pr}.preview.example.com", where)
			}
		}
	}
	return errs
}

func checkViewports(vs []string) error {
	if len(vs) == 0 {
		return errors.New("name at least one, such as desktop")
	}
	seen := map[string]bool{}
	for _, v := range vs {
		if _, err := ParseViewport(v); err != nil {
			return err
		}
		if seen[v] {
			return fmt.Errorf("%s is listed twice", v)
		}
		seen[v] = true
	}
	return nil
}
