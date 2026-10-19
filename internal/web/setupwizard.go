package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The get-started page walks through what `factory setup` asks in the
// terminal: this machine, the repos, forge access, model keys,
// notifications and job images. It is a separate thing from the setup
// screen (readiness.go), which says how ready each repo is.
//
// While the factory can't start for want of forge access, `factory serve`
// runs the dashboard with only this page (SetupMode) until Finish.

// Wizard does the get-started page's work. The page never gets a saved
// secret back: only whether it is set, and a masked hint of it.
type Wizard interface {
	// State is where each step stands, from what was saved and the last
	// checks. With live it first checks the forges again (access, the
	// app's installations, labels) unless that was done moments ago;
	// without, it asks no host.
	State(ctx context.Context, live bool) APIWizard
	// Do runs one of WizardActions and says what it did. An error is the
	// reason the input was refused, said for the person.
	Do(ctx context.Context, action string, in WizardInput) (string, error)
	// StartGitHubApp prepares a new GitHub App for GitHub's manifest
	// flow; GitHub sends the browser back to callback.
	StartGitHubApp(ctx context.Context, callback string) (APIWizardManifest, error)
	// FinishGitHubApp exchanges the code GitHub sent back for the app,
	// if state is one StartGitHubApp handed out and not yet used.
	FinishGitHubApp(ctx context.Context, code, state string) (string, error)
}

// WizardActions are the page's buttons, each POST /api/v1/setup/<action>.
var WizardActions = []string{
	"machine/check", "repos", "github/install-check", "github/existing-app", "github/token",
	"access/check", "base-branch", "labels", "gitlab/token", "claude", "codex",
	"codex/sign-in", "codex/sign-in/cancel", "notify", "notify/test", "sentry", "images/build", "finish",
}

// WizardInput is what an action takes; each uses only its own fields.
type WizardInput struct {
	Repos   string `json:"repos"`    // repos: owner/name or GitLab project URLs, comma separated
	AppID   string `json:"app_id"`   // github/existing-app
	KeyPath string `json:"key_path"` // github/existing-app: the .pem on the factory's machine
	Token   string `json:"token"`    // github/token, gitlab/token, sentry
	Repo    string `json:"repo"`     // base-branch
	Branch  string `json:"branch"`   // base-branch
	Key     string `json:"key"`      // claude; codex's API key or access token
	Anyway  bool   `json:"anyway"`   // claude: save a key that doesn't look like one
	Mode    string `json:"mode"`     // codex: api-key, chatgpt or access-token
	URL     string `json:"url"`      // notify
}

// APIWizard is the get-started page.
type APIWizard struct {
	// Mode is "setup" while the dashboard runs only this page, until
	// Finish starts the factory; else "normal".
	Mode string `json:"mode"`
	// Writable says whether this request may save here (see
	// settingsWritable); Reason says why not.
	Writable   bool   `json:"writable"`
	Reason     string `json:"reason"`
	ConfigPath string `json:"config_path"`
	EnvPath    string `json:"env_path"`
	// Steps are the cards in order, with where each stands.
	Steps []APIWizardStep `json:"steps"`
	// Machine is what the doctor found on this machine.
	Machine          []APIWizardFinding `json:"machine"`
	MachineCheckedAt string             `json:"machine_checked_at"`
	Repos            []string           `json:"repos"`
	// Access is each repo's last access check.
	Access []APIWizardAccess `json:"access"`
	// Labels are what the last "create labels" did; LabelNames the labels
	// the factory uses.
	Labels     []APIWizardLabel `json:"labels"`
	LabelNames []string         `json:"label_names"`
	GitHub     *APIWizardGitHub `json:"github"`
	GitLab     *APIWizardGitLab `json:"gitlab"`
	Claude     *APIWizardClaude `json:"claude"`
	Codex      *APIWizardCodex  `json:"codex"`
	Notify     APIWizardSecret  `json:"notify"`
	Sentry     *APIWizardSecret `json:"sentry"`
	Images     *APIWizardImages `json:"images"`
	// CanFinish once the factory can reach every repo; Warnings say what
	// won't work yet if you finish now.
	CanFinish bool     `json:"can_finish"`
	Warnings  []string `json:"warnings"`
	// Changed: factory.yaml or factory.env changed since the factory
	// started, so Finish restarts it to apply them.
	Changed        bool `json:"changed"`
	RestartPending bool `json:"restart_pending"`
	// SetupReason is why the factory can't start, in setup mode; else "".
	// StartedAt is when this process started, so the page can tell a
	// restarted one from the one it asked to restart.
	SetupReason string `json:"setup_reason"`
	StartedAt   string `json:"started_at"`
}

// APIWizardStep is one card.
type APIWizardStep struct {
	Key     string `json:"key"` // machine, repos, github, gitlab, claude, codex, notify, sentry, images, finish
	Title   string `json:"title"`
	State   string `json:"state"` // done, attention or optional
	Summary string `json:"summary"`
}

