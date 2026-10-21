package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

func TestSetupAndUsageSchemasMatchGo(t *testing.T) {
	t.Parallel()
	var spec struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"Readiness": APIReadiness{}, "RepoReadiness": APIRepoReadiness{}, "ReadinessStep": APIReadinessStep{}, "ReadinessFinding": APIReadinessFinding{},
		"PlanUsage": APIPlanUsage{}, "UsagePlan": APIUsagePlan{}} {
		props, required := spec.Components.Schemas[name].flatten(spec.Components.Schemas)
		goFields := strings.Join(jsonFields(reflect.TypeOf(v)), ",")
		if strings.Join(props, ",") != goFields || strings.Join(required, ",") != goFields {
			t.Errorf("%s: spec properties %v, required %v, Go fields %s", name, props, required, goFields)
		}
	}
}

// notReady is o/r's report: lint fails (the factory is building its
// fix), tests are flaky (filed, not queued), a service is missing (yours
// to fix), and two gaps, one not filed and one closed.
func notReady() *readiness.Report {
	return &readiness.Report{Repo: "o/r", Branch: "main", SHA: "abc1234", Score: 30, Started: time.Now(), Filed: []int{12, 13, 14},
		Findings: []readiness.Finding{
			{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on main", Detail: "Every change would fail this check.",
				Issue: &readiness.Issue{Key: "check:lint", Title: "Make the lint check pass on main", Number: 12, State: "open"}},
			{Level: readiness.Blocker, Area: "services", Title: "`test` can't reach a service it needs"},
			{Level: readiness.Blocker, Area: "tests", Title: "Tests are flaky", Issue: &readiness.Issue{Key: "flaky", Title: "Fix flaky tests", Number: 13, State: "open"}},
			{Level: readiness.Gap, Area: "feature map", Title: "No feature map", Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map"}},
			{Level: readiness.Gap, Area: "e2e", Title: "No end-to-end tests", Issue: &readiness.Issue{Key: "e2e", Title: "Add e2e", Number: 14, State: "closed"}},
			{Level: readiness.Good, Area: "install", Title: "Installs from a clean checkout"},
			{Level: readiness.Good, Area: "ci", Title: "CI runs on the repo"},
		}}
}

func readinessServer(t *testing.T) (string, *store.Store, *Server) {
	t.Helper()
	dir := t.TempDir()
	st := storetest.Open(t, filepath.Join(dir, "f.db"))
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 12, "Make the lint check pass on main")
	st.SetStage(ctx, it.ID, state.Ready, "daemon", "")
	st.SetStage(ctx, it.ID, state.Implementing, "daemon", "")
	reports := filepath.Join(dir, "readiness")
	if _, err := readiness.Save(reports, notReady()); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Actions: &fakeActions{}, Repos: []string{"o/r", "o/s"}, ReadinessDir: reports, IntakeLabel: "factory:ready"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httpServer(t, h), st, s
}

func TestReadinessLinksBlockersToTheirWork(t *testing.T) {
	t.Parallel()
	base, _, _ := readinessServer(t)
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if got.Banner != "not_ready" || got.CanCheck || !strings.Contains(got.CheckNote, "FACTORY_WEB_PASSWORD") || len(got.Repos) != 2 {
		t.Fatalf("readiness = %+v", got)
	}
	r, s := got.Repos[0], got.Repos[1]
	if s.State != "unchecked" || len(s.Steps) != 0 {
		t.Fatalf("o/s = %+v", s)
	}
	// The person's part: a missing service. The factory's: lint and flaky
	// tests, each with its issue and the work on it.
	if r.State != "not_ready" || r.Done != 0 || r.Total != 1 || len(r.Steps) != 1 || r.SHA != "abc1234" {
		t.Fatalf("o/r = %+v", r)
	}
	run := r.Steps[0]
	if run.Key != "run" || run.State != "failing" || len(run.Blockers) != 1 || run.Passed != "" {
		t.Fatalf("run = %+v", run)
	}
	if svc := run.Blockers[0]; svc.Status != "none" || svc.Issue != 0 {
		t.Fatalf("services = %+v", svc)
	}
	if len(r.Work) != 2 {
		t.Fatalf("work = %+v", r.Work)
	}
	lint, fl := r.Work[0], r.Work[1]
	if lint.Status != "building" || lint.Issue != 12 || !strings.HasSuffix(lint.IssueURL, "/o/r/issues/12") || lint.ItemID == nil || lint.ItemStage != "implementing" {
		t.Fatalf("lint = %+v", lint)
	}
	if fl.Status != "not_queued" || fl.Issue != 13 || fl.ItemID != nil {
		t.Fatalf("flaky = %+v", fl)
	}
	status := map[string]string{}
	for _, f := range r.Extras {
		status[f.Title] = f.Status
	}
	if len(r.Extras) != 3 || status["No feature map"] != "not_filed" || status["No end-to-end tests"] != "closed" || status["CI runs on the repo"] != "none" {
		t.Fatalf("extras = %+v", r.Extras)
	}
}

