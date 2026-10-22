// Package web is the control room: server-rendered HTML with htmx, embedded
// in the binary. It binds to localhost by default; reach it remotely over
// Tailscale or an SSH tunnel. With FACTORY_WEB_PASSWORD set it asks for a
// password (see Login); without one it refuses to listen beyond those.
package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/compare"
	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/push"
	"github.com/claptimes5/factory/internal/stack"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

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
	// IssueURL and PRURL link to an issue or pull (merge) request on its
	// repo's host; they default to GitHub's.
	IssueURL func(repo string, n int) string
	PRURL    func(repo string, n int) string
	DailyCap float64
	// DiskLow says why jobs aren't starting for lack of disk (may be nil).
	DiskLow func() string
	// ImageWait says an item on repo waits for its job image to build,
	// and why; "" when it doesn't (may be nil).
	ImageWait func(repo string) string
	// SetupPark is the setup problem a parked item stopped on, and Detect
	// reads a repo's files for what its setup should be: with both, Fix
	// setup offers the change (may be nil).
	SetupPark func(ctx context.Context, it *store.WorkItem) (pipeline.Park, bool)
	Detect    func(ctx context.Context, repo string) (*stack.Suggestion, error)
	AwaitCap  int
	Attach    func(ctx context.Context, jobID int64) (string, error) // returns a URL or a command
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
	tmpl       *template.Template
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
	funcs := template.FuncMap{
		"money": func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"pct": func(v float64) string {
			if v < 0 {
				return "–"
			}
			return fmt.Sprintf("%.0f%%", v*100)
		},
		"hours": func(v float64) string { return fmt.Sprintf("%.1f h", v) },
		"rounds": func(v float64) string {
			if v < 0 {
				return "–"
			}
			return fmt.Sprintf("%.1f", v)
		},
		"short": func(s string, n int) string {
			if len(s) > n {
				return s[:n] + "…"
			}
			return s
		},
		"since": func(ts string) string {
			t, err := time.Parse(store.TimeFormat, ts)
			if err != nil {
				return ts
			}
			return time.Since(t).Round(time.Second).String() + " ago"
		},
		// ref names an issue: #12, or owner/name#12 when there are several repos.
		"ref":      func(repo string, n int) string { return s.ref(repo, n) },
		"issueURL": s.issueURL,
		"prURL":    s.prURL,
		"pretty": func(raw string) string {
			var v any
			if json.Unmarshal([]byte(raw), &v) != nil {
				return raw
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
	}
	t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
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
	static, err := staticFS()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.needsYou)
	mux.HandleFunc("GET /board", s.board)
	mux.HandleFunc("GET /items/{id}", s.item)
	mux.HandleFunc("GET /jobs/{id}", s.job)
	mux.HandleFunc("GET /jobs/{id}/stream", s.stream)
	mux.HandleFunc("POST /jobs/{id}/retry", s.retry)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /jobs/{id}/attach", s.attach)
	mux.HandleFunc("POST /items/{id}/park", s.park)
	mux.HandleFunc("GET /orchestrator", s.orchestrator)
	mux.HandleFunc("GET /spend", s.spend)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /compare", s.compareList)
	mux.HandleFunc("GET /compare/{id}", s.compareView)
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

func staticFS() (fs.FS, error) { return fs.Sub(assets, "static") }

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

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data["Page"] = name
	data["SignedIn"] = s.Login != nil && s.Login.Password != ""
	if err := s.tmpl.ExecuteTemplate(w, name+".html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fail(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func idParam(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }

// --- pages ---

type parkedRow struct {
	Item   *store.WorkItem
	Reason string
}

func (s *Server) needsYou(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	waiting, err := s.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		fail(w, err)
		return
	}
	parkedItems, err := s.Store.ListItems(ctx, state.Parked)
	if err != nil {
		fail(w, err)
		return
	}
	var parked []parkedRow
	for _, it := range parkedItems {
		reason := ""
		if evs, err := s.Store.Events(ctx, it.ID); err == nil {
			for i := len(evs) - 1; i >= 0; i-- {
				if evs[i].Type == "stage" && strings.Contains(evs[i].Data, `"to":"parked"`) {
					reason = evs[i].Reason
					break
				}
			}
		}
		parked = append(parked, parkedRow{it, reason})
	}
	spent, _ := s.Store.SpendOn(ctx, store.Today())
	running, _ := s.Store.ListJobs(ctx, store.JobFilter{State: state.Running})
	s.render(w, "home", map[string]any{
		"Waiting": waiting, "Parked": parked, "Spent": spent, "Cap": s.DailyCap,
		"OverBudget": spent >= s.DailyCap, "NearBudget": spent >= 0.8*s.DailyCap,
		"AwaitFull": len(waiting) >= s.AwaitCap, "AwaitCap": s.AwaitCap, "Running": running,
	})
}

var boardStages = []state.Stage{state.Epic, state.Triage, state.Spec, state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Parked, state.Blocked, state.Done}

type boardCard struct {
	Item  *store.WorkItem
	Model string
	Cost  float64
	Live  bool
}

type boardCol struct {
	Stage state.Stage
	Cards []boardCard
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := s.Store.ListItems(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	cols := map[state.Stage]*boardCol{}
	for _, st := range boardStages {
		cols[st] = &boardCol{Stage: st}
	}
	for _, it := range items {
		jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		c := boardCard{Item: it}
		for _, j := range jobs {
			c.Cost += j.CostUSD
			if c.Model == "" && j.Kind == "implement" {
				c.Model = j.Model
			}
			c.Live = c.Live || j.State == state.Running
		}
		if col, ok := cols[it.Stage]; ok {
			col.Cards = append(col.Cards, c)
		}
	}
	var out []*boardCol
	for _, st := range boardStages {
		out = append(out, cols[st])
	}
	s.render(w, "board", map[string]any{"Cols": out})
}

func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: id})
	evs, _ := s.Store.Events(ctx, id)
	deps, _ := s.Store.Dependencies(ctx, id)
	s.render(w, "item", map[string]any{"Item": it, "Jobs": jobs, "Events": evs, "Deps": deps})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.Store.GetJob(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	gr, _ := s.Store.GateResults(ctx, id)
	s.render(w, "job", map[string]any{"Job": j, "Gates": gr, "Live": j.State == state.Running, "Msg": r.URL.Query().Get("msg")})
}

