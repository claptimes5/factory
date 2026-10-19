package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
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
	if got.Banner != "not_ready" || got.CanCheck || !strings.Contains(got.CheckNote, "terminal") || len(got.Repos) != 2 {
		t.Fatalf("readiness = %+v", got)
	}
	r, s := got.Repos[0], got.Repos[1]
	if s.State != "unchecked" || s.Command != "./factory readiness o/s" || len(s.Steps) != 0 {
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
	if r.Unqueued != 1 || r.QueueCommand != "./factory readiness -queue o/r" {
		t.Fatalf("unqueued %d, queue %q", r.Unqueued, r.QueueCommand)
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
	// A daemon that can't run it says to use the terminal.
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
	if got.Repos[1].CheckError != "cloning o/s: no access" || got.Repos[1].State != "unchecked" {
		t.Fatalf("o/s = %+v", got.Repos[1])
	}
	// Real work hides the banner.
	st.UpsertItem(ctx, "o/r", 50, "Add dark mode")
	getJSON(t, base+"/api/v1/readiness", &got)
	if got.Banner != "" {
		t.Fatalf("banner after real work = %q", got.Banner)
	}
}