func TestReadinessCheckAgain(t *testing.T) {
	t.Parallel()
	base, st, s := readinessServer(t)
	ctx := context.Background()
	// Without a password, only from this machine.
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/r"}`); code != http.StatusForbidden || !strings.Contains(body, "FACTORY_WEB_PASSWORD") {
		t.Fatalf("check = %d %s", code, body)
	}
	s.Listen = "127.0.0.1:8080"
	// A daemon that can't run it says so.
	if code, _ := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/r"}`); code != http.StatusForbidden {
		t.Fatalf("check without a checker = %d", code)
	}
	// A check is a job the daemon runs: the page queues it.
	s.JobsDir = t.TempDir()
	s.QueueReadiness = (&pipeline.Pipeline{Store: st}).QueueReadiness
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"nope/x"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown repo = %d %s", code, body)
	}
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"O/R"}`); code != http.StatusOK || !strings.Contains(body, "Checking o/r") {
		t.Fatalf("check = %d %s", code, body)
	}
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if r := got.Repos[0]; !got.CanCheck || !r.Checking || r.CheckingSince == "" || r.CheckJob == nil || r.CheckingWhy != "" {
		t.Fatalf("while queued = %+v", r)
	}
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/r"}`); code != http.StatusConflict || !strings.Contains(body, "already queued or running") {
		t.Fatalf("second check = %d %s", code, body)
	}

	// The daemon runs it, saying what it does as it goes. The fix for lint
	// merged, and someone fixed the flaky tests by hand: it reads the repo
	// as it is now.
	j, err := st.ClaimJobKind(ctx, pipeline.KindReadiness)
	if err != nil || j == nil {
		t.Fatalf("claimed %+v, %v", j, err)
	}
	dir := filepath.Join(s.JobsDir, strconv.FormatInt(j.ID, 10))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, pipeline.ProgressFile), []byte(`{"at":"2026-10-21T10:00:00Z","text":"passed `+"`bundle install`"+` in 2m14s"}`+"\n"), 0o644)
	getJSON(t, base+"/api/v1/readiness", &got)
	if r := got.Repos[0]; !r.Checking || len(r.Progress) != 1 || r.Progress[0].Text != "passed `bundle install` in 2m14s" {
		t.Fatalf("while running = %+v", r)
	}
	r := notReady()
	r.Findings = []readiness.Finding{r.Findings[3], r.Findings[4], r.Findings[5], r.Findings[6],
		{Level: readiness.Good, Area: "checks", Title: "Every required check passes on main: lint, test"},
		{Level: readiness.Good, Area: "tests", Title: "12 test files, passing twice in a row"}}
	r.Ready = true
	readiness.Save(s.ReadinessDir, r)
	it, _ := st.ItemByIssue(ctx, "o/r", 12)
	st.SetStage(ctx, it.ID, state.Reviewing, "daemon", "")
	st.SetStage(ctx, it.ID, state.Done, "daemon", "")
	os.WriteFile(filepath.Join(dir, pipeline.ResultFile), []byte("Checked main at abc1234. Opened 0 issue(s) on o/r."), 0o644)
	st.FinishJob(ctx, j.ID, store.Result{State: state.Succeeded})
	got = APIReadiness{}
	getJSON(t, base+"/api/v1/readiness", &got)
	rr := got.Repos[0]
	if rr.Checking || rr.State != "ready" || rr.Done != 1 || rr.Total != 1 || len(rr.Work) != 0 || rr.Result != "Checked main at abc1234. Opened 0 issue(s) on o/r." || len(rr.Progress) != 1 {
		t.Fatalf("after = %+v", rr)
	}
	// Every checked repo is ready and the factory has only worked on setup.
	if got.Banner != "ready" || got.IntakeLabel != "factory:ready" {
		t.Fatalf("banner %q", got.Banner)
	}
	// A failed check says why; one the factory started says what did.
	if queued, err := s.QueueReadiness(ctx, "o/s", "main moved from 1a2b3c4 to 5d6e7f8"); !queued || err != nil {
		t.Fatalf("queued %v, %v", queued, err)
	}
	getJSON(t, base+"/api/v1/readiness", &got)
	if w := got.Repos[1].CheckingWhy; w != "Started by itself: main moved from 1a2b3c4 to 5d6e7f8." {
		t.Fatalf("why = %q", w)
	}
	j, _ = st.ClaimJobKind(ctx, pipeline.KindReadiness)
	st.FinishJob(ctx, j.ID, store.Result{State: state.Failed, Error: "cloning o/s: no access"})
	getJSON(t, base+"/api/v1/readiness", &got)
	if got.Repos[1].CheckError != "The last check failed: cloning o/s: no access" || got.Repos[1].State != "unchecked" {
		t.Fatalf("o/s = %+v", got.Repos[1])
	}
	// Real work hides the banner.
	st.UpsertItem(ctx, "o/r", 50, "Add dark mode")
	getJSON(t, base+"/api/v1/readiness", &got)
	if got.Banner != "" {
		t.Fatalf("banner after real work = %q", got.Banner)
	}
}

