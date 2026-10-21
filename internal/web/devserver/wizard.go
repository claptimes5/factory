package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/web"
)

// wizard stands in for the get-started page's backend. It starts part
// way through setup: this machine and Claude are done, GitHub, Codex and
// one job image are not. GitHub's app flow runs on the devserver itself
// (see fakeGitHub), so nothing leaves this machine.
type wizard struct {
	restart *restarter

	mu        sync.Mutex
	repos     []string
	app       bool // the app is made
	installed bool
	token     string
	claude    string
	codexMode string
	notify    string
	built     bool
	signedIn  bool // the fake sign-in never sets it; see fakeSignIn
	signIn    web.APIWizardSignIn
	building  bool
	log       string
	result    string
	labels    []web.APIWizardLabel
	labeled   bool // acme/web has its labels; until then two are missing
	pending   map[string]bool
	changed   bool
	// saved are the checks saved per repo: acme/api's test command is the
	// one its AGENTS.md disagrees with; acme/web has none yet.
	saved map[string]map[string]string
	tries map[string]*web.APIWizardTry
}

func newWizard(r *restarter) *wizard {
	return &wizard{restart: r, repos: []string{"acme/api", "acme/web"}, claude: "sk-ant-api03-devserverwxyz", codexMode: "chatgpt", pending: map[string]bool{},
		saved: map[string]map[string]string{"acme/api": {"lint": "bundle exec rubocop", "test": "bundle exec rspec"}}}
}

var labelNames = []string{"factory:ready", "factory:spec", "factory:parked", "factory:retry"}

func secret(v string) web.APIWizardSecret {
	if v == "" {
		return web.APIWizardSecret{}
	}
	return web.APIWizardSecret{Set: true, Masked: v[:4] + "…" + v[len(v)-4:]}
}

func (w *wizard) reach() bool { return (w.app && w.installed) || w.token != "" }