// stream tails the job's log file over SSE until the job ends.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.tailLog(w, r, id, func(line string) string {
		return fmt.Sprintf("event: line\ndata: <div class=\"l\">%s</div>\n\n", template.HTMLEscapeString(summarize(line)))
	}, func(st state.JobState) string {
		return fmt.Sprintf("event: done\ndata: <em>job %s</em>\n\n", st)
	})
}

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

func (s *Server) back(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?msg="+urlEscape(msg), http.StatusSeeOther)
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "+", "&", "%26", "#", "%23", "?", "%3F").Replace(s)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	msg := "Retry queued."
	if err := s.Actions.Retry(r.Context(), id, r.FormValue("tier") == "up"); err != nil {
		msg = "Retry refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), msg)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	msg := "Cancelled."
	if err := s.Actions.Cancel(r.Context(), id); err != nil {
		msg = "Cancel refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), msg)
}

func (s *Server) park(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "Parked from the control room."
	}
	msg := "Parked."
	if err := s.Actions.Park(r.Context(), id, reason); err != nil {
		msg = "Park refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/items/%d", id), msg)
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.Attach == nil {
		s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Attach is not available with this runner.")
		return
	}
	target, err := s.Attach(r.Context(), id)
	if err != nil {
		s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Attach failed: "+err.Error())
		return
	}
	if strings.HasPrefix(target, "http") {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Run this on the host: "+target)
}