// A blocker whose item was split shows the split's progress, and the
// item's page lists the issues it was split into.
func TestReadinessShowsASplitBlocker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, st, _ := readinessServer(t)
	lint, _ := st.ItemByIssue(ctx, "o/r", 12)
	st.SetStage(ctx, lint.ID, state.Parked, "daemon", "")
	st.SetStage(ctx, lint.ID, state.Epic, "chat", "split")
	for i, stage := range []state.Stage{state.Triage, state.Done} {
		c, _, _ := st.UpsertItem(ctx, "o/r", 20+i, "part")
		st.SetParent(ctx, c.ID, lint.ID)
		if stage == state.Done {
			for _, s := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done} {
				st.SetStage(ctx, c.ID, s, "daemon", "")
			}
		}
	}
	// The last waits for both, but only the one not done yet shows.
	last, _, _ := st.UpsertItem(ctx, "o/r", 22, "the rest")
	st.SetParent(ctx, last.ID, lint.ID)
	first, _ := st.ItemByIssue(ctx, "o/r", 20)
	done, _ := st.ItemByIssue(ctx, "o/r", 21)
	if err := st.SetDependencies(ctx, last.ID, []int64{first.ID, done.ID}, "chat", "after the autocorrects"); err != nil {
		t.Fatal(err)
	}
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if f := got.Repos[0].Work[0]; f.Status != "split" || f.SplitDone != 1 || f.SplitTotal != 3 {
		t.Fatalf("lint = %+v", f)
	}
	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/"+strconv.FormatInt(lint.ID, 10), &d)
	if len(d.Children) != 3 || d.Children[0].Issue != 20 || d.Children[1].Stage != "done" || d.Item.NeedsYou {
		t.Fatalf("detail = %+v", d)
	}
	if w := d.Children[2].WaitsFor; len(w) != 1 || w[0].ID != first.ID || w[0].Ref != "o/r#20" || len(d.Children[0].WaitsFor) != 0 {
		t.Fatalf("waits for = %+v", d.Children)
	}
}

// A repo that isn't ready is checked again once its base branch moves:
// a check is queued, saying why.
func TestFollowReadinessChecksAgainWhenTheBranchMoves(t *testing.T) {
	t.Parallel()
	_, _, s := readinessServer(t)
	sha := "abc1234" // what the report saw
	s.BaseSHA = func(context.Context, string) (string, error) { return sha, nil }
	var queued []string
	s.QueueReadiness = func(_ context.Context, repo, why string) (bool, error) {
		queued = append(queued, repo+": "+why)
		return true, nil
	}
	ctx := context.Background()
	s.followOnce(ctx, "o/r")
	s.followOnce(ctx, "o/s") // never checked: nothing to follow
	if len(queued) != 0 {
		t.Fatalf("queued %v with nothing changed", queued)
	}
	sha = "def5678"
	s.followOnce(ctx, "o/r")
	if strings.Join(queued, "|") != "o/r: main moved from abc1234 to def5678" {
		t.Fatalf("queued %v", queued)
	}
}

// A blocker split into pieces follows its pieces, even once splitting
// closed its issue. While the repo isn't ready, a piece says it blocks
// setup, and any other work says it waits for setup.
func TestSplitBlockerAndWorkWaitingForSetup(t *testing.T) {
	t.Parallel()
	base, st, _ := readinessServer(t)
	ctx := context.Background()
	lint, _ := st.ItemByIssue(ctx, "o/r", 12)
	piece, _, _ := st.UpsertItem(ctx, "o/r", 20, "RuboCop: autocorrect app/")
	if err := st.SetParent(ctx, piece.ID, lint.ID); err != nil {
		t.Fatal(err)
	}
	st.SetStage(ctx, lint.ID, state.Done, "daemon", "its issue was closed")
	task, _, _ := st.UpsertItem(ctx, "o/r", 30, "Report outdated gems")

	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if f := got.Repos[0].Work[0]; f.Status != "split" || f.SplitTotal != 1 || f.SplitDone != 0 {
		t.Fatalf("lint = %+v", f)
	}
	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/"+strconv.FormatInt(piece.ID, 10), &d)
	if d.Item.Setup != "blocks" || !strings.Contains(d.Item.SetupNote, "Fixes a setup blocker (lint fails on main)") {
		t.Fatalf("piece = %q %q", d.Item.Setup, d.Item.SetupNote)
	}
	getJSON(t, base+"/api/v1/items/"+strconv.FormatInt(task.ID, 10), &d)
	if d.Item.Setup != "held" || !strings.Contains(d.Item.SetupNote, "starts by itself") {
		t.Fatalf("task = %q %q", d.Item.Setup, d.Item.SetupNote)
	}
}
