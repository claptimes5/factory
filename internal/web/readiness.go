package web

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// The setup screen: how ready each repo is for the factory, from the
// readiness check's latest report, with each blocker's issue and the
// factory's work on it.

// APIReadiness is every repo's readiness.
type APIReadiness struct {
	Repos []APIRepoReadiness `json:"repos"`
	// Banner is what to show above every screen: "not_ready" while a
	// checked repo has blockers or its check couldn't finish, "ready" once
	// every checked repo is ready and the factory has worked on nothing
	// but their setup, else "".
	Banner      string `json:"banner"`
	IntakeLabel string `json:"intake_label"`
	// CanCheck says whether this request may start a check here (the
	// password, or a request from this machine, and a daemon that can run
	// one); CheckNote says why not. CanFile says it may also file issues.
	CanCheck  bool   `json:"can_check"`
	CheckNote string `json:"check_note"`
	CanFile   bool   `json:"can_file"`
}

// APIRepoReadiness is one repo's latest readiness report.
type APIRepoReadiness struct {
	Repo string `json:"repo"`
	// State is unchecked, incomplete (the check couldn't finish),
	// not_ready (it has blockers) or ready.
	State     string `json:"state"`
	CheckedAt string `json:"checked_at"`
	Branch    string `json:"branch"`
	SHA       string `json:"sha"`
	Score     int    `json:"score"`
	// Done of Total setup steps pass.
	Done   int                   `json:"done"`
	Total  int                   `json:"total"`
	Steps  []APIReadinessStep    `json:"steps"`
	Extras []APIReadinessFinding `json:"extras"`
	// Unqueued counts issues filed but not labeled for the factory, and
	// Unfiled the findings that need an issue and have none.
	Unqueued int `json:"unqueued"`
	Unfiled  int `json:"unfiled"`
	// CanQueue says filing with the intake label would do something: file
	// what isn't filed, or label what waited for the blockers.
	CanQueue bool `json:"can_queue"`
	// Checking: a run started here is going, since CheckingSince. Action
	// is what it does: check, file, or queue (file and start work).
	Checking      bool   `json:"checking"`
	CheckingSince string `json:"checking_since"`
	Action        string `json:"action"`
	// CheckError is why the last run started here failed, or "", and
	// Result what it did when it worked.
	CheckError string `json:"check_error"`
	Result     string `json:"result"`
}

// APIReadinessStep is one setup step.
type APIReadinessStep struct {
	Key      string                `json:"key"`
	Title    string                `json:"title"`
	Why      string                `json:"why"`
	State    string                `json:"state"` // done, failing, unchecked
	Passed   string                `json:"passed"`
	Blockers []APIReadinessFinding `json:"blockers"`
}