func (s *Server) orchestrator(w http.ResponseWriter, r *http.Request) {
	evs, err := s.Store.EventsByActor(r.Context(), "orchestrator", 300)
	if err != nil {
		fail(w, err)
		return
	}
	s.render(w, "orchestrator", map[string]any{"Events": evs})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	rep, err := metrics.Compute(r.Context(), s.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "metrics", map[string]any{"R": rep})
}

type compareRun struct {
	ID     string
	Report compare.Report
}

func (s *Server) compareRuns() []compareRun {
	var runs []compareRun
	ents, _ := os.ReadDir(s.CompareDir)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(s.CompareDir, e.Name(), "report.json"))
		if err != nil {
			continue
		}
		var r compare.Report
		if json.Unmarshal(b, &r) == nil {
			runs = append(runs, compareRun{e.Name(), r})
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Report.Started.After(runs[j].Report.Started) })
	return runs
}

func (s *Server) compareList(w http.ResponseWriter, r *http.Request) {
	s.render(w, "compare", map[string]any{"Runs": s.compareRuns()})
}

func (s *Server) compareView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.CompareDir == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		http.NotFound(w, r)
		return
	}
	md, err := os.ReadFile(filepath.Join(s.CompareDir, id, "report.md"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "compare_view", map[string]any{"ID": id, "Report": string(md)})
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

type daySpend struct {
	Day  string
	Cost float64
}

func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var days []daySpend
	var week float64
	for i := 0; i < 7; i++ {
		d := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		c, _ := s.Store.SpendOn(ctx, d)
		week += c
		days = append(days, daySpend{d, c})
	}
	done, _ := s.Store.ListItems(ctx, state.Done)
	var doneCost float64
	byModel := map[string]float64{}
	for _, it := range done {
		jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		for _, j := range jobs {
			doneCost += j.CostUSD
			byModel[j.Model] += j.CostUSD
		}
	}
	perPR := 0.0
	if len(done) > 0 {
		perPR = doneCost / float64(len(done))
	}
	type mc struct {
		Model string
		Cost  float64
	}
	var models []mc
	for m, c := range byModel {
		models = append(models, mc{m, c})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })
	s.render(w, "spend", map[string]any{"Days": days, "Week": week, "Merged": len(done), "PerPR": perPR, "Models": models, "Cap": s.DailyCap})
}

// TTYDAttach returns an Attach func that opens a one-shot browser terminal
// into a running job's container with ttyd, when ttyd is installed, and
// otherwise returns the docker exec command to run by hand.
func TTYDAttach(st *store.Store, host string) func(ctx context.Context, jobID int64) (string, error) {
	return func(ctx context.Context, jobID int64) (string, error) {
		j, err := st.GetJob(ctx, jobID)
		if err != nil {
			return "", err
		}
		if j.State != state.Running {
			return "", fmt.Errorf("job %d is %s, not running", jobID, j.State)
		}
		name := fmt.Sprintf("factory-job-%d", jobID)
		manual := "docker exec -it " + name + " sh"
		if _, err := exec.LookPath("ttyd"); err != nil {
			return manual, nil
		}
		// ttyd has no login of its own: only where the control room needs
		// none may it listen. Elsewhere the in-page terminal does instead.
		if NeedsLogin(net.JoinHostPort(host, "0")) {
			return manual, nil
		}
		l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return manual, nil
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		cmd := exec.Command("ttyd", "--once", "--writable", "-i", host, "-p", strconv.Itoa(port), "docker", "exec", "-it", name, "sh")
		if err := cmd.Start(); err != nil {
			return manual, nil
		}
		go func() { _ = cmd.Wait() }()
		time.Sleep(300 * time.Millisecond)
		return fmt.Sprintf("http://%s/", net.JoinHostPort(host, strconv.Itoa(port))), nil
	}
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
