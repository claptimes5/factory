// Package web is the control room: the dashboard's API and the React app,
// embedded in the binary. It binds to localhost by default; reach it remotely over
// Tailscale or an SSH tunnel. With FACTORY_WEB_PASSWORD set it asks for a
// password (see Login); without one it refuses to listen beyond those.
package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/push"
	"github.com/claptimes5/factory/internal/stack"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Actions are the buttons: every one goes through the daemon, which checks
// it like any other request.
type Actions interface {
	Retry(ctx context.Context, jobID int64, tierUp bool) error
	Cancel(ctx context.Context, jobID int64) error
	Park(ctx context.Context, itemID int64, reason string) error
	// ApproveEstimate lets an item whose cost estimate or plan waits go
	// on: one OK covers both. planJob is the plan job the page showed (0
	// for none); a plan that changed since is refused.
	ApproveEstimate(ctx context.Context, itemID, planJob int64) error
	// Replan sends the waiting plan planJob back to the builder with what
	// to change, as a comment and factory:replan on the issue do.
	Replan(ctx context.Context, itemID, planJob int64, ask string) error
	// ResetAutoMerge turns a repo's tripped auto-merge breaker off.
	ResetAutoMerge(ctx context.Context, repo, by string) error
	// Continue picks up a build that stopped on its turn or dollar cap in
	// the checkout and session it kept, with extra turns and budget (0.5
	// is half as much again) and a note to the builder.
	Continue(ctx context.Context, jobID int64, extra float64, note string) error
	// Steer sends a running build a message it reads at its next step.
	Steer(ctx context.Context, jobID int64, text string) error
}

// Server serves the control room.
type Server struct {
	Store   *store.Store
	Actions Actions
	Repos   []string // owner/name of each repo the factory works on
	// Workspaces are the factory's workspaces, in order, each with its
	// repos (nil: each repo is its own, as with none in factory.yaml).
	Workspaces []Workspace
	// IssueURL and PRURL link to an issue or pull (merge) request on its
	// repo's host; they default to GitHub's.
	IssueURL func(repo string, n int) string
	PRURL    func(repo string, n int) string
	DailyCap float64
	// DiskLow says why jobs aren't starting for lack of disk (may be nil).
	DiskLow func() string
	// ImageBuilding is the build of repo's job image going now, or nil
	// (may be nil): its jobs, its check included, wait for it.
	ImageBuilding func(repo string) *APIImageBuild
	// SetupPark is the setup problem a parked item stopped on, and Detect
	// reads a repo's files for what its setup should be: with both, Fix
	// setup offers the change (may be nil).
	SetupPark func(ctx context.Context, it *store.WorkItem) (pipeline.Park, bool)
	Detect    func(ctx context.Context, repo string) (*stack.Suggestion, error)
	AwaitCap  int
	// CompareDir holds `factory compare` reports, one directory per run.
	CompareDir string
	// Hosts are extra Host header values to accept (a Tailscale name or
	// address); localhost addresses are always accepted.
	Hosts []string
	// JobsDir is data/jobs, where review jobs save the diff they saw.
	JobsDir string
	// Issue and Checks read GitHub for the dashboard (either may be nil).
	Issue  IssueFunc
	Checks CheckFunc
	// Exec builds the command a live terminal runs for a running job
	// (nil when the runner has no containers to attach to).
	Exec ExecFunc
	// Login, with a password set, makes everyone sign in first.
	Login *Login
	// ConfigPath is the factory.yaml the Settings screen edits ("" for
	// none). Listen is the control room's address: settings change from
	// the browser only behind a password or on a loopback address.
	ConfigPath string
	Listen     string
	// StartedConfig is the ConfigVersion of the file the daemon read,
	// so the screen can say when saved changes wait for a restart.
	StartedConfig string
	// Restarter applies saved settings by restarting (may be nil).
	Restarter Restarter
	// Push sends Web Push notifications (nil: the factory can't).
	Push *push.Sender
	// SetTrust gives a repo its own auto_merge and self_start in
	// factory.yaml, for a trust level; self_start nil removes it (may be
	// nil: then trust can't be changed here).
	SetTrust func(ctx context.Context, repo string, a *config.AutoMerge, s *config.SelfStart) error
	// Breakers reads each repo's auto-merge breaker (may be nil).
	Breakers func(ctx context.Context) []APIBreaker
	// Choices are what a person can do next with a job that ended:
	// continue, retry, tier up, park, each explained (may be nil).
	Choices func(ctx context.Context, jobID int64) []APIChoice
	// SteerUnsupported says why a job can't take messages while it runs,
	// or "" (may be nil: then none can).
	SteerUnsupported func(j *store.Job) string
	// ReadinessDir holds the readiness check's latest report on each repo
	// ("" for none), and QueueReadiness queues a check of one, a job the
	// daemon runs, unless one is queued or running (nil when the
	// dashboard can't). IntakeLabel is the label that hands an issue to
	// the factory.
	ReadinessDir   string
	QueueReadiness func(ctx context.Context, repo, why string) (bool, error)
	// Harnesses are the harnesses with a key or sign-in, which a preset may
	// route to (nil when the dashboard can't tell: then any).
	Harnesses func() []string
	// BaseSHA is a repo's base branch head on its host, for
	// FollowReadiness to see when it moves.
	BaseSHA     func(ctx context.Context, repo string) (string, error)
	IntakeLabel string
	// Plans are the harnesses in use that run on a subscription plan,
	// whose usage windows matter more than dollars, and PauseAtUsage the
	// percent of a window that holds their jobs.
	Plans []string
	// PlansOnly: every harness in use runs on a plan, so no job costs
	// dollars and the dashboard shows plan usage, not spend.
	PlansOnly    bool
	PauseAtUsage float64
	// Chat is the chat with the orchestrator's model (nil for none), and
	// Orchestrator what the dashboard says about the orchestrator.
	Chat         Chatter
	Orchestrator OrchestratorInfo
	// Wizard runs the get-started page (nil for none). SetupMode serves
	// that page alone, for a factory that can't start until it's done;
	// SetupReason says why it can't.
	Wizard      Wizard
	SetupMode   bool
	SetupReason string
	// Uninstaller lists and removes what the factory made, for Settings'
	// "Remove the factory" (nil hides it).
	Uninstaller Uninstaller

	ready      readinessFollow
	pollPeriod time.Duration
	settingsMu sync.Mutex
}

