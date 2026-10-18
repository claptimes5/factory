package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// The JSON API behind the dashboard. Every type here is described in
// openapi.yaml, which the frontend generates its types from; the test in
// api_test.go fails when the two drift.

// APIItem is a work item.
type APIItem struct {
	ID         int64  `json:"id"`
	Repo       string `json:"repo"` // owner/name
	Ref        string `json:"ref"`  // how to show the issue: #12, or owner/name#12 with several repos
	Issue      int    `json:"issue"`
	Title      string `json:"title"`
	Stage      string `json:"stage"`
	Complexity string `json:"complexity"`
	Risk       string `json:"risk"`
	Priority   int    `json:"priority"`
	ParentID   *int64 `json:"parent_id"`
	Branch     string `json:"branch"`
	PRNumber   int    `json:"pr_number"`
	// PRs are a workspace change's PRs, one per repo, in merge order.
	PRs         []APILinkedPR `json:"prs"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
	IssueURL    string        `json:"issue_url"`
	PRURL       string        `json:"pr_url"`
	NeedsYou    bool          `json:"needs_you"`
	BlockedNote string        `json:"blocked_note"`
}

// APILinkedPR is one PR of a change that spans a workspace's repos.
type APILinkedPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Ref    string `json:"ref"` // web#12
	URL    string `json:"url"`
}

// APICard is an item on the board, with its live status.
type APICard struct {
	APIItem
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
	Live    bool    `json:"live"`
	LiveJob *int64  `json:"live_job"`
	// The running job: its kind, when it started, and what its log says
	// it is doing now ("editing parser/parser.go").
	LiveKind  string `json:"live_kind"`
	LiveSince string `json:"live_since"`
	Activity  string `json:"activity"`
	// The latest implement attempt, and how its gates went.
	Attempt     int `json:"attempt"`
	GatesPassed int `json:"gates_passed"`
	GatesTotal  int `json:"gates_total"`
}

// APIColumn is one board column.
type APIColumn struct {
	Stage string    `json:"stage"`
	Cards []APICard `json:"cards"`
}

// APIBoard is the whole board.
type APIBoard struct {
	Columns []APIColumn `json:"columns"`
}

// APIOverview is the header strip shown on every screen.
type APIOverview struct {
	Repo          string   `json:"repo"`  // the first repo, kept for older clients
	Repos         []string `json:"repos"` // every repo the factory works on
	SpentTodayUSD float64  `json:"spent_today_usd"`
	DailyCapUSD   float64  `json:"daily_cap_usd"`
	Awaiting      int      `json:"awaiting"`
	AwaitingCap   int      `json:"awaiting_cap"`
	Running       int      `json:"running"`
	Queued        int      `json:"queued"`
	Version       string   `json:"version"`
	// DiskLow says why no job is starting for lack of disk, or "".
	DiskLow string `json:"disk_low"`
	// SignedIn is true when the control room asks for a password, so the
	// app offers to sign out.
	SignedIn bool `json:"signed_in"`
	// MainHealth is each repo's base branch CI, as main health last read it.
	MainHealth []APIMainHealth `json:"main_health"`
}

// APIMainHealth is one repo's base branch CI.
type APIMainHealth struct {
	Repo   string           `json:"repo"`
	Branch string           `json:"branch"`
	State  string           `json:"state"` // green, red, flaky, pending, no_ci or unreadable
	SHA    string           `json:"sha"`
	Detail string           `json:"detail"`
	Checks []APIHealthCheck `json:"checks"`
}

// APIHealthCheck is a red or flaky check and the issue filed for it.
type APIHealthCheck struct {
	Check    string `json:"check"`
	Kind     string `json:"kind"`
	Issue    int    `json:"issue"` // 0 until filed
	IssueURL string `json:"issue_url"`
}

// APIJob is one job.
type APIJob struct {
	ID         int64   `json:"id"`
	ItemID     *int64  `json:"item_id"`
	Kind       string  `json:"kind"`
	State      string  `json:"state"`
	Harness    string  `json:"harness"`
	Model      string  `json:"model"`
	Effort     string  `json:"effort"`
	Attempt    int     `json:"attempt"`
	CostUSD    float64 `json:"cost_usd"`
	BudgetUSD  float64 `json:"budget_usd"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	Verdict    string  `json:"verdict"`
	Error      string  `json:"error"`
	QueuedAt   string  `json:"queued_at"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
	Branch     string  `json:"branch"`
	Skillset   string  `json:"skillset"`
	HeadSHA    string  `json:"head_sha"`
}

// APIGate is one gate result.
type APIGate struct {
	Gate   string `json:"gate"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// APIEvent is one audit-trail entry.
type APIEvent struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	ItemID *int64 `json:"item_id"`
	JobID  *int64 `json:"job_id"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Data   string `json:"data"`
}

// APIItemDetail is everything about one item.
type APIItemDetail struct {
	Item      APIItem    `json:"item"`
	Jobs      []APIJob   `json:"jobs"`
	Events    []APIEvent `json:"events"`
	DependsOn []int64    `json:"depends_on"`
	// IssueBody is the issue text from GitHub ("" when it cannot be read).
	IssueBody string `json:"issue_body"`
	// Spec is the triage stage's summary of what to build.
	Spec string `json:"spec"`
}

// APIJobDetail is one job with its gates.
type APIJobDetail struct {
	Job   APIJob    `json:"job"`
	Gates []APIGate `json:"gates"`
	// CanAttach says whether a live terminal can be opened.
	CanAttach bool `json:"can_attach"`
}

// APIFinding is one reviewer finding.
type APIFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
}

// APIReview is the latest reviewer verdict on an item.
type APIReview struct {
	JobID       int64        `json:"job_id"`
	Decision    string       `json:"decision"`
	Findings    []APIFinding `json:"findings"`
	Assumptions []string     `json:"challenged_assumptions"`
}

// APICheck is one CI check run on GitHub.
type APICheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// APIPRPanel is the PR panel: CI, gates and the reviewer's verdict.
type APIPRPanel struct {
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	// PRs are a workspace change's PRs, in merge order.
	PRs     []APILinkedPR `json:"prs"`
	HeadSHA string        `json:"head_sha"`
	Checks  []APICheck    `json:"checks"`
	// ChecksError is set when GitHub could not be asked.
	ChecksError string     `json:"checks_error"`
	GateJobID   *int64     `json:"gate_job_id"`
	Gates       []APIGate  `json:"gates"`
	Review      *APIReview `json:"review"`
	// LastJobID is the job retry acts on.
	LastJobID *int64 `json:"last_job_id"`
}

// APIDiff is the patch the reviewer saw, with its findings.
type APIDiff struct {
	JobID    int64        `json:"job_id"`
	Patch    string       `json:"patch"`
	Findings []APIFinding `json:"findings"`
}

// APIFeed is the orchestrator's decisions, newest first.
type APIFeed struct {
	Events []APIEvent `json:"events"`
}

// APIDaySpend is one day of spend.
type APIDaySpend struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"cost_usd"`
}