// APIWizardFinding is one line of the doctor's checklist.
type APIWizardFinding struct {
	Status string `json:"status"` // pass, warn or fail
	What   string `json:"what"`
	Fix    string `json:"fix"`
	// Action is a button that fixes it, in place of Fix: build_images, or
	// "" for none.
	Action string `json:"action"`
}

// APIWizardSecret is a saved secret as the page may see it.
type APIWizardSecret struct {
	Set    bool   `json:"set"`
	Masked string `json:"masked"`
}

// APIWizardAccess is whether the factory can reach one repo.
type APIWizardAccess struct {
	Repo  string `json:"repo"`
	Host  string `json:"host"`  // github or gitlab
	State string `json:"state"` // unchecked, ok or failed
	Error string `json:"error"`
	// Hints say what to try when it failed.
	Hints []string `json:"hints"`
	// BaseBranch is the branch factory.yaml uses; DefaultBranch the
	// repo's default on its host when they differ, else "".
	BaseBranch    string `json:"base_branch"`
	DefaultBranch string `json:"default_branch"`
	// CanUseDefault: one repo, so its base_branch can be set here.
	CanUseDefault bool `json:"can_use_default"`
	// MissingLabels are the factory's labels the repo lacks, from the last
	// check; LabelsError says why they couldn't be listed.
	MissingLabels []string `json:"missing_labels"`
	LabelsError   string   `json:"labels_error"`
}

// APIWizardLabel is what creating one label on one repo did.
type APIWizardLabel struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Result string `json:"result"` // created, exists or failed
	Error  string `json:"error"`
}

// APIWizardOwner is an account the GitHub App isn't installed on.
type APIWizardOwner struct {
	Owner string   `json:"owner"`
	Repos []string `json:"repos"`
}

// APIWizardGitHub is how the factory reaches GitHub.
type APIWizardGitHub struct {
	Method         string `json:"method"` // app, token or none
	AppID          string `json:"app_id"`
	AppSlug        string `json:"app_slug"`
	KeyPath        string `json:"key_path"`
	InstallationID string `json:"installation_id"`
	// InstallURL installs the app ("" when its slug isn't known).
	InstallURL string `json:"install_url"`
	// Missing are the accounts the last check found the app isn't
	// installed on; InstallSteps say how to install it there.
	Missing      []APIWizardOwner `json:"missing"`
	InstallSteps []string         `json:"install_steps"`
	InstallError string           `json:"install_error"`
	Token        APIWizardSecret  `json:"token"`
	// TokenURL is where to make a personal access token.
	TokenURL string `json:"token_url"`
	// AppOwner is the account a new app belongs to.
	AppOwner string `json:"app_owner"`
}

// APIWizardGitLab is how the factory reaches GitLab.
type APIWizardGitLab struct {
	Token APIWizardSecret `json:"token"`
	Hosts []APIWizardHost `json:"hosts"`
	// FineGrained are the permissions a fine-grained token needs on each
	// project.
	FineGrained []string `json:"fine_grained"`
}

// APIWizardHost is where to make a GitLab token on one host.
type APIWizardHost struct {
	Host string `json:"host"`
	// LegacyURL opens the form for a legacy token with its scopes ticked;
	// TokensURL is the page fine-grained tokens are made on.
	LegacyURL string `json:"legacy_url"`
	TokensURL string `json:"tokens_url"`
}

// APIWizardClaude is Claude's saved credential.
type APIWizardClaude struct {
	Kind string          `json:"kind"` // api_key, subscription or ""
	Key  APIWizardSecret `json:"key"`
}

// APIWizardCodex is how Codex signs in.
type APIWizardCodex struct {
	Mode        string          `json:"mode"` // api-key, chatgpt or access-token
	APIKey      APIWizardSecret `json:"api_key"`
	AccessToken APIWizardSecret `json:"access_token"`
	// SignedIn: the ChatGPT sign-in is saved. SignIn is the one started
	// from the page (codex/sign-in), running or last.
	SignedIn bool            `json:"signed_in"`
	SignIn   APIWizardSignIn `json:"sign_in"`
	// ReviewOnly: Claude writes the code and Codex reviews it.
	ReviewOnly bool `json:"review_only"`
}

// APIWizardSignIn is a ChatGPT sign-in with a device code: Codex shows a
// link and a code, and finishes once someone enters the code there.
type APIWizardSignIn struct {
	Running bool   `json:"running"`
	URL     string `json:"url"`  // the link to open, once Codex shows it
	Code    string `json:"code"` // the code to enter there
	// Log is the end of Codex's output; Result says how it ended ("" before
	// one ran).
	Log    string `json:"log"`
	Result string `json:"result"`
	Failed bool   `json:"failed"`
}

