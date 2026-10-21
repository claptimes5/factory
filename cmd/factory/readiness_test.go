package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store/storetest"
)

// With -queue, a ready repo's gaps all go to the factory; a repo that
// can't be checked doesn't stop the others.
func TestRunReadinessQueue(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []config.Repo{{Name: "o/broken"}, {Name: "o/r"}}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	assess = func(_ context.Context, _ *config.Config, p *pipeline.Pipeline, repo string, _ io.Writer) (*readiness.Report, error) {
		if !strings.HasPrefix(p.JobPrefix, "ready") {
			t.Errorf("job prefix %q would collide with the daemon's containers", p.JobPrefix)
		}
		if repo == "o/broken" {
			return nil, errors.New("clone failed")
		}
		return &readiness.Report{Repo: repo, Branch: "main", Ready: true, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}, nil
	}
	err = runReadiness(context.Background(), cfg, p, []string{"-queue"})
	if err == nil || !strings.Contains(err.Error(), "o/broken") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(f.issues, "|") != "/repos/o/r/issues Add end-to-end smoke tests factory:readiness,factory:ready" {
		t.Fatalf("issues: %v", f.issues)
	}

	// An incomplete report files nothing.
	f.issues = nil
	assess = func(context.Context, *config.Config, *pipeline.Pipeline, string, io.Writer) (*readiness.Report, error) {
		return &readiness.Report{Repo: "o/r", Incomplete: true, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}, nil
	}
	if err := runReadiness(context.Background(), cfg, p, []string{"-file", "o/r"}); err != nil || len(f.issues) != 0 {
		t.Fatalf("err %v, issues %v", err, f.issues)
	}
}

// Gaps filed while the repo had blockers are queued once it's ready;
// ones the factory already picked up are left alone.
func TestRunReadinessQueuesWaiting(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, filed: []map[string]any{
		{"number": 3, "title": "Start a feature map", "body": "x\n<!-- factory-readiness: feature-map -->", "state": "open", "labels": []map[string]string{{"name": "factory:readiness"}}},
		{"number": 4, "title": "Commit a lockfile", "body": "<!-- factory-readiness: lockfile -->", "state": "open", "labels": []map[string]string{{"name": "factory:readiness"}, {"name": "factory:running"}}},
		{"number": 5, "title": "Someone else's", "state": "open", "labels": []map[string]string{{"name": "factory:readiness"}}},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	assess = func(context.Context, *config.Config, *pipeline.Pipeline, string, io.Writer) (*readiness.Report, error) {
		return &readiness.Report{Repo: "o/r", Ready: true, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map"}},
		}}, nil
	}
	if err := runReadiness(context.Background(), cfg, p, []string{"-queue"}); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 0 || strings.Join(f.labeled, "|") != "/repos/o/r/issues/3/labels factory:ready" {
		t.Fatalf("issues %v, labeled %v", f.issues, f.labeled)
	}
}