func (w *wizard) State(context.Context, bool) web.APIWizard {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := web.APIWizard{ConfigPath: "/srv/factory/factory.yaml", EnvPath: "/srv/factory/factory.env", Repos: w.repos,
		MachineCheckedAt: time.Now().UTC().Format(time.RFC3339), LabelNames: labelNames, Labels: w.labels, Warnings: []string{},
		Notify: secret(w.notify), Changed: w.changed,
		Machine: []web.APIWizardFinding{
			{Status: "pass", What: "git 2.50.1"},
			{Status: "pass", What: "Docker not needed (runner.kind: local)"},
			{Status: "warn", What: "ttyd isn't installed (optional: browser terminals into running jobs)", Fix: "sudo apt-get install ttyd"},
		}}
	if out.Labels == nil {
		out.Labels = []web.APIWizardLabel{}
	}
	step := func(key, title, state, summary string) {
		out.Steps = append(out.Steps, web.APIWizardStep{Key: key, Title: title, State: state, Summary: summary})
	}
	step("machine", "This machine", "done", "Everything the factory needs is here")
	step("repos", "Repositories", "done", strings.Join(w.repos, " and "))

	g := &web.APIWizardGitHub{Method: "none", Token: secret(w.token), TokenURL: "https://github.com/settings/personal-access-tokens/new",
		AppOwner: "acme", Missing: []web.APIWizardOwner{}, InstallSteps: []string{}}
	switch {
	case w.app:
		g.Method, g.AppID, g.AppSlug, g.KeyPath, g.Token = "app", "42", "factory-api-ab12", "/srv/factory/github-app-factory-api-ab12.pem", web.APIWizardSecret{}
		g.InstallURL = "https://github.com/apps/factory-api-ab12/installations/new"
		if w.installed {
			g.InstallationID = "55"
		} else {
			g.Missing = []web.APIWizardOwner{{Owner: "acme", Repos: w.repos}}
			g.InstallSteps = []string{`Pick acme, choose "Only select repositories", select ` + strings.Join(w.repos, " and ") + ", and click Install."}
		}
	case w.token != "":
		g.Method = "token"
	}
	out.GitHub = g
	for _, r := range w.repos {
		a := web.APIWizardAccess{Repo: r, Host: "github", State: "unchecked", Hints: []string{}, BaseBranch: "main", CanUseDefault: len(w.repos) == 1}
		a.MissingLabels = []string{}
		if w.reach() {
			a.State = "ok"
			if r == "acme/web" && !w.labeled {
				a.MissingLabels = []string{"factory:parked", "factory:retry"}
			}
		}
		out.Access = append(out.Access, a)
	}
	switch {
	case w.reach() && !w.labeled && len(w.repos) > 1:
		step("github", "GitHub", "attention", "Labels missing on acme/web")
	case w.reach():
		step("github", "GitHub", "done", "The factory can reach "+strings.Join(w.repos, " and "))
	case w.app:
		step("github", "GitHub", "attention", "Not checked yet")
	default:
		step("github", "GitHub", "attention", "Not connected yet")
	}
	out.CanFinish = w.reach()
	var unsaved []string
	for _, r := range w.repos {
		c := web.APIWizardChecks{Repo: r, Gates: []web.APIWizardGate{}, Notes: []string{}, Services: []web.APIWizardService{}, Env: map[string]string{}, Try: w.tries[r]}
		if r == "acme/api" {
			c.Services = []web.APIWizardService{{Name: "postgres", Image: "postgres:16"}}
			c.Env["DATABASE_URL"] = "postgres://postgres:factory@postgres:5432/test"
		}
		{
			c.Stack, c.Install, c.ImageFrom = "Ruby on Rails", "bundle install", "ruby:3.4-bookworm"
			sug := map[string][2]string{"lint": {"bundle exec rubocop", ""}, "test": {"bin/rails db:prepare test", "AGENTS.md"}}
			if r != "acme/api" {
				c.Stack, c.Install, c.ImageFrom = "React (Vite)", "npm ci", ""
				sug = map[string][2]string{"lint": {"npm run lint", ""}, "test": {"CI=true npm run test", ""}, "build": {"npm run build", ""}}
			}
			saved, ok := w.saved[r]
			c.Saved = ok
			for _, g := range []string{"lint", "test", "build"} {
				s, has := sug[g]
				if !has && saved[g] == "" {
					continue
				}
				row := web.APIWizardGate{Gate: g, Command: s[0], Source: s[1]}
				if ok {
					row.Command = saved[g]
					if s[0] != row.Command {
						row.Suggested = s[0]
					}
				}
				c.Gates = append(c.Gates, row)
			}
		}
		if !c.Saved {
			unsaved = append(unsaved, r)
		}
		out.Checks = append(out.Checks, c)
	}
	if len(unsaved) > 0 {
		step("checks", "Checks", "attention", "Confirm the checks for "+strings.Join(unsaved, " and "))
	} else {
		step("checks", "Checks", "done", "Every repo's checks are set")
	}

	c := &web.APIWizardClaude{Key: secret(w.claude)}
	switch {
	case strings.HasPrefix(w.claude, "sk-ant-oat"):
		c.Kind = "subscription"
		step("claude", "Claude", "done", "Subscription token saved")
	case w.claude != "":
		c.Kind = "api_key"
		step("claude", "Claude", "done", "API key saved")
	default:
		step("claude", "Claude", "attention", "No key yet")
	}
	out.Claude = c
	out.Codex = &web.APIWizardCodex{Mode: w.codexMode, SignedIn: w.signedIn, SignIn: w.signIn, ReviewOnly: true}
	switch {
	case w.codexMode == "chatgpt" && w.signedIn:
		step("codex", "OpenAI (Codex)", "done", "ChatGPT plan, signed in")
	case w.codexMode == "chatgpt":
		step("codex", "OpenAI (Codex)", "attention", "ChatGPT plan, not signed in yet")
		out.Warnings = append(out.Warnings, "Codex isn't signed in to ChatGPT, so Codex jobs wait until you sign in.")
	default:
		step("codex", "OpenAI (Codex)", "done", "Saved")
	}
	if w.notify != "" {
		step("notify", "Notifications", "done", "Saved")
	} else {
		step("notify", "Notifications", "optional", "Optional: get a message when something needs you")
	}
	im := &web.APIWizardImages{Building: w.building, Log: w.log, Result: w.result, Images: []web.APIWizardImage{
		{Tag: "factory-job:latest", For: "the orchestrator, tuning and grooming", Built: true},
		{Tag: "factory-job-node:latest", For: "acme/web", Built: w.built},
	}}
	out.Images = im
	switch {
	case w.building:
		step("images", "Job images", "attention", "Building…")
	case !w.built:
		step("images", "Job images", "attention", "1 image to build")
		out.Warnings = append(out.Warnings, "Jobs can't start until the job images are built.")
	default:
		step("images", "Job images", "done", "Built")
	}
	if out.CanFinish {
		step("finish", "Finish", "done", "Ready")
	} else {
		step("finish", "Finish", "attention", "Once the factory can reach every repo")
	}
	return out
}

