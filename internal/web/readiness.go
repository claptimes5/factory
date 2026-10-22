package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/pipeline"
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
	// one); CheckNote says why not.
	CanCheck  bool   `json:"can_check"`
	CheckNote string `json:"check_note"`
}

// APIImageBuild is a build of a repo's job image going now: why it's
// built, since when, and the step it's on ("" before the first).
type APIImageBuild struct {
	Why   string `json:"why"`
	Since string `json:"since"`
	Step  string `json:"step"`
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
	Done  int                `json:"done"`
	Total int                `json:"total"`
	Steps []APIReadinessStep `json:"steps"`
	// Work are the blockers the factory fixes, each filed as an issue:
	// until they're merged, new tasks on the repo wait.
	Work   []APIReadinessFinding `json:"work"`
	Extras []APIReadinessFinding `json:"extras"`
	// Checking: a check of the repo is queued or running (a readiness
	// job, CheckJob), since CheckingSince. CheckingWhy says what started
	// it when the factory did by itself ("Started by itself: master moved
	// from 1a2b3c4 to 5d6e7f8."), or "".
	Checking      bool   `json:"checking"`
	CheckingSince string `json:"checking_since"`
	CheckingWhy   string `json:"checking_why"`
	CheckJob      *int64 `json:"check_job"`
	// CheckWaits is what a check that's queued, not running, waits for
	// ("its job image to build"), or "": then CheckingSince is when it
	// was queued.
	CheckWaits string `json:"check_waits"`
	// Image is the build of its job image going now, or null.
	Image *APIImageBuild `json:"image"`
	// CheckError is why the last check failed, or "", and Result what it
	// did when it worked.
	CheckError string `json:"check_error"`
	Result     string `json:"result"`
	// Progress is what the check going (or the last one) has done so far,
	// a step at a time, oldest first.
	Progress []APIProgress `json:"progress"`
}