// APIModelSpend is spend on one model.
type APIModelSpend struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
}

// APIUsage is a usage-limit window a harness last reported.
type APIUsage struct {
	Harness     string  `json:"harness"`
	Window      string  `json:"window"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    string  `json:"resets_at"`
	SeenAt      string  `json:"seen_at"`
}

// APISpend is spend by day (newest first, 30 days) and by model.
type APISpend struct {
	Days        []APIDaySpend   `json:"days"`
	WeekUSD     float64         `json:"week_usd"`
	DailyCapUSD float64         `json:"daily_cap_usd"`
	ByModel     []APIModelSpend `json:"by_model"`
	Usage       []APIUsage      `json:"usage"`
}

// APINotification is one thing the human may need to act on.
type APINotification struct {
	// ID is stable for as long as the condition holds, so the browser can
	// tell new notifications from ones it has shown.
	ID     string `json:"id"`
	Kind   string `json:"kind"` // needs_input|ready_for_review|budget
	Title  string `json:"title"`
	Detail string `json:"detail"`
	ItemID *int64 `json:"item_id"`
	At     string `json:"at"`
}

// APINotifications is the notifications center.
type APINotifications struct {
	Notifications []APINotification `json:"notifications"`
}

// APIActionResult answers a button press.
type APIActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// APIError is any error response.
type APIError struct {
	Error string `json:"error"`
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, APIError{Error: msg})
}

func (s *Server) apiItem(ctx context.Context, it *store.WorkItem) APIItem {
	a := APIItem{
		ID: it.ID, Issue: it.IssueNumber, Title: it.Title, Stage: string(it.Stage), Complexity: it.Complexity,
		Risk: it.Risk, Priority: it.Priority, ParentID: it.ParentID, Branch: it.Branch, PRNumber: it.PRNumber,
		CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		Repo: it.Repo, Ref: s.ref(it.Repo, it.IssueNumber),
		IssueURL: s.issueURL(it.Repo, it.IssueNumber),
		NeedsYou: it.Stage == state.AwaitingHuman || it.Stage == state.Parked,
	}
	a.PRs, a.PRURL = s.linkedPRs(ctx, it)
	if it.Stage == state.Parked || it.Stage == state.Blocked {
		a.BlockedNote = s.stageReason(ctx, it.ID, it.Stage)
	}
	return a
}

// linkedPRs are an item's PRs across a workspace, and the URL of its main
// PR: the one in its own repo when the change has one, else the first.
func (s *Server) linkedPRs(ctx context.Context, it *store.WorkItem) ([]APILinkedPR, string) {
	out := []APILinkedPR{}
	rows, _ := s.Store.ItemPRs(ctx, it.ID)
	for _, r := range rows {
		out = append(out, APILinkedPR{Repo: r.Repo, Number: r.Number, Ref: fmt.Sprintf("%s#%d", repoShort(r.Repo), r.Number), URL: s.prURL(r.Repo, r.Number)})
	}
	switch {
	case len(out) > 0:
		return out, mainPR(it, out).URL
	case it.PRNumber > 0:
		return out, s.prURL(it.Repo, it.PRNumber)
	}
	return out, ""
}

func mainPR(it *store.WorkItem, prs []APILinkedPR) APILinkedPR {
	for _, p := range prs {
		if strings.EqualFold(p.Repo, it.Repo) {
			return p
		}
	}
	return prs[0]
}

func repoShort(repo string) string { return repo[strings.LastIndex(repo, "/")+1:] }

// stageReason is the reason recorded when the item last entered stage.
func (s *Server) stageReason(ctx context.Context, itemID int64, st state.Stage) string {
	e := s.stageEntry(ctx, itemID, st)
	if e == nil {
		return ""
	}
	return e.Reason
}

// stageEntry is the event of the item's last move into stage.
func (s *Server) stageEntry(ctx context.Context, itemID int64, st state.Stage) *store.Event {
	evs, err := s.Store.Events(ctx, itemID)
	if err != nil {
		return nil
	}
	want := fmt.Sprintf(`"to":%q`, string(st))
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == "stage" && strings.Contains(evs[i].Data, want) {
			return &evs[i]
		}
	}
	return nil
}

// isReview covers the reviewer and, on high-risk items, the second-opinion critic.
func isReview(j *store.Job) bool { return j.Kind == "review" || j.Kind == "critic" }

func apiJob(j *store.Job) APIJob {
	return APIJob{
		ID: j.ID, ItemID: j.ItemID, Kind: j.Kind, State: string(j.State), Harness: j.Harness, Model: j.ModelLabel(),
		Effort: j.Effort, Attempt: j.Attempt, CostUSD: j.CostUSD, BudgetUSD: j.BudgetUSD, TokensIn: j.TokensIn,
		TokensOut: j.TokensOut, Verdict: j.Verdict, Error: j.Error, QueuedAt: j.QueuedAt, StartedAt: j.StartedAt,
		FinishedAt: j.FinishedAt, Branch: j.Branch, Skillset: j.Skillset, HeadSHA: j.HeadSHA,
	}
}

func apiEvents(evs []store.Event) []APIEvent {
	out := make([]APIEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, APIEvent{ID: e.ID, At: e.At, Actor: e.Actor, ItemID: e.ItemID, JobID: e.JobID, Type: e.Type, Reason: e.Reason, Data: e.Data})
	}
	return out
}

func apiGates(gs []store.GateResult) []APIGate {
	out := make([]APIGate, 0, len(gs))
	for _, g := range gs {
		out = append(out, APIGate{Gate: g.Gate, Status: g.Status, Detail: g.Detail})
	}
	return out
}

func apiFindings(fs []verdict.Finding) []APIFinding {
	out := make([]APIFinding, 0, len(fs))
	for _, f := range fs {
		out = append(out, APIFinding{Severity: f.Severity, File: f.File, Line: f.Line, Claim: f.Claim, Evidence: f.Evidence})
	}
	return out
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := idParam(r)
	if err != nil {
		apiErr(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

// sortedJobs returns an item's jobs, oldest first.
func (s *Server) sortedJobs(ctx context.Context, itemID int64) ([]*store.Job, error) {
	jobs, err := s.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID})
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
	return jobs, nil
}

// --- handlers ---

func (s *Server) routesAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/openapi.yaml", s.apiSpec)
	mux.HandleFunc("GET /api/v1/overview", s.apiOverview)
	mux.HandleFunc("GET /api/v1/board", s.apiBoard)
	mux.HandleFunc("GET /api/v1/items/{id}", s.apiItemDetail)
	mux.HandleFunc("GET /api/v1/items/{id}/pr", s.apiPRPanel)
	mux.HandleFunc("GET /api/v1/items/{id}/diff", s.apiDiff)
	mux.HandleFunc("POST /api/v1/items/{id}/park", s.apiPark)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.apiJobDetail)
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", s.apiLog)
	mux.HandleFunc("GET /api/v1/jobs/{id}/terminal", s.apiTerminal)
	mux.HandleFunc("POST /api/v1/jobs/{id}/retry", s.apiRetry)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.apiCancel)
	mux.HandleFunc("GET /api/v1/orchestrator", s.apiFeed)
	mux.HandleFunc("GET /api/v1/activity", s.apiActivity)
	mux.HandleFunc("GET /api/v1/metrics", s.metricsJSON)
	mux.HandleFunc("GET /api/v1/spend", s.apiSpend)
	mux.HandleFunc("GET /api/v1/notifications", s.apiNotifications)
	mux.HandleFunc("GET /api/v1/stream", s.apiStream)
	mux.HandleFunc("GET /api/v1/settings", s.apiSettings)
	mux.HandleFunc("POST /api/v1/settings", s.apiSaveSettings)
	mux.HandleFunc("POST /api/v1/settings/restart", s.apiRestart)
	mux.HandleFunc("POST /api/v1/settings/restart/cancel", s.apiCancelRestart)
	mux.HandleFunc("GET /api/v1/settings/tuning", s.apiTuning)
	mux.HandleFunc("POST /api/v1/settings/tuning/{id}/apply", s.apiApplyTuning)
	mux.HandleFunc("POST /api/v1/settings/tuning/{id}/dismiss", s.apiDismissTuning)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { apiErr(w, http.StatusNotFound, "no such endpoint") })
}

func (s *Server) apiSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(OpenAPI)
}

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	spent, _ := s.Store.SpendOn(ctx, store.Today())
	waiting, _ := s.Store.ListItems(ctx, state.AwaitingHuman)
	running, _ := s.Store.CountJobs(ctx, state.Running, "")
	queued, _ := s.Store.CountJobs(ctx, state.Queued, "")
	v, err := s.Store.Version(ctx)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APIOverview{Repo: firstOr(s.Repos, ""), Repos: s.Repos, SpentTodayUSD: spent, DailyCapUSD: s.DailyCap, Awaiting: len(waiting),
		AwaitingCap: s.AwaitCap, Running: running, Queued: queued, Version: v, DiskLow: s.diskLow(), SignedIn: s.Login != nil && s.Login.Password != "",
		MainHealth: s.mainHealth(ctx)})
}

func (s *Server) mainHealth(ctx context.Context) []APIMainHealth {
	out := []APIMainHealth{}
	hs, _ := s.Store.RepoHealths(ctx)
	issues, _ := s.Store.HealthIssues(ctx, "")
	for _, h := range hs {
		if !slices.ContainsFunc(s.Repos, func(r string) bool { return strings.EqualFold(r, h.Repo) }) {
			continue // no longer configured
		}
		m := APIMainHealth{Repo: h.Repo, Branch: h.Branch, State: h.State, SHA: h.HeadSHA, Detail: h.Detail, Checks: []APIHealthCheck{}}
		for _, is := range issues {
			if is.Repo == h.Repo && (is.State == store.HealthRed || is.State == store.HealthFlaky) {
				c := APIHealthCheck{Check: is.Check, Kind: is.State, Issue: is.IssueNumber}
				if is.IssueNumber > 0 {
					c.IssueURL = s.issueURL(is.Repo, is.IssueNumber)
				}
				m.Checks = append(m.Checks, c)
			}
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) apiBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := s.Store.ListItems(ctx)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cols := map[state.Stage]*APIColumn{}
	board := APIBoard{Columns: []APIColumn{}}
	for _, st := range boardStages {
		cols[st] = &APIColumn{Stage: string(st), Cards: []APICard{}}
	}
	for _, it := range items {
		col, ok := cols[it.Stage]
		if !ok {
			continue
		}
		jobs, err := s.sortedJobs(ctx, it.ID)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		c := APICard{APIItem: s.apiItem(ctx, it)}
		for _, j := range jobs {
			c.CostUSD += j.CostUSD
			if j.Kind == "implement" {
				c.Model = j.Model
			}
			if j.State == state.Running {
				c.Live = true
				id := j.ID
				c.LiveJob = &id
			}
		}
		s.cardProgress(ctx, &c, jobs)
		col.Cards = append(col.Cards, c)
	}
	for _, st := range boardStages {
		c := cols[st]
		sort.SliceStable(c.Cards, func(a, b int) bool { return c.Cards[a].Priority > c.Cards[b].Priority })
		board.Columns = append(board.Columns, *c)
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) apiItemDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		apiErr(w, http.StatusNotFound, "no such item")
		return
	}
	jobs, err := s.sortedJobs(ctx, id)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	evs, _ := s.Store.Events(ctx, id)
	deps, _ := s.Store.Dependencies(ctx, id)
	d := APIItemDetail{Item: s.apiItem(ctx, it), Jobs: []APIJob{}, Events: apiEvents(evs), DependsOn: deps}
	if d.DependsOn == nil {
		d.DependsOn = []int64{}
	}
	for _, j := range jobs {
		d.Jobs = append(d.Jobs, apiJob(j))
		if j.Kind == "triage" && j.State == state.Succeeded {
			var t verdict.Triage
			if json.Unmarshal([]byte(j.Verdict), &t) == nil && t.Summary != "" {
				d.Spec = t.Summary
			}
		}
	}
	if s.Issue != nil {
		ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if is, err := s.Issue(ictx, it.Repo, it.IssueNumber); err == nil {
			d.IssueBody = is.Body
		}
		cancel()
	}
	writeJSON(w, http.StatusOK, d)
}

// latestReview is the item's newest review or critic job with a usable
// verdict. The PR panel and the diff viewer both use it, so a finding's
// link always lands on the diff that finding was made against.
func latestReview(jobs []*store.Job) (*store.Job, *verdict.Review) {
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if !isReview(j) || j.State != state.Succeeded {
			continue
		}
		var v verdict.Review
		if verdict.Parse([]byte(j.Verdict), &v) == nil {
			return j, &v
		}
	}
	return nil, nil
}

func (s *Server) apiPRPanel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		apiErr(w, http.StatusNotFound, "no such item")
		return
	}
	jobs, err := s.sortedJobs(ctx, id)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := APIPRPanel{PRNumber: it.PRNumber, Checks: []APICheck{}, Gates: []APIGate{}}
	p.PRs, p.PRURL = s.linkedPRs(ctx, it)
	// The checks shown are the main PR's: in a workspace, the head the
	// pipeline recorded is that PR's repo's.
	checksRepo := it.Repo
	for i := len(jobs) - 1; i >= 0; i-- {
		if p.LastJobID == nil && jobs[i].Kind != "orchestrate" {
			jid := jobs[i].ID
			p.LastJobID = &jid
		}
		if jobs[i].Kind == "implement" && jobs[i].HeadSHA != "" && p.GateJobID == nil {
			jid := jobs[i].ID
			p.GateJobID = &jid
			p.HeadSHA = jobs[i].HeadSHA
			// In a workspace the head is in whichever repo that job recorded it.
			hs, _ := s.Store.JobHeads(ctx, jid)
			for _, h := range hs {
				if h.SHA == p.HeadSHA {
					checksRepo = h.Repo
				}
			}
			gs, _ := s.Store.GateResults(ctx, jid)
			p.Gates = apiGates(gs)
		}
	}
	if j, v := latestReview(jobs); j != nil {
		p.Review = &APIReview{JobID: j.ID, Decision: v.Decision, Findings: apiFindings(v.Findings), Assumptions: v.ChallengedAssumptions}
		if p.Review.Assumptions == nil {
			p.Review.Assumptions = []string{}
		}
	}
	if p.HeadSHA != "" && s.Checks != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		runs, err := s.Checks(cctx, checksRepo, p.HeadSHA)
		cancel()
		var he *github.HTTPError
		switch {
		case errors.As(err, &he) && he.Status == http.StatusForbidden:
			p.ChecksError = "GitHub refused to show check runs: give the factory's GitHub App the Checks: Read-only permission"
		case err != nil:
			p.ChecksError = err.Error()
		}
		for _, c := range runs {
			p.Checks = append(p.Checks, APICheck{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion, URL: c.URL})
		}
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiDiff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	jobs, err := s.sortedJobs(ctx, id)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	read := func(j *store.Job) ([]byte, bool) {
		if s.JobsDir == "" {
			return nil, false
		}
		b, err := os.ReadFile(filepath.Join(s.JobsDir, strconv.FormatInt(j.ID, 10), "diff.patch"))
		return b, err == nil
	}
	// The diff the latest verdict was made against, with its findings.
	if j, v := latestReview(jobs); j != nil {
		if b, ok := read(j); ok {
			writeJSON(w, http.StatusOK, APIDiff{JobID: j.ID, Patch: string(b), Findings: apiFindings(v.Findings)})
			return
		}
	}
	// Otherwise the newest diff a review has saved (one still running).
	for i := len(jobs) - 1; i >= 0; i-- {
		if !isReview(jobs[i]) {
			continue
		}
		if b, ok := read(jobs[i]); ok {
			writeJSON(w, http.StatusOK, APIDiff{JobID: jobs[i].ID, Patch: string(b), Findings: []APIFinding{}})
			return
		}
	}
	apiErr(w, http.StatusNotFound, "no reviewed diff for this item yet")
}

func (s *Server) apiJobDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := s.Store.GetJob(ctx, id)
	if err != nil {
		apiErr(w, http.StatusNotFound, "no such job")
		return
	}
	gs, _ := s.Store.GateResults(ctx, id)
	writeJSON(w, http.StatusOK, APIJobDetail{Job: apiJob(j), Gates: apiGates(gs), CanAttach: s.Exec != nil && j.State == state.Running})
}

// apiLog streams a job's transcript as SSE: one `line` event per log line
// (data is {"text": ...}), then one `done` event ({"state": ...}).
func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	s.tailLog(w, r, id, func(line string) string {
		b, _ := json.Marshal(map[string]string{"text": summarize(line)})
		return "event: line\ndata: " + string(b) + "\n\n"
	}, func(st state.JobState) string {
		b, _ := json.Marshal(map[string]string{"state": string(st)})
		return "event: done\ndata: " + string(b) + "\n\n"
	})
}

func (s *Server) readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// jsonOnly refuses a POST that is not JSON. A cross-site form cannot send
// application/json without a CORS preflight, which this server never grants.
func jsonOnly(w http.ResponseWriter, r *http.Request) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		apiErr(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	return true
}

func (s *Server) act(w http.ResponseWriter, err error, ok string) {
	if err != nil {
		writeJSON(w, http.StatusConflict, APIActionResult{OK: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, APIActionResult{OK: true, Message: ok})
}

func (s *Server) apiRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	var in struct {
		TierUp bool `json:"tier_up"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.act(w, s.Actions.Retry(r.Context(), id, in.TierUp), "Retry queued.")
}

