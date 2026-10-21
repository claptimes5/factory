package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
	if got.Banner != "not_ready" || got.CanCheck || got.CanFile || !strings.Contains(got.CheckNote, "FACTORY_WEB_PASSWORD") || len(got.Repos) != 2 {
		t.Fatalf("readiness = %+v", got)
	}
	r, s := got.Repos[0], got.Repos[1]
	if s.State != "unchecked" || s.CanQueue || len(s.Steps) != 0 {
		t.Fatalf("o/s = %+v", s)
	}
	// Install passed; the checks and the tests fail.
	if r.State != "not_ready" || r.Done != 1 || r.Total != 3 || len(r.Steps) != 3 || r.SHA != "abc1234" {
		t.Fatalf("o/r = %+v", r)
	}
	if st := r.Steps[0]; st.Key != "install" || st.State != "done" || st.Passed != "Installs from a clean checkout" {
		t.Fatalf("install = %+v", st)
	}
	checks := r.Steps[1]
	if checks.State != "failing" || len(checks.Blockers) != 2 {
		t.Fatalf("checks = %+v", checks)
	}
	lint, svc := checks.Blockers[0], checks.Blockers[1]
	if lint.Status != "building" || lint.Issue != 12 || !strings.HasSuffix(lint.IssueURL, "/o/r/issues/12") || lint.ItemID == nil || lint.ItemStage != "implementing" {
		t.Fatalf("lint = %+v", lint)
	}
	if svc.Status != "none" || svc.Issue != 0 {
		t.Fatalf("services = %+v", svc)
	}
	if fl := r.Steps[2].Blockers[0]; fl.Status != "not_queued" || fl.Issue != 13 || fl.ItemID != nil {
		t.Fatalf("flaky = %+v", fl)
	}
	status := map[string]string{}
	for _, f := range r.Extras {
		status[f.Title] = f.Status
	}
	if len(r.Extras) != 3 || status["No feature map"] != "not_filed" || status["No end-to-end tests"] != "closed" || status["CI runs on the repo"] != "none" {
		t.Fatalf("extras = %+v", r.Extras)
	}
	if r.Unqueued != 1 || r.Unfiled != 1 || !r.CanQueue {
		t.Fatalf("unqueued %d, unfiled %d, can queue %v", r.Unqueued, r.Unfiled, r.CanQueue)
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

	// The fix for lint merged, and someone fixed the flaky tests by hand:
	// the check reads the repo as it is now.
	release := make(chan struct{})
	s.CheckReadiness = func(ctx context.Context, repo string) error {
		<-release
		if repo == "o/s" {
			return errors.New("cloning o/s: no access")
		}
		r := notReady()
		r.Findings = []readiness.Finding{r.Findings[3], r.Findings[4], r.Findings[5], r.Findings[6],
			{Level: readiness.Good, Area: "checks", Title: "Every required check passes on main: lint, test"},
			{Level: readiness.Good, Area: "tests", Title: "12 test files, passing twice in a row"}}
		r.Ready = true
		_, err := readiness.Save(s.ReadinessDir, r)
		return err
	}
	it, _ := st.ItemByIssue(ctx, "o/r", 12)
	st.SetStage(ctx, it.ID, state.Reviewing, "daemon", "")
	st.SetStage(ctx, it.ID, state.Done, "daemon", "")
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"nope/x"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown repo = %d %s", code, body)
	}
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"O/R"}`); code != http.StatusOK || !strings.Contains(body, "Checking o/r") {
		t.Fatalf("check = %d %s", code, body)
	}
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if !got.CanCheck || !got.Repos[0].Checking || got.Repos[0].CheckingSince == "" {
		t.Fatalf("while checking = %+v", got)
	}
	if code, body := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/s"}`); code != http.StatusConflict || !strings.Contains(body, "Already checking o/r") {
		t.Fatalf("second check = %d %s", code, body)
	}
	close(release)
	wait := func() {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; {
			got = APIReadiness{}
			getJSON(t, base+"/api/v1/readiness", &got)
			if !got.Repos[0].Checking && !got.Repos[1].Checking {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the check never finished")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait()
	r := got.Repos[0]
	if r.State != "ready" || r.Done != 3 || r.Total != 3 || r.CheckError != "" {
		t.Fatalf("after = %+v", r)
	}
	// Every checked repo is ready and the factory has only worked on setup.
	if got.Banner != "ready" || got.IntakeLabel != "factory:ready" {
		t.Fatalf("banner %q", got.Banner)
	}
	// A failed check says why.
	if code, _ := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/s"}`); code != http.StatusOK {
		t.Fatalf("check o/s = %d", code)
	}
	wait()
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

func TestReadinessFileFromThePage(t *testing.T) {
	t.Parallel()
	base, _, s := readinessServer(t)
	if code, body := postJSON(t, base+"/api/v1/readiness/file", "", `{"repo":"o/r"}`); code != http.StatusForbidden {
		t.Fatalf("file without a filer = %d %s", code, body)
	}
	release := make(chan struct{})
	var queued []bool
	s.FileReadiness = func(_ context.Context, repo string, queue bool) (string, error) {
		<-release
		queued = append(queued, queue)
		return "Opened 1 issue(s) on " + repo + ".", nil
	}
	// Without a password, only from this machine, as the check.
	if code, body := postJSON(t, base+"/api/v1/readiness/file", "", `{"repo":"o/r"}`); code != http.StatusForbidden || !strings.Contains(body, "FACTORY_WEB_PASSWORD") {
		t.Fatalf("file = %d %s", code, body)
	}
	s.Listen = "127.0.0.1:8080"
	s.CheckReadiness = func(context.Context, string) error { return nil }
	if code, body := postJSON(t, base+"/api/v1/readiness/file", "", `{"repo":"o/r","queue":true}`); code != http.StatusOK || !strings.Contains(body, "then filing") {
		t.Fatalf("file = %d %s", code, body)
	}
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if !got.CanFile || !got.Repos[0].Checking || got.Repos[0].Action != "queue" {
		t.Fatalf("while filing = %+v", got.Repos[0])
	}
	// One run at a time, so nothing is filed twice.
	if code, _ := postJSON(t, base+"/api/v1/readiness/file", "", `{"repo":"o/r"}`); code != http.StatusConflict {
		t.Fatalf("second file = %d", code)
	}
	if code, _ := postJSON(t, base+"/api/v1/readiness/check", "", `{"repo":"o/r"}`); code != http.StatusConflict {
		t.Fatalf("check while filing = %d", code)
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; {
		got = APIReadiness{}
		getJSON(t, base+"/api/v1/readiness", &got)
		if !got.Repos[0].Checking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("filing never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := got.Repos[0]; r.Result != "Opened 1 issue(s) on o/r." || r.CheckError != "" || len(queued) != 1 || !queued[0] {
		t.Fatalf("after = %+v, queued %v", r, queued)
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
	if f := got.Repos[0].Steps[1].Blockers[0]; f.Status != "split" || f.SplitDone != 1 || f.SplitTotal != 3 {
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

// Lint failing on the base branch, with a linter that can be baselined,
// offers the baseline; making it runs in the background and says where
// its pull request is.
func TestReadinessLintBaseline(t *testing.T) {
	t.Parallel()
	base, _, s := readinessServer(t)
	s.Listen = "127.0.0.1:8080"
	s.CheckReadiness = func(context.Context, string) error { return nil }
	var got APIReadiness
	getJSON(t, base+"/api/v1/readiness", &got)
	if got.Repos[0].CanBaseline {
		t.Fatal("offered without a way to make it")
	}
	release := make(chan struct{})
	s.CanBaseline = func(repo string) bool { return repo == "o/r" }
	s.LintBaseline = func(_ context.Context, repo string) (string, error) {
		<-release
		return "https://github.com/o/r/pull/7", nil
	}
	getJSON(t, base+"/api/v1/readiness", &got)
	if !got.Repos[0].CanBaseline || got.Repos[1].CanBaseline {
		t.Fatalf("can baseline = %v %v", got.Repos[0].CanBaseline, got.Repos[1].CanBaseline)
	}
	if code, body := postJSON(t, base+"/api/v1/readiness/baseline", "", `{"repo":"o/r"}`); code != http.StatusOK || !strings.Contains(body, "lint offenses as a baseline") {
		t.Fatalf("baseline = %d %s", code, body)
	}
	getJSON(t, base+"/api/v1/readiness", &got)
	if !got.Repos[0].Checking || got.Repos[0].Action != "baseline" {
		t.Fatalf("while making it = %+v", got.Repos[0])
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got = APIReadiness{}
		getJSON(t, base+"/api/v1/readiness", &got)
		if !got.Repos[0].Checking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the baseline never finished")
		}
	}
	// The pull request stays on the page until lint passes, whatever runs next.
	if got.Repos[0].BaselinePR != "https://github.com/o/r/pull/7" || !strings.Contains(got.Repos[0].Result, "merge it") {
		t.Fatalf("result = %q, PR %q (error %q)", got.Repos[0].Result, got.Repos[0].BaselinePR, got.Repos[0].CheckError)
	}
}

// A repo that isn't ready is checked again once its base branch moves;
// once someone chose File and start work, it's filed and queued instead.
func TestFollowReadinessChecksAgainWhenTheBranchMoves(t *testing.T) {
	t.Parallel()
	_, _, s := readinessServer(t)
	sha := "abc1234" // what the report saw
	s.BaseSHA = func(context.Context, string) (string, error) { return sha, nil }
	ran := make(chan string, 4)
	s.CheckReadiness = func(_ context.Context, repo string) error { ran <- "check " + repo; return nil }
	s.FileReadiness = func(_ context.Context, repo string, queue bool) (string, error) {
		ran <- fmt.Sprintf("file %s queue=%v", repo, queue)
		return "Opened 1 issue(s).", nil
	}
	next := func() string {
		select {
		case r := <-ran:
			for busy := true; busy; time.Sleep(5 * time.Millisecond) { // the run's result lands
				s.ready.mu.Lock()
				busy = s.ready.repo != ""
				s.ready.mu.Unlock()
			}
			return r
		case <-time.After(2 * time.Second):
			return ""
		}
	}
	ctx := context.Background()
	s.followOnce(ctx, "o/r")
	s.followOnce(ctx, "o/s") // never checked: nothing to follow
	if r := next(); r != "" {
		t.Fatalf("ran %q with nothing changed", r)
	}
	sha = "def5678"
	s.followOnce(ctx, "o/r")
	if r := next(); r != "check o/r" {
		t.Fatalf("ran %q", r)
	}
	s.ready.mu.Lock()
	res := s.ready.results["o/r"]
	s.ready.mu.Unlock()
	if !strings.Contains(res, "main moved from abc1234 to def5678") {
		t.Fatalf("result = %q", res)
	}
	if err := readiness.Follow(s.ReadinessDir, "o/r"); err != nil {
		t.Fatal(err)
	}
	s.followOnce(ctx, "o/r")
	if r := next(); r != "file o/r queue=true" {
		t.Fatalf("ran %q", r)
	}
}

// A blocker split into pieces follows its pieces, even once splitting
// closed its issue. While the repo isn't ready, a piece says it blocks
// setup (and, for lint that can be baselined, that the baseline is the
// way), and any other work says it waits for setup.
func TestSplitBlockerAndWorkWaitingForSetup(t *testing.T) {
	t.Parallel()
	base, st, s := readinessServer(t)
	s.CanBaseline = func(string) bool { return true }
	s.LintBaseline = func(context.Context, string) (string, error) { return "", nil }
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
	if f := got.Repos[0].Steps[1].Blockers[0]; f.Status != "split" || f.SplitTotal != 1 || f.SplitDone != 0 {
		t.Fatalf("lint = %+v", f)
	}
	var d APIItemDetail
	getJSON(t, base+"/api/v1/items/"+strconv.FormatInt(piece.ID, 10), &d)
	if d.Item.Setup != "blocks" || !strings.Contains(d.Item.SetupNote, "Accept today's offenses") {
		t.Fatalf("piece = %q %q", d.Item.Setup, d.Item.SetupNote)
	}
	getJSON(t, base+"/api/v1/items/"+strconv.FormatInt(task.ID, 10), &d)
	if d.Item.Setup != "held" || !strings.Contains(d.Item.SetupNote, "starts by itself") {
		t.Fatalf("task = %q %q", d.Item.Setup, d.Item.SetupNote)
	}
}