func (s *Server) ref(repo string, n int) string {
	if len(s.Repos) > 1 && repo != "" {
		return fmt.Sprintf("%s#%d", repo, n)
	}
	return fmt.Sprintf("#%d", n)
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	if s.pollPeriod == 0 {
		s.pollPeriod = 500 * time.Millisecond
	}
	if s.SetupMode && s.Wizard != nil {
		mux, err := s.setupHandler()
		if err != nil {
			return nil, err
		}
		if s.Login != nil && s.Login.Password != "" {
			return s.guard(s.Login.wrap(mux)), nil
		}
		return s.guard(mux), nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /metrics.json", s.metricsJSON)
	s.routesAPI(mux)
	s.routesReadiness(mux)
	s.routesUsage(mux)
	s.routesChat(mux)
	s.routesWizard(mux)
	s.routesUninstall(mux)
	if err := s.routesApp(mux); err != nil {
		return nil, err
	}
	if s.Login != nil && s.Login.Password != "" {
		return s.guard(s.Login.wrap(mux)), nil
	}
	return s.guard(mux), nil
}

// guard refuses requests whose Host is not one the control room serves,
// so a DNS-rebinding page cannot reach it under its own name, and
// cross-site POSTs, so a page in your browser cannot press its buttons.
func (s *Server) guard(next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range s.Hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowed[strings.ToLower(strings.Trim(host, "[]"))] {
			http.Error(w, "unknown host "+r.Host+"; add it to web.hosts", http.StatusMisdirectedRequest)
			return
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func idParam(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }

// tailLog follows a job's log file, writing line(l) for each complete line
// and done(state) once the job has ended.
func (s *Server) tailLog(w http.ResponseWriter, r *http.Request, id int64, line func(string) string, done func(state.JobState) string) {
	ctx := r.Context()
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var f *os.File
	var rd *bufio.Reader
	for {
		j, err := s.Store.GetJob(ctx, id)
		if err != nil {
			return
		}
		if f == nil && j.LogPath != "" {
			if f, err = os.Open(j.LogPath); err == nil {
				defer f.Close()
				rd = bufio.NewReaderSize(f, 1<<20)
			} else {
				f = nil
			}
		}
		if rd != nil {
			for {
				ln, err := rd.ReadString('\n')
				if err == io.EOF {
					if ln != "" {
						// partial line: rewind so it is re-read when complete
						_, _ = f.Seek(-int64(len(ln)), io.SeekCurrent)
						rd.Reset(f)
					}
					break
				}
				if err != nil {
					return
				}
				_, _ = io.WriteString(w, line(ln))
			}
			fl.Flush()
		}
		if j.State.Terminal() {
			_, _ = io.WriteString(w, done(j.State))
			fl.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollPeriod):
		}
	}
}

// summarize turns one raw CLI JSON line into a short human line.
func summarize(line string) string {
	var v struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &v) != nil {
		return strings.TrimSpace(line)
	}
	var parts []string
	for _, c := range v.Message.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "tool_use":
			parts = append(parts, "→ "+c.Name+" "+trim(string(c.Input), 160))
		}
	}
	switch {
	case v.Type == "result":
		parts = append(parts, "result ("+v.Subtype+"): "+trim(v.Result, 400))
	case v.Item.Type == "agent_message":
		parts = append(parts, v.Item.Text)
	case v.Item.Type == "command_execution":
		parts = append(parts, "→ "+v.Item.Command)
	}
	if len(parts) == 0 {
		return v.Type
	}
	return trim(strings.Join(parts, " "), 2000)
}

// trim keeps a line to about n bytes, cutting between characters.
func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func (s *Server) metricsJSON(w http.ResponseWriter, r *http.Request) {
	rep, err := metrics.Compute(r.Context(), s.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}

func (s *Server) issueURL(repo string, n int) string {
	if s.IssueURL != nil {
		return s.IssueURL(repo, n)
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)
}

func (s *Server) prURL(repo string, n int) string {
	if s.PRURL != nil {
		return s.PRURL(repo, n)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
}

func (s *Server) diskLow() string {
	if s.DiskLow == nil {
		return ""
	}
	return s.DiskLow()
}