func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	s.act(w, s.Actions.Cancel(r.Context(), id), "Cancelled.")
}

func (s *Server) apiPark(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "Parked from the dashboard."
	}
	s.act(w, s.Actions.Park(r.Context(), id, reason), "Parked.")
}

func (s *Server) apiFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var item int64
	if q := r.URL.Query().Get("item"); q != "" {
		var err error
		if item, err = strconv.ParseInt(q, 10, 64); err != nil || item <= 0 {
			apiErr(w, http.StatusBadRequest, "item must be an item id")
			return
		}
	}
	evs, err := s.Store.EventsByActorFor(ctx, "orchestrator", item, 500)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APIFeed{Events: apiEvents(evs)})
}

func (s *Server) apiSpend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := APISpend{Days: []APIDaySpend{}, DailyCapUSD: s.DailyCap, ByModel: []APIModelSpend{}, Usage: []APIUsage{}}
	limits, err := s.Store.Limits(ctx)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, l := range limits {
		out.Usage = append(out.Usage, APIUsage{Harness: l.Harness, Window: l.Window, UsedPercent: l.UsedPercent, ResetsAt: l.ResetsAt, SeenAt: l.SeenAt})
	}
	for i := 0; i < 30; i++ {
		d := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		c, err := s.Store.SpendOn(ctx, d)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if i < 7 {
			out.WeekUSD += c
		}
		out.Days = append(out.Days, APIDaySpend{Day: d, CostUSD: c})
	}
	jobs, err := s.Store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	by := map[string]float64{}
	for _, j := range jobs {
		if j.CostUSD > 0 {
			by[j.Harness+"/"+j.Model] += j.CostUSD
		}
	}
	for m, c := range by {
		out.ByModel = append(out.ByModel, APIModelSpend{Model: m, CostUSD: c})
	}
	sort.Slice(out.ByModel, func(a, b int) bool { return out.ByModel[a].CostUSD > out.ByModel[b].CostUSD })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiNotifications(w http.ResponseWriter, r *http.Request) {
	ns, err := s.notifications(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APINotifications{Notifications: ns})
}