// The saved report links each finding to the issue filed for it, old or
// new, so the dashboard and status can follow the work; status sums it up.
func TestReadinessLinksIssuesAndStatusSumsUp(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, filed: []map[string]any{
		{"number": 2, "title": "Start a feature map", "body": "<!-- factory-readiness: feature-map -->", "state": "closed", "state_reason": "not_planned"},
		{"number": 3, "title": "Start a feature map", "body": "x\n<!-- factory-readiness: feature-map -->", "state": "open"},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	if l := readinessLines(ctx, st, cfg); l != nil {
		t.Fatalf("never checked: %q", l)
	}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	report := func() *readiness.Report {
		return &readiness.Report{Repo: "o/r", Branch: "main", Findings: []readiness.Finding{
			{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on main", Issue: &readiness.Issue{Key: "check:lint", Title: "Make the lint check pass on main"}},
			{Level: readiness.Gap, Area: "feature map", Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map"}},
			{Level: readiness.Good, Area: "install", Title: "Installs from a clean checkout"},
		}}
	}
	assess = func(_ context.Context, cfg *config.Config, _ *pipeline.Pipeline, _ string, _ io.Writer) (*readiness.Report, error) {
		r := report()
		_, err := readiness.Save(readinessDir(cfg), r)
		return r, err
	}
	if err := runReadiness(ctx, cfg, p, []string{"-queue"}); err != nil {
		t.Fatal(err)
	}
	r, err := readiness.Load(readinessDir(cfg), "o/r")
	if err != nil || r == nil {
		t.Fatalf("report %v, %v", r, err)
	}
	if lint, fm := r.Findings[0].Issue, r.Findings[1].Issue; lint.Number != 101 || lint.State != "open" || fm.Number != 3 || fm.State != "open" {
		t.Fatalf("linked %+v %+v", lint, fm)
	}
	if fmt.Sprint(r.Filed) != "[2 3 101]" {
		t.Fatalf("filed %v", r.Filed)
	}

	it, _, _ := st.UpsertItem(ctx, "o/r", 101, "Make the lint check pass on main")
	st.SetStage(ctx, it.ID, state.Ready, "daemon", "")
	st.SetStage(ctx, it.ID, state.Implementing, "daemon", "")
	want := "setup o/r:\t1 of 1 setup steps done, not ready: lint fails on main (#101, implementing)"
	if got := strings.Join(readinessLines(ctx, st, cfg), "\n"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	// The plain check links too, without filing.
	f.issues = nil
	assess = func(_ context.Context, cfg *config.Config, _ *pipeline.Pipeline, _ string, _ io.Writer) (*readiness.Report, error) {
		r := report()
		r.Findings = r.Findings[1:]
		r.Findings = append(r.Findings, readiness.Finding{Level: readiness.Good, Area: "checks", Title: "Every required check passes on main: lint"})
		r.Ready = true
		_, err := readiness.Save(readinessDir(cfg), r)
		return r, err
	}
	if err := runReadiness(ctx, cfg, p, nil); err != nil || len(f.issues) != 0 {
		t.Fatalf("err %v, issues %v", err, f.issues)
	}
	want = "setup o/r:\t1 of 1 setup steps done, ready; add the factory:ready label to an issue to start"
	if got := strings.Join(readinessLines(ctx, st, cfg), "\n"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if r, _ := readiness.Load(readinessDir(cfg), "o/r"); r.Findings[0].Issue.Number != 3 {
		t.Fatalf("not linked: %+v", r.Findings[0].Issue)
	}
}

// The dashboard's File and start work does what -queue does, and says so.
func TestFileReadinessFromTheDashboard(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, filed: []map[string]any{
		{"number": 3, "title": "Start a feature map", "body": "x\n<!-- factory-readiness: feature-map -->", "state": "open", "labels": []map[string]string{{"name": "factory:readiness"}}},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	incomplete := false
	assess = func(_ context.Context, _ *config.Config, p *pipeline.Pipeline, repo string, _ io.Writer) (*readiness.Report, error) {
		if !strings.HasPrefix(p.JobPrefix, "ready") {
			t.Errorf("job prefix %q would collide with the daemon's containers", p.JobPrefix)
		}
		return &readiness.Report{Repo: repo, Branch: "main", Ready: true, Incomplete: incomplete, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map"}},
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}, nil
	}
	msg, err := fileReadiness(context.Background(), cfg, p, "o/r", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "Opened 1 issue(s) on o/r (1 already filed). Labeled 1 issue(s) filed earlier factory:ready, so the factory works on them." {
		t.Fatal(msg)
	}
	if strings.Join(f.issues, "|") != "/repos/o/r/issues Add end-to-end smoke tests factory:readiness,factory:ready" ||
		strings.Join(f.labeled, "|") != "/repos/o/r/issues/3/labels factory:ready" {
		t.Fatalf("issues %v, labeled %v", f.issues, f.labeled)
	}
	r, err := readiness.Load(readinessDir(cfg), "o/r")
	if err != nil || r == nil || r.Findings[1].Issue.Number == 0 {
		t.Fatalf("saved %+v, %v", r, err)
	}

	f.issues, f.labeled = nil, nil
	incomplete = true
	if msg, err := fileReadiness(context.Background(), cfg, p, "o/r", false, io.Discard); err != nil || !strings.Contains(msg, "nothing was filed") || len(f.issues) != 0 {
		t.Fatalf("msg %q, err %v, issues %v", msg, err, f.issues)
	}
}

// closingForge records what closeFixed closes and says; nothing else of
// the forge is used.
type closingForge struct {
	forge
	open     map[int]bool
	closed   []int
	comments map[int]string
}

func (f *closingForge) CloseIssue(_ context.Context, n int) error {
	f.closed, f.open[n] = append(f.closed, n), false
	return nil
}
func (f *closingForge) Comment(_ context.Context, n int, body string) error {
	f.comments[n] = body
	return nil
}
func (f *closingForge) GetIssue(_ context.Context, n int) (*github.Issue, error) {
	st := "closed"
	if f.open[n] {
		st = "open"
	}
	return &github.Issue{Number: n, State: st}, nil
}

// Once a check the setup check filed an issue for passes on the base
// branch, that issue and the issues it was split into are closed.
func TestCloseFixedClosesTheIssueAndItsSplit(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	lint, _, _ := st.UpsertItem(ctx, "o/r", 9, "Make the lint check pass on master")
	for _, n := range []int{12, 13} {
		c, _, _ := st.UpsertItem(ctx, "o/r", n, "part")
		st.SetParent(ctx, c.ID, lint.ID)
	}
	f := &closingForge{open: map[int]bool{9: true, 10: true, 12: true, 13: false}, comments: map[int]string{}}
	filed := []github.Issue{
		{Number: 9, State: "open", Body: "x\n<!-- factory-readiness: check:lint -->"},
		{Number: 10, State: "open", Body: "<!-- factory-readiness: e2e -->"},
		{Number: 11, State: "open", Body: "<!-- factory-readiness: check:test -->"},
	}
	r := &readiness.Report{Repo: "o/r", Branch: "master", SHA: "abc1234567", Runs: []readiness.Run{{Gate: "lint", Passed: true}, {Gate: "test", Passed: false}}}
	closeFixed(ctx, f, st, r, filed)
	if fmt.Sprint(f.closed) != "[9 12]" || filed[0].State != "closed" || filed[2].State != "open" {
		t.Fatalf("closed %v, filed %+v", f.closed, filed)
	}
	if !strings.Contains(f.comments[9], "`lint` passes on `master` now, at abc1234") || !strings.Contains(f.comments[12], "closed with #9") {
		t.Fatalf("comments = %v", f.comments)
	}
	// An incomplete check closes nothing.
	f.closed = nil
	filed[2].State = "open"
	closeFixed(ctx, f, st, &readiness.Report{Repo: "o/r", Incomplete: true, Runs: []readiness.Run{{Gate: "test", Passed: true}}}, filed)
	if len(f.closed) != 0 {
		t.Fatalf("closed %v from an incomplete check", f.closed)
	}
}

// The first run makes factory.yaml from the example beside it, and never
// touches one that's there.
func TestFirstConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.yaml")
	if err := firstConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("made a config with no example to make it from")
	}
	os.WriteFile(filepath.Join(dir, "factory.example.yaml"), []byte("repo: you/app\n"), 0o644)
	if err := firstConfig(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "repo: you/app\n" {
		t.Fatalf("factory.yaml = %q", b)
	}
	os.WriteFile(path, []byte("repo: me/mine\n"), 0o644)
	firstConfig(path)
	if b, _ := os.ReadFile(path); string(b) != "repo: me/mine\n" {
		t.Fatalf("an existing config was overwritten: %q", b)
	}
}

// baselineForge is closingForge that files issues and knows one pull request.
type baselineForge struct {
	closingForge
	pr      *github.PR
	created []string // title and labels of each issue filed
}

func (f *baselineForge) GetPR(context.Context, int) (*github.PR, error) { return f.pr, nil }
func (f *baselineForge) CreateIssue(_ context.Context, title, _ string, labels []string) (int, error) {
	f.created = append(f.created, title+" "+strings.Join(labels, ","))
	return 30, nil
}

// Once the commit checked has the lint baseline, the page stops asking to
// merge it, the pieces lint was split into close, and what lint still
// finds gets an issue of its own for the factory, once. Closed without
// merging, the page stops asking for it too.
func TestSettleBaseline(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DataDir: t.TempDir()}
	cfg.Intake.Label = "factory:ready"
	dir := readinessDir(cfg)
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	lint, _, _ := st.UpsertItem(ctx, "o/r", 9, "Make the lint check pass on master")
	for _, n := range []int{12, 13} {
		c, _, _ := st.UpsertItem(ctx, "o/r", n, "RuboCop piece")
		st.SetParent(ctx, c.ID, lint.ID)
	}
	f := &baselineForge{closingForge: closingForge{open: map[int]bool{12: true, 13: true}, comments: map[int]string{}}}
	filed := []github.Issue{{Number: 9, State: "closed", Body: "x\n<!-- factory-readiness: check:lint -->"}}
	lintFails := readiness.Finding{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on master",
		Issue: &readiness.Issue{Key: "check:lint", Title: "Make the lint check pass on master", Body: "1 offense"}}
	readiness.SaveBaselinePR(dir, "o/r", "https://gitlab.com/o/r/-/merge_requests/42")

	// Not on master yet, and still open: nothing changes.
	f.pr = &github.PR{Number: 42, State: "open"}
	r := &readiness.Report{Repo: "o/r", Branch: "master", SHA: "c0ffee1234", Findings: []readiness.Finding{lintFails}}
	if got := settleBaseline(ctx, cfg, f, st, r, filed); len(got) != 1 || readiness.BaselinePR(dir, "o/r") == "" || len(f.closed) != 0 {
		t.Fatalf("before the merge: filed %v, closed %v", got, f.closed)
	}

	r.Baselined = true
	got := settleBaseline(ctx, cfg, f, st, r, filed)
	if readiness.BaselinePR(dir, "o/r") != "" {
		t.Fatal("still asks to merge a baseline that's in")
	}
	if fmt.Sprint(f.closed) != "[12 13]" || !strings.Contains(f.comments[12], "lint baseline is on `master` now (at c0ffee1)") {
		t.Fatalf("closed %v, comments %v", f.closed, f.comments)
	}
	if len(got) != 2 || got[1].State != "open" || strings.Join(f.created, "|") != "Fix the lint offenses on master since the baseline factory:readiness,factory:ready" {
		t.Fatalf("filed %v, created %v", got, f.created)
	}
	// It has its issue now: checking again files no other.
	settleBaseline(ctx, cfg, f, st, r, got)
	if len(f.created) != 1 {
		t.Fatalf("filed again: %v", f.created)
	}

	// A baseline closed without merging stops being asked for.
	readiness.SaveBaselinePR(dir, "o/r", "https://gitlab.com/o/r/-/merge_requests/43")
	f.pr = &github.PR{Number: 43, State: "closed"}
	settleBaseline(ctx, cfg, f, st, &readiness.Report{Repo: "o/r", Findings: []readiness.Finding{lintFails}}, filed)
	if readiness.BaselinePR(dir, "o/r") != "" {
		t.Fatal("still asks to merge a closed baseline")
	}
}

// A check, by the dashboard or by itself, files the factory's work: an
// issue for each blocker a change can fix, labeled for the factory, while
// gaps wait unlabeled until the repo is ready.
func TestRecheckFilesTheFactorysWork(t *testing.T) {
	// Flaky tests were filed before, unlabeled, by a plain check.
	f := &fakeGitHub{labels: map[string]bool{}, filed: []map[string]any{
		{"number": 13, "title": "Fix flaky tests", "body": "x\n<!-- factory-readiness: flaky -->", "state": "open", "labels": []map[string]string{{"name": "factory:readiness"}}},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	assess = func(_ context.Context, cfg *config.Config, _ *pipeline.Pipeline, repo string, _ io.Writer) (*readiness.Report, error) {
		r := &readiness.Report{Repo: repo, Branch: "main", Findings: []readiness.Finding{
			{Level: readiness.Blocker, Area: "tests", Title: "Tests are flaky", Issue: &readiness.Issue{Key: "flaky", Title: "Fix flaky tests"}},
			{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on main", Issue: &readiness.Issue{Key: "check:lint", Title: "Make the lint check pass on main"}},
			{Level: readiness.Blocker, Area: "services", Title: "`test` can't reach a service it needs"},
			{Level: readiness.Gap, Area: "e2e", Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}
		_, err := readiness.Save(readinessDir(cfg), r)
		return r, err
	}
	var out strings.Builder
	if err := recheckReadiness(context.Background(), cfg, p, "o/r", &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.issues, "|"); got != "/repos/o/r/issues Make the lint check pass on main factory:readiness,factory:ready|/repos/o/r/issues Add end-to-end smoke tests factory:readiness" {
		t.Fatalf("issues %s", got)
	}
	if !strings.Contains(out.String(), "opened 2 issue(s)") || strings.Join(f.labeled, "|") != "/repos/o/r/issues/13/labels factory:ready" {
		t.Fatalf("said %q, labeled %v", out.String(), f.labeled)
	}
}