// APIReadinessFinding is one thing the check found, with the issue filed
// for it and the factory's item for that issue.
type APIReadinessFinding struct {
	Level    string `json:"level"` // blocker, gap, tip, good
	Area     string `json:"area"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Issue    int    `json:"issue"` // 0 when none is filed
	IssueURL string `json:"issue_url"`
	// Status is the work on it: none (nothing to file: it's yours, or in
	// place), not_filed, not_queued (filed without the intake label),
	// closed, waiting, building, needs_you, split (into SplitTotal
	// issues, SplitDone of them done) or done.
	Status     string `json:"status"`
	SplitDone  int    `json:"split_done"`
	SplitTotal int    `json:"split_total"`
	ItemID     *int64 `json:"item_id"`
	ItemStage  string `json:"item_stage"`
}

// readinessRun is the check (or check and file) started from the
// dashboard, one at a time: each clones a repo and runs its install and
// checks.
type readinessRun struct {
	mu      sync.Mutex
	repo    string
	action  string
	since   time.Time
	errs    map[string]string // repo -> why its last run failed
	results map[string]string // repo -> what its last run did
}

func (s *Server) routesReadiness(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/readiness", s.apiReadiness)
	mux.HandleFunc("POST /api/v1/readiness/check", s.apiCheckReadiness)
	mux.HandleFunc("POST /api/v1/readiness/file", s.apiFileReadiness)
}

func (s *Server) apiReadiness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.readiness(r))
}

func (s *Server) readiness(r *http.Request) APIReadiness {
	ctx := r.Context()
	out := APIReadiness{Repos: []APIRepoReadiness{}, IntakeLabel: s.IntakeLabel}
	if out.IntakeLabel == "" {
		out.IntakeLabel = "factory:ready"
	}
	out.CanCheck, out.CheckNote = s.canCheckReadiness(r)
	out.CanFile = out.CanCheck && s.FileReadiness != nil
	s.ready.mu.Lock()
	running, action, since := s.ready.repo, s.ready.action, s.ready.since
	errs, results := maps.Clone(s.ready.errs), maps.Clone(s.ready.results)
	s.ready.mu.Unlock()
	linked := map[int64]bool{}
	checked, ready := 0, 0
	for _, repo := range s.Repos {
		rr := APIRepoReadiness{Repo: repo, State: "unchecked", Steps: []APIReadinessStep{}, Extras: []APIReadinessFinding{},
			CheckError: errs[repo], Result: results[repo]}
		if strings.EqualFold(running, repo) {
			rr.Checking, rr.CheckingSince, rr.Action = true, since.UTC().Format(store.TimeFormat), action
		}
		rep, err := readiness.Load(s.ReadinessDir, repo)
		if err != nil && rr.CheckError == "" {
			rr.CheckError = "Couldn't read its last report: " + err.Error()
		}
		if s.ReadinessDir == "" || rep == nil {
			out.Repos = append(out.Repos, rr)
			continue
		}
		checked++
		rr.CheckedAt, rr.Branch, rr.SHA, rr.Score = rep.Started.UTC().Format(store.TimeFormat), rep.Branch, rep.SHA, rep.Score
		switch {
		case rep.Incomplete:
			rr.State = "incomplete"
		case !rep.Ready:
			rr.State = "not_ready"
		default:
			rr.State = "ready"
			ready++
		}
		for _, n := range rep.Filed {
			if it, err := s.Store.ItemByIssue(ctx, repo, n); err == nil {
				linked[it.ID] = true
			}
		}
		rr.Done, rr.Total = rep.Progress()
		find := func(f readiness.Finding) APIReadinessFinding {
			a := s.readinessFinding(ctx, repo, f, linked)
			if a.Status == "not_filed" {
				rr.Unfiled++
			}
			if a.Status == "not_queued" {
				rr.Unqueued++
			}
			return a
		}
		for _, st := range rep.Steps() {
			as := APIReadinessStep{Key: st.Key, Title: st.Title, Why: st.Why, State: st.State, Passed: st.Passed, Blockers: []APIReadinessFinding{}}
			for _, f := range st.Blockers {
				as.Blockers = append(as.Blockers, find(f))
			}
			rr.Steps = append(rr.Steps, as)
		}
		for _, f := range rep.Extras() {
			rr.Extras = append(rr.Extras, find(f))
		}
		// Queueing files what isn't filed, and once the repo is ready labels
		// what was filed for later. An incomplete check files nothing.
		if rep.Incomplete {
			rr.Unfiled = 0
		}
		rr.CanQueue = !rep.Incomplete && (rr.Unfiled > 0 || rep.Ready && rr.Unqueued > 0)
		out.Repos = append(out.Repos, rr)
	}
	switch {
	case checked == 0:
	case ready < checked:
		out.Banner = "not_ready"
	case !s.workedBeyondSetup(ctx, linked):
		out.Banner = "ready"
	}
	return out
}

// readinessFinding links a finding to its issue and the factory's item
// for it, adding the item to linked.
func (s *Server) readinessFinding(ctx context.Context, repo string, f readiness.Finding, linked map[int64]bool) APIReadinessFinding {
	a := APIReadinessFinding{Level: string(f.Level), Area: f.Area, Title: f.Title, Detail: f.Detail, Status: "none"}
	if f.Issue == nil {
		return a
	}
	if f.Issue.Number == 0 {
		a.Status = "not_filed"
		return a
	}
	a.Issue, a.IssueURL = f.Issue.Number, s.issueURL(repo, f.Issue.Number)
	it, err := s.Store.ItemByIssue(ctx, repo, f.Issue.Number)
	if err != nil {
		a.Status = "not_queued"
		if f.Issue.State == "closed" {
			a.Status = "closed"
		}
		return a
	}
	linked[it.ID] = true
	a.ItemID, a.ItemStage = &it.ID, string(it.Stage)
	switch item := s.apiItem(ctx, it); {
	case it.Stage == state.Done:
		a.Status = "done"
	case it.Stage == state.Epic:
		a.Status = "split"
		for _, c := range s.children(ctx, it.ID) {
			a.SplitTotal++
			if c.Stage == state.Done {
				a.SplitDone++
			}
		}
	case item.NeedsYou:
		a.Status = "needs_you"
	case it.Stage == state.Implementing || it.Stage == state.Reviewing:
		a.Status = "building"
	default:
		a.Status = "waiting"
	}
	return a
}

// workedBeyondSetup says whether the factory has an item that isn't for
// one of the issues the readiness check filed: once it has, nobody needs
// telling how to start.
func (s *Server) workedBeyondSetup(ctx context.Context, linked map[int64]bool) bool {
	items, err := s.Store.ListItems(ctx)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(items, func(it *store.WorkItem) bool { return !linked[it.ID] })
}

func (s *Server) canCheckReadiness(r *http.Request) (bool, string) {
	if ok, _ := s.settingsWritable(r); !ok {
		return false, "Checking from here needs a dashboard password: set FACTORY_WEB_PASSWORD (see docs/security.md)."
	}
	if s.CheckReadiness == nil || s.ReadinessDir == "" {
		return false, "This dashboard can't run the check."
	}
	return true, ""
}

// apiCheckReadiness runs the readiness check on a repo again, in the
// background. It runs the repo's install and checks, as a build's gates
// do, and files nothing. It takes the daemon's time, so like settings it
// needs the password, or a request from this machine.
func (s *Server) apiCheckReadiness(w http.ResponseWriter, r *http.Request) {
	s.startReadiness(w, r, false, func(ctx context.Context, repo string, _ bool) (string, error) {
		return "", s.CheckReadiness(ctx, repo)
	})
}

// apiFileReadiness checks a repo again, then files an issue for each gap
// that has none. With queue they get the intake label (only the blockers
// while it has any), and once it is ready the issues filed earlier get it
// too, so the factory starts on them.
func (s *Server) apiFileReadiness(w http.ResponseWriter, r *http.Request) {
	if s.FileReadiness == nil {
		apiErr(w, http.StatusForbidden, "This dashboard can't file issues.")
		return
	}
	s.startReadiness(w, r, true, s.FileReadiness)
}

// startReadiness starts run on the repo in the request, one run at a time.
func (s *Server) startReadiness(w http.ResponseWriter, r *http.Request, files bool, run func(ctx context.Context, repo string, queue bool) (string, error)) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.canCheckReadiness(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	var in struct {
		Repo  string `json:"repo"`
		Queue bool   `json:"queue"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	i := slices.IndexFunc(s.Repos, func(x string) bool { return strings.EqualFold(x, in.Repo) })
	if i < 0 {
		apiErr(w, http.StatusBadRequest, "send {\"repo\": \"owner/name\"}, one of the repos the factory works on")
		return
	}
	repo := s.Repos[i]
	action, msg := "check", "Checking "+repo+". It takes a few minutes, and this page updates when it's done."
	if files {
		action, msg = "file", "Checking "+repo+" again, then filing its issues. It takes a few minutes, and this page updates when it's done."
		if in.Queue {
			action = "queue"
		}
	}
	s.ready.mu.Lock()
	if s.ready.repo != "" {
		busy := s.ready.repo
		s.ready.mu.Unlock()
		writeJSON(w, http.StatusConflict, APIActionResult{Message: "Already checking " + busy + ". One check runs at a time; try again when it's done."})
		return
	}
	s.ready.repo, s.ready.action, s.ready.since = repo, action, time.Now()
	if s.ready.errs == nil {
		s.ready.errs, s.ready.results = map[string]string{}, map[string]string{}
	}
	delete(s.ready.errs, repo)
	delete(s.ready.results, repo)
	s.ready.mu.Unlock()
	go func() {
		res, err := run(context.WithoutCancel(r.Context()), repo, in.Queue)
		s.ready.mu.Lock()
		defer s.ready.mu.Unlock()
		s.ready.repo, s.ready.action = "", ""
		if err != nil {
			s.ready.errs[repo] = err.Error()
		} else if res != "" {
			s.ready.results[repo] = res
		}
	}()
	s.act(w, nil, msg)
}
