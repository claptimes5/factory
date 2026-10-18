package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
)

// githubWeb is GitHub's site root; tests point it elsewhere.
var githubWeb = "https://github.com"

// browse stands in for the person's browser in tests.
var browse func(base string)

// appPermissions are what the factory needs, and nothing else. Workflows is
// left out on purpose, so the factory can never edit its own CI.
var appPermissions = map[string]string{
	"contents": "write", "issues": "write", "pull_requests": "write", "checks": "read", "metadata": "read",
}

// createApp registers a GitHub App for the factory using GitHub's manifest
// flow: the browser posts a prepared form to GitHub, you confirm the name,
// and GitHub sends back a code this exchanges for the app's ID and key.
// It returns the app's ID, slug and the path the key was saved to.
func createApp(ctx context.Context, p *prompter, cfg *config.Config, keyDir string) (id, slug, keyPath string, err error) {
	owner, name, _ := strings.Cut(cfg.Repo, "/")
	org, err := github.IsOrg(ctx, githubAPI, owner)
	if err != nil {
		return "", "", "", fmt.Errorf("looking up %s on GitHub: %w", owner, err)
	}
	newURL := githubWeb + "/settings/apps/new"
	if org {
		newURL = githubWeb + "/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
	}
	// Loopback only: whoever can load the page can finish the flow, so it
	// must not be reachable from the network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = ln.Close() }()
	base := "http://" + ln.Addr().String()
	state := randHex(16)
	// GitHub caps app names at 34 characters.
	short := strings.ToLower(name)
	if len(short) > 21 {
		short = strings.TrimRight(short[:21], "-_.")
	}
	manifest, _ := json.Marshal(map[string]any{
		"name":                "factory-" + short + "-" + randHex(2),
		"url":                 githubWeb + "/" + cfg.Repo,
		"redirect_url":        base + "/callback",
		"public":              false,
		"default_permissions": appPermissions,
		"default_events":      []string{},
		// The factory polls GitHub, so the webhook stays off.
		"hook_attributes": map[string]any{"url": githubWeb + "/" + cfg.Repo, "active": false},
	})

	var (
		mu  sync.Mutex
		got *github.CreatedApp
		bad error
	)
	exchange := func(code, st string) (*github.CreatedApp, error) {
		if st != state {
			return nil, errors.New("that address is from a different setup run")
		}
		app, err := github.ConvertManifest(ctx, githubAPI, code)
		if err == nil && !strings.EqualFold(app.Owner.Login, owner) {
			return nil, fmt.Errorf("GitHub made the app under %s, not %s; delete it from its settings page and try again", app.Owner.Login, owner)
		}
		return app, err
	}
	// Refuse requests that didn't come to the loopback address itself, so a
	// web page can't reach this server through DNS rebinding.
	local := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Host != ln.Addr().String() {
				http.Error(w, "wrong host", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", local(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = manifestPage.Execute(w, map[string]string{"Action": newURL + "?state=" + state, "Manifest": string(manifest), "Repo": cfg.Repo})
	}))
	mux.HandleFunc("GET /callback", local(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := got != nil
		mu.Unlock()
		if done {
			_, _ = io.WriteString(w, "<p>The app was already created. Go back to the terminal.</p>")
			return
		}
		app, err := exchange(r.URL.Query().Get("code"), r.URL.Query().Get("state"))
		mu.Lock()
		if err == nil && got == nil {
			got = app
		} else if got == nil {
			bad = err
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			fmt.Fprintf(w, "<p>Setup could not finish creating the app: %s</p><p>Go back to the terminal.</p>", template.HTMLEscapeString(err.Error()))
			return
		}
		_, _ = io.WriteString(w, "<p>App created. Go back to the terminal for the next step.</p>")
	}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	if browse != nil {
		browse(base)
	}

	p.say("Open this link in a browser on this machine and follow it:")
	p.say("\n    %s/\n", base)
	p.say("If this machine is a server you reach over SSH, first run this on your own")
	p.say("computer, then open the link there:\n\n    ssh -L %d:127.0.0.1:%d <this server>\n", ln.Addr().(*net.TCPAddr).Port, ln.Addr().(*net.TCPAddr).Port)
	p.say("GitHub shows a form for the new app with everything filled in. Click")
	p.say("\"Create GitHub App\" at the bottom. You don't need to change anything.")
	for {
		in := p.ask("Press Enter here once the browser says the app was created (or paste the browser's address if it showed an error)", "")
		mu.Lock()
		app, err := got, bad
		mu.Unlock()
		if app == nil && strings.Contains(in, "code=") {
			if u, perr := url.Parse(in); perr == nil {
				app, err = exchange(u.Query().Get("code"), u.Query().Get("state"))
			}
		}
		if app != nil {
			keyPath = filepath.Join(keyDir, "github-app-"+app.Slug+".pem")
			if err := writePrivate(keyPath, app.PEM); err != nil {
				return "", "", "", err
			}
			p.say("Created %s. Its private key is saved in %s.", app.HTMLURL, keyPath)
			return strconv.FormatInt(app.ID, 10), app.Slug, keyPath, nil
		}
		if err != nil {
			p.say("That didn't work: %v", err)
		} else {
			p.say("GitHub hasn't sent the app back yet.")
		}
		if !p.yes("Keep waiting?", true) {
			return "", "", "", errors.New("app not created")
		}
	}
}

// installApp waits for you to install the app on the repo's owner and
// returns the installation ID.
func installApp(ctx context.Context, p *prompter, cfg *config.Config, appID, slug, keyPath string) (string, error) {
	owner, _, _ := strings.Cut(cfg.Repo, "/")
	key, err := github.LoadAppKey(keyPath)
	if err != nil {
		return "", err
	}
	tokens := &github.AppTokens{AppID: appID, Key: key, Repo: cfg.Repo, BaseURL: githubAPI}
	if id, err := tokens.FindInstallation(ctx, owner); err == nil && id != "" {
		return id, nil
	}
	p.say("Now install the app on %s. Open:", cfg.Repo)
	p.say("\n    %s/apps/%s/installations/new\n", githubWeb, slug)
	p.say("Choose \"Only select repositories\", pick %s, and click Install.", cfg.Repo)
	for {
		p.ask("Press Enter here once it's installed", "")
		id, err := tokens.FindInstallation(ctx, owner)
		if err == nil && id != "" {
			return id, nil
		}
		if err != nil {
			p.say("Couldn't check: %v", err)
		} else {
			p.say("It isn't installed on %s yet.", owner)
		}
		if !p.yes("Check again?", true) {
			return "", errors.New("app not installed")
		}
	}
}

func writePrivate(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o600)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var manifestPage = template.Must(template.New("m").Parse(`<!doctype html>
<meta charset="utf-8"><title>Create the factory's GitHub App</title>
<body style="font-family:system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem">
<h1>Create the factory's GitHub App</h1>
<p>This sends you to GitHub with a new app for <b>{{.Repo}}</b> already filled in:
read and write access to code, issues and pull requests, read access to checks, and nothing else.</p>
<form id="f" action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<button type="submit" style="font-size:1.1rem;padding:.6rem 1.2rem">Continue to GitHub</button>
</form>
</body>`))