func (w *wizard) Do(ctx context.Context, action string, in web.WizardInput) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch action {
	case "machine/check":
		return "Checked this machine again.", nil
	case "checks/try":
		// The tests need the database: without it they fail as a Rails
		// app's would.
		pg := slices.Contains(in.Services, "postgres") || in.Repo != "acme/api"
		t := &web.APIWizardTry{Since: time.Now().UTC().Format(time.RFC3339), Steps: []web.APIWizardTryStep{
			{Gate: "install", Command: in.Install, State: "pass", Seconds: 41, Output: "Bundle complete!"}}}
		for _, g := range []string{"lint", "test", "build"} {
			if c := strings.TrimSpace(in.Commands[g]); c != "" {
				st := web.APIWizardTryStep{Gate: g, Command: c, State: "pass", Seconds: 12, Output: "ok"}
				if g == "test" && !pg {
					st.State, st.Output = "fail", "ActiveRecord::ConnectionNotEstablished: connection to server at \"127.0.0.1\", port 5432 failed: Connection refused"
				}
				t.Steps = append(t.Steps, st)
			}
		}
		if w.tries == nil {
			w.tries = map[string]*web.APIWizardTry{}
		}
		w.tries[in.Repo] = t
		return "Trying " + in.Repo + "'s checks.", nil
	case "checks/save":
		cmds := map[string]string{}
		for g, c := range in.Commands {
			if c = strings.TrimSpace(c); c != "" {
				cmds[g] = c
			}
		}
		if len(cmds) == 0 {
			return "", errors.New("give at least one check: a change that runs none is checked by nothing")
		}
		w.saved[in.Repo], w.changed = cmds, true
		return "Saved " + in.Repo + "'s checks to factory.yaml.", nil
	case "repos":
		var names []string
		for _, f := range strings.FieldsFunc(in.Repos, func(r rune) bool { return r == ',' || r == ' ' }) {
			if o, n, ok := strings.Cut(f, "/"); !ok || o == "" || n == "" {
				return "", fmt.Errorf("write %q as owner/name, for example octocat/hello-world", f)
			}
			names = append(names, f)
		}
		if len(names) == 0 {
			return "", errors.New("name at least one repository, as owner/name")
		}
		w.repos, w.changed = names, true
		return "Saved " + strings.Join(names, " and ") + " to factory.yaml.", nil
	case "github/install-check":
		if !w.app {
			return "", errors.New("make or pick a GitHub App first")
		}
		w.installed, w.changed = true, true
		return "Installed. The factory can reach " + strings.Join(w.repos, " and ") + ".", nil
	case "github/existing-app":
		if strings.TrimSpace(in.AppID) == "" || strings.TrimSpace(in.KeyPath) == "" {
			return "", errors.New("give the app ID and the path of its key")
		}
		w.app, w.installed, w.token, w.changed = true, true, "", true
		return "Saved the app.", nil
	case "github/token":
		if strings.TrimSpace(in.Token) == "" {
			return "", errors.New("paste the token first")
		}
		if strings.HasPrefix(strings.TrimSpace(in.Token), "glpat-") {
			return "", errors.New("that's a GitLab token (glpat-…), not a GitHub one: add a GitLab project under Repositories, and paste it in the GitLab step")
		}
		w.token, w.app, w.installed, w.changed = strings.TrimSpace(in.Token), false, false, true
		return "Saved the token. The factory can reach " + strings.Join(w.repos, " and ") + ".", nil
	case "github/disconnect":
		w.token, w.app, w.installed, w.changed = "", false, false, true
		return "Removed GitHub's saved credentials from factory.env.", nil
	case "access/check":
		if !w.reach() {
			return "Connect the factory to " + strings.Join(w.repos, " and ") + " first.", nil
		}
		return "The factory can reach " + strings.Join(w.repos, " and ") + ".", nil
	case "base-branch":
		return "The factory now works on " + in.Branch + ".", nil
	case "labels":
		w.labels, w.labeled = nil, true
		for _, r := range w.repos {
			for _, n := range labelNames {
				res := "exists"
				if r == "acme/web" && (n == "factory:parked" || n == "factory:retry") {
					res = "created"
				}
				w.labels = append(w.labels, web.APIWizardLabel{Repo: r, Name: n, Result: res})
			}
		}
		return "Created labels; the rest were already there.", nil
	case "claude":
		k := strings.TrimSpace(in.Key)
		switch {
		case k == "":
			return "", errors.New("paste an API key or a subscription token first")
		case !strings.HasPrefix(k, "sk-ant-") && !in.Anyway:
			return "", errors.New("that doesn't look like an Anthropic key (they start with sk-ant-); check it, or save it anyway")
		case len(k) < 12:
			k += strings.Repeat("x", 12-len(k))
		}
		w.claude, w.changed = k, true
		return "Saved the key.", nil
	case "codex":
		switch in.Mode {
		case "api-key", "access-token":
			w.codexMode, w.changed = in.Mode, true
			return "Saved.", nil
		case "chatgpt":
			w.codexMode, w.changed = in.Mode, true
			return "Saved. Now sign in with ChatGPT.", nil
		}
		return "", errors.New("pick an API key, a ChatGPT plan or an access token")
	case "notify":
		u := strings.TrimSpace(in.URL)
		if u != "" && !strings.Contains(u, "ntfy") && !strings.Contains(u, "hooks.slack.com") && !strings.Contains(u, "discord.com/api/webhooks") {
			return "", errors.New("that isn't an ntfy, Slack or Discord address")
		}
		w.notify, w.changed = u, true
		if u == "" {
			return "Notifications are off.", nil
		}
		return "Saved. Send a test to check it works.", nil
	case "notify/test":
		if w.notify == "" {
			return "", errors.New("save a notification address first")
		}
		return "Sent. Check your phone or channel.", nil
	case "codex/sign-in":
		if w.signIn.Running {
			return "", errors.New("a sign-in is already running: open the link and enter the code")
		}
		w.signIn = web.APIWizardSignIn{Running: true, URL: "https://auth.openai.com/codex/device", Code: "DEVS-12345",
			Log: "Follow these steps to sign in with ChatGPT using device code authorization:\n\n1. Open this link in your browser and sign in to your account\n   https://auth.openai.com/codex/device\n\n2. Enter this one-time code (expires in 15 minutes)\n   DEVS-12345\n"}
		go w.fakeSignIn()
		return "Started. Open the link and enter the code that appear here.", nil
	case "codex/sign-in/cancel":
		if !w.signIn.Running {
			return "", errors.New("no sign-in is running")
		}
		w.signIn = web.APIWizardSignIn{Result: "Sign-in cancelled."}
		return "Sign-in cancelled.", nil
	case "images/build":
		if w.building {
			return "", errors.New("a build is already running")
		}
		w.building, w.log, w.result = true, "Building factory-job-node:latest for acme/web...\n", ""
		go w.fakeBuild()
		return "Building. It takes a few minutes per image.", nil
	case "finish":
		if !w.reach() {
			return "", errors.New("the factory can't reach every repo yet")
		}
		if n := w.restart.RequestRestart(ctx); n > 0 {
			return "Restarting once the running job finishes. No new jobs start until then.", nil
		}
		return "Restarting now to apply the changes.", nil
	}
	return "", fmt.Errorf("the devserver can't %s", action)
}