// APIProgress is one step of a setup run: "running `bundle install`",
// "passed `bundle install` in 2m14s", and when it was said.
type APIProgress struct {
	At   string `json:"at"`
	Text string `json:"text"`
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
	// Key names what it's about across checks ("check:lint", "flaky"),
	// or "" for a finding with nothing to file.
	Key      string `json:"key"`
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

// readinessFollow is what FollowReadiness keeps between looks.
type readinessFollow struct {
	mu       sync.Mutex
	nudge    chan struct{} // a page asked: follow now, not at the next tick
	nudgedAt time.Time     // when a page last did, to ask the host at most so often
}

func (s *Server) routesReadiness(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/readiness", s.apiReadiness)
	mux.HandleFunc("POST /api/v1/readiness/check", s.apiCheckReadiness)
}

func (s *Server) apiReadiness(w http.ResponseWriter, r *http.Request) {
	s.nudgeFollow()
	writeJSON(w, http.StatusOK, s.readiness(r))
}

// followNudge is how often a page being looked at can have the host asked
// about each repo's base branch and baseline.
const followNudge = 30 * time.Second

// nudgeFollow has FollowReadiness look now, as someone is looking: a
// baseline merged since shows as checked again soon, not at the next tick.
func (s *Server) nudgeFollow() {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if s.ready.nudge == nil || time.Since(s.ready.nudgedAt) < followNudge {
		return
	}
	s.ready.nudgedAt = time.Now()
	select {
	case s.ready.nudge <- struct{}{}:
	default:
	}
}

// lastCheck fills in rr from the latest readiness job for its repo, of
// checks (newest first): going, or how the last one ended.
func (s *Server) lastCheck(rr *APIRepoReadiness, checks []*store.Job) {
	for _, j := range checks {
		var in pipeline.ReadinessInput
		if json.Unmarshal([]byte(j.Input), &in) != nil || !strings.EqualFold(in.Repo, rr.Repo) {
			continue
		}
		dir := filepath.Join(s.JobsDir, strconv.FormatInt(j.ID, 10))
		if s.JobsDir != "" {
			for _, l := range pipeline.ReadProgress(dir) {
				rr.Progress = append(rr.Progress, APIProgress{At: l.At, Text: l.Text})
			}
		}
		id := j.ID
		rr.CheckJob = &id
		switch {
		case !j.State.Terminal():
			rr.Checking, rr.CheckingSince = true, j.StartedAt
			if j.State == state.Queued {
				rr.CheckingSince, rr.CheckWaits = j.QueuedAt, "a free slot"
				if rr.Image != nil {
					rr.CheckWaits = "its job image to build"
				}
			}
			if in.Why != "" {
				rr.CheckingWhy = "Started by itself: " + in.Why + "."
			}
		case j.State == state.Succeeded:
			if s.JobsDir != "" {
				rr.Result = pipeline.ReadResult(dir)
			}
			if in.Why != "" && rr.Result != "" {
				rr.Result = "Checked again by itself: " + in.Why + ". " + rr.Result
			}
		case j.State == state.Failed:
			rr.CheckError = "The last check failed: " + j.Error
		}
		return
	}
}

func (s *Server) readiness(r *http.Request) APIReadiness {
	ctx := r.Context()
	out := APIReadiness{Repos: []APIRepoReadiness{}, IntakeLabel: s.IntakeLabel}
	if out.IntakeLabel == "" {
		out.IntakeLabel = "factory:ready"
	}
	out.CanCheck, out.CheckNote = s.canCheckReadiness(r)
	checks, _ := s.Store.ListJobs(ctx, store.JobFilter{Kind: pipeline.KindReadiness, Limit: 100})
	linked := map[int64]bool{}
	checked, ready := 0, 0
	for _, repo := range s.Repos {
		rr := APIRepoReadiness{Repo: repo, State: "unchecked", Steps: []APIReadinessStep{}, Work: []APIReadinessFinding{}, Extras: []APIReadinessFinding{}, Progress: []APIProgress{}}
		if s.ImageBuilding != nil {
			rr.Image = s.ImageBuilding(repo)
		}
		s.lastCheck(&rr, checks)
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
		find := func(f readiness.Finding) APIReadinessFinding { return s.readinessFinding(ctx, repo, f, linked) }
		for _, st := range rep.Steps() {
			as := APIReadinessStep{Key: st.Key, Title: st.Title, Why: st.Why, State: st.State, Passed: st.Passed, Blockers: []APIReadinessFinding{}}
			for _, f := range st.Blockers {
				as.Blockers = append(as.Blockers, find(f))
			}
			rr.Steps = append(rr.Steps, as)
		}
		for _, f := range rep.Work() {
			rr.Work = append(rr.Work, find(f))
		}
		for _, f := range rep.Extras() {
			rr.Extras = append(rr.Extras, find(f))
		}
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
	a.Key = f.Issue.Key
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
	// Split work is done when its pieces are, whatever became of the
	// issue it was split from (splitting can close it).
	kids := s.children(ctx, it.ID)
	switch item := s.apiItem(ctx, it); {
	case len(kids) > 0 || it.Stage == state.Epic:
		a.Status = "split"
		for _, c := range kids {
			linked[c.ID] = true
			a.SplitTotal++
			if c.Stage == state.Done {
				a.SplitDone++
			}
		}
	case it.Stage == state.Done:
		a.Status = "done"
	case item.NeedsYou:
		a.Status = "needs_you"
	case it.Stage == state.Implementing || it.Stage == state.Reviewing:
		a.Status = "building"
	default:
		a.Status = "waiting"
	}
	return a
}

// setupOf says how item it stands to its repo's setup (see
// APIItem.Setup), and why.
func (s *Server) setupOf(ctx context.Context, it *store.WorkItem) (string, string) {
	if s.ReadinessDir == "" || it.Stage == state.Done {
		return "", ""
	}
	r, _ := readiness.Load(s.ReadinessDir, it.Repo)
	if it.Stage == state.Parked {
		if red := s.redOnBase(ctx, r, it.ID); len(red) > 0 {
			return "base", fmt.Sprintf("Waits for %s: it failed %s, which fails on %s too, so no change passes until that's fixed there. Retry it then.",
				r.Branch, strings.Join(red, " and "), r.Branch)
		}
	}
	if r == nil || r.Ready || r.Incomplete {
		return "", ""
	}
	parent := 0
	if it.ParentID != nil {
		if p, err := s.Store.GetItem(ctx, *it.ParentID); err == nil {
			parent = p.IssueNumber
		}
	}
	f := r.Fixes(it.IssueNumber, parent)
	if f == nil {
		return "held", "Waits for setup: every job on " + it.Repo + " would fail until its setup steps pass, so it starts by itself once they do."
	}
	return "blocks", "Fixes a setup blocker (" + strings.ReplaceAll(f.Title, "`", "") + "). Nothing else runs on " + it.Repo + " until the blockers are fixed."
}

// redOnBase are the checks item id's last build failed that fail on the
// base branch too, as report r found.
func (s *Server) redOnBase(ctx context.Context, r *readiness.Report, id int64) []string {
	if r == nil {
		return nil
	}
	jobs, err := s.Store.ListJobs(ctx, store.JobFilter{ItemID: id, Kind: "implement"})
	if err != nil {
		return nil
	}
	for _, j := range jobs { // newest first
		if j.State == state.Failed {
			var out []string
			for _, g := range r.RedOnBase(j.Error) {
				out = append(out, "`"+g+"`")
			}
			return out
		}
	}
	return nil
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
	if s.QueueReadiness == nil || s.ReadinessDir == "" {
		return false, "This dashboard can't run the check."
	}
	return true, ""
}

// apiCheckReadiness queues a readiness check of a repo: a job the daemon
// runs, which installs the repo and runs its checks, as a build's gates
// do, then files what a change can fix for the factory. It takes the
// daemon's time, so like settings it needs the password, or a request
// from this machine.
func (s *Server) apiCheckReadiness(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.canCheckReadiness(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	var in struct {
		Repo string `json:"repo"`
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
	queued, err := s.QueueReadiness(r.Context(), repo, "")
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !queued {
		writeJSON(w, http.StatusConflict, APIActionResult{Message: "A check of " + repo + " is already queued or running."})
		return
	}
	s.act(w, nil, "Checking "+repo+". It takes a few minutes, and this page updates as it goes.")
}

// FollowReadiness checks each repo that isn't ready again whenever its
// base branch moves (a fix merged, by the factory or a person), every
// interval until ctx ends. A repo someone chose File and start work for
// is filed and queued as well, so once its setup steps pass the factory
// starts on the rest by itself.
func (s *Server) FollowReadiness(ctx context.Context, every time.Duration) {
	if s.BaseSHA == nil || s.QueueReadiness == nil || s.ReadinessDir == "" {
		return
	}
	nudge := make(chan struct{}, 1)
	s.ready.mu.Lock()
	s.ready.nudge = nudge
	s.ready.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	// Once at the start too: a baseline merged while the factory was
	// stopped doesn't wait for the base branch to move again.
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-nudge:
			}
		}
		for _, repo := range s.Repos {
			s.followOnce(ctx, repo)
		}
	}
}

func (s *Server) followOnce(ctx context.Context, repo string) {
	rep, err := readiness.Load(s.ReadinessDir, repo)
	// An incomplete check is the factory's setup, not the repo: a new
	// commit wouldn't change it.
	if err != nil || rep == nil || rep.Ready || rep.Incomplete {
		return
	}
	sha, err := s.BaseSHA(ctx, repo)
	if err != nil || sha == "" || sha == rep.SHA {
		return
	}
	short := func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	}
	// Queued once: a check already queued or running is the same news.
	if _, err := s.QueueReadiness(ctx, repo, fmt.Sprintf("%s moved from %s to %s", rep.Branch, short(rep.SHA), short(sha))); err != nil {
		slog.Warn("readiness", "repo", repo, "queue", err)
	}
}