func (s *Server) notifications(ctx context.Context) ([]APINotification, error) {
	out := []APINotification{}
	waiting, err := s.Store.ListItems(ctx, state.AwaitingHuman, state.Parked)
	if err != nil {
		return nil, err
	}
	for _, it := range waiting {
		id := it.ID
		n := APINotification{ItemID: &id, At: it.UpdatedAt}
		if it.Stage == state.AwaitingHuman {
			n.Kind = "ready_for_review"
			n.Title = s.ref(it.Repo, it.IssueNumber) + " is ready for your review"
			n.Detail = it.Title
			if prs, _ := s.linkedPRs(ctx, it); len(prs) > 0 {
				refs := make([]string, len(prs))
				for i, p := range prs {
					refs[i] = p.Ref
				}
				n.Detail = fmt.Sprintf("PRs %s: %s", strings.Join(refs, ", "), it.Title)
			} else if it.PRNumber > 0 {
				n.Detail = fmt.Sprintf("PR #%d: %s", it.PRNumber, it.Title)
			}
		} else {
			n.Kind = "needs_input"
			n.Title = s.ref(it.Repo, it.IssueNumber) + " needs your input"
			n.Detail = s.stageReason(ctx, it.ID, state.Parked)
			if n.Detail == "" {
				n.Detail = it.Title
			}
		}
		// Stable while the item stays in the stage: priority or parent
		// changes must not raise the same alert again.
		n.ID = fmt.Sprintf("%s-%d", n.Kind, it.ID)
		if e := s.stageEntry(ctx, it.ID, it.Stage); e != nil {
			n.ID = fmt.Sprintf("%s-%d-%d", n.Kind, it.ID, e.ID)
			n.At = e.At
		}
		out = append(out, n)
	}
	if s.DailyCap > 0 {
		spent, err := s.Store.SpendOn(ctx, store.Today())
		if err != nil {
			return nil, err
		}
		level := ""
		switch {
		case spent >= s.DailyCap:
			level = "reached"
		case spent >= 0.8*s.DailyCap:
			level = "near"
		}
		if level != "" {
			out = append(out, APINotification{
				ID: "budget-" + level + "-" + store.Today(), Kind: "budget",
				Title:  fmt.Sprintf("Daily budget %s: $%.2f of $%.2f", map[string]string{"reached": "reached", "near": "at 80%"}[level], spent, s.DailyCap),
				Detail: map[string]string{"reached": "No new jobs start until tomorrow (UTC).", "near": "New jobs stop starting when the cap is reached."}[level],
				At:     store.Now(),
			})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At > out[b].At })
	return out, nil
}

// apiStream sends a `version` event whenever anything the dashboard shows
// changes; the app refetches what it is showing when one arrives.
func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fl, ok := w.(http.Flusher)
	if !ok {
		apiErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	last := ""
	tick := time.NewTicker(s.pollPeriod)
	defer tick.Stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		v, err := s.Store.Version(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return
		}
		if v != last {
			last = v
			b, _ := json.Marshal(map[string]string{"version": v})
			fmt.Fprintf(w, "event: version\ndata: %s\n\n", b)
			fl.Flush()
		}
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case <-tick.C:
		}
	}
}

// IssueFunc reads an issue; CheckFunc lists CI checks on a commit.
type (
	IssueFunc func(ctx context.Context, repo string, n int) (*github.Issue, error)
	CheckFunc func(ctx context.Context, repo, sha string) ([]github.CheckRun, error)
)

func firstOr(xs []string, def string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return def
}