// APIWizardImages are the job images and their build.
type APIWizardImages struct {
	Images   []APIWizardImage `json:"images"`
	Building bool             `json:"building"`
	// Log is the end of the running or last build's output; Result says
	// how it ended ("" before one ran).
	Log    string `json:"log"`
	Result string `json:"result"`
	Failed bool   `json:"failed"`
}

// APIWizardImage is one image jobs run in.
type APIWizardImage struct {
	Tag   string `json:"tag"`
	For   string `json:"for"`
	Built bool   `json:"built"`
}

// APIWizardManifest is what the page posts to GitHub to make the app.
type APIWizardManifest struct {
	URL      string `json:"url"` // GitHub's new-app form, with state
	State    string `json:"state"`
	Manifest string `json:"manifest"`
}

// APIWizardResult is an action's message and the page after it.
type APIWizardResult struct {
	Message string    `json:"message"`
	Wizard  APIWizard `json:"wizard"`
}

const getStarted = "/app/get-started"

func (s *Server) routesWizard(mux *http.ServeMux) {
	if s.Wizard == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/setup", s.apiWizard)
	mux.HandleFunc("POST /api/v1/setup/github/app", s.apiWizardApp)
	mux.HandleFunc("GET /api/v1/setup/github/callback", s.apiWizardCallback)
	for _, a := range WizardActions {
		mux.HandleFunc("POST /api/v1/setup/"+a, func(w http.ResponseWriter, r *http.Request) { s.apiWizardDo(w, r, a) })
	}
}

// wizardState is the page as this request may see it.
func (s *Server) wizardState(r *http.Request) APIWizard {
	out := s.Wizard.State(r.Context(), r.URL.Query().Get("live") == "1")
	out.Mode = "normal"
	if s.SetupMode {
		out.Mode = "setup"
	}
	out.Writable, out.Reason = s.settingsWritable(r)
	// The sign-in's link and code would let a viewer sign the factory in
	// to their own account: only those who may change settings see them.
	if out.Codex != nil && !out.Writable {
		out.Codex.SignIn.URL, out.Codex.SignIn.Code, out.Codex.SignIn.Log = "", "", ""
	}
	if s.SetupMode {
		out.SetupReason = s.SetupReason
	}
	out.StartedAt = processStart
	if s.Restarter != nil {
		out.RestartPending = s.Restarter.RestartPending()
	}
	return out
}

func (s *Server) apiWizard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.wizardState(r))
}

// wizardWrite checks a POST may change settings: it is JSON, and comes
// with the password or from this machine, like the Settings screen.
func (s *Server) wizardWrite(w http.ResponseWriter, r *http.Request) bool {
	if !jsonOnly(w, r) {
		return false
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return false
	}
	return true
}

func (s *Server) apiWizardDo(w http.ResponseWriter, r *http.Request, action string) {
	if !s.wizardWrite(w, r) {
		return
	}
	var in WizardInput
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	msg, err := s.Wizard.Do(r.Context(), action, in)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APIWizardResult{Message: msg, Wizard: s.wizardState(r)})
}

func (s *Server) apiWizardApp(w http.ResponseWriter, r *http.Request) {
	if !s.wizardWrite(w, r) {
		return
	}
	m, err := s.Wizard.StartGitHubApp(r.Context(), requestOrigin(r)+"/api/v1/setup/github/callback")
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// apiWizardCallback is where GitHub sends the browser once the app is
// made. The state it carries proves the page that started it could save
// settings, so it is the one check here; then back to the page.
func (s *Server) apiWizardCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msg, err := s.Wizard.FinishGitHubApp(r.Context(), q.Get("code"), q.Get("state"))
	v := url.Values{}
	if err != nil {
		v.Set("github_error", err.Error())
	} else {
		v.Set("github", msg)
	}
	http.Redirect(w, r, getStarted+"?"+v.Encode(), http.StatusSeeOther)
}

// requestOrigin is the scheme and host the browser used, which guard has
// checked is one this server answers to.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// processStart is when this process started.
var processStart = time.Now().UTC().Format(time.RFC3339Nano)

// errSetupMode is what every other endpoint answers until setup finishes.
var errSetupMode = errors.New("the factory isn't set up yet: finish setup at " + getStarted)

// setupHandler serves only the get-started page and what it needs, for
// a factory that can't start yet.
func (s *Server) setupHandler() (*http.ServeMux, error) {
	mux := http.NewServeMux()
	static, err := staticFS()
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, getStarted, http.StatusSeeOther)
	})
	mux.HandleFunc("GET /api/v1/openapi.yaml", s.apiSpec)
	s.routesWizard(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		apiErr(w, http.StatusServiceUnavailable, errSetupMode.Error())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, errSetupMode.Error(), http.StatusServiceUnavailable)
	})
	if err := s.routesApp(mux); err != nil {
		return nil, err
	}
	return mux, nil
}