// fakeSignIn finishes the sign-in as if someone entered the code. Codex
// stays not signed in, so every browser test starts from the same page.
func (w *wizard) fakeSignIn() {
	time.Sleep(2 * time.Second)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.signIn.Running { // not cancelled
		w.signIn.Running, w.signIn.Result = false, "Signed in. Codex uses your ChatGPT plan."
	}
}

// fakeBuild writes a few lines of build output, then says it's built.
func (w *wizard) fakeBuild() {
	for _, l := range []string{"Step 1/9 : FROM node:22-bookworm", "Step 2/9 : RUN apt-get update", "Successfully tagged factory-job-node:latest"} {
		time.Sleep(400 * time.Millisecond)
		w.mu.Lock()
		w.log += l + "\n"
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.building, w.built, w.result = false, true, "The job images are built."
	w.mu.Unlock()
}

func (w *wizard) StartGitHubApp(_ context.Context, callback string) (web.APIWizardManifest, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	w.pending[state] = true
	origin := strings.TrimSuffix(callback, "/api/v1/setup/github/callback")
	return web.APIWizardManifest{URL: origin + "/fake-github/settings/apps/new?state=" + state, State: state,
		Manifest: `{"name":"factory-api-ab12","redirect_url":"` + callback + `"}`}, nil
}

func (w *wizard) FinishGitHubApp(_ context.Context, code, state string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pending[state] {
		return "", errors.New(`that link is from another or an earlier setup; start again with "Create an app for me"`)
	}
	delete(w.pending, state)
	if code == "" {
		return "", errors.New("GitHub sent no code back")
	}
	w.app, w.installed, w.token, w.changed = true, false, "", true
	return "Created the app factory-api-ab12. Its key is saved in /srv/factory/github-app-factory-api-ab12.pem. Now install it.", nil
}

// fakeGitHub is GitHub's new-app form: it takes the posted manifest and
// sends the browser straight back, as GitHub does once you confirm.
func fakeGitHub(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("manifest") == "" {
		http.Error(w, "no manifest posted", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/api/v1/setup/github/callback?code=devserver-code&state="+r.URL.Query().Get("state"), http.StatusSeeOther)
}
