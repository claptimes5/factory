package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/web"
)

// labelHost is a GitHub with some labels, the factory's and others.
type labelHost struct {
	mu      sync.Mutex
	labels  [][2]string // name, description
	open    map[string]int
	deleted []string
	refuse  string // a label whose delete is refused
}

func (h *labelHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/repos/o/r/labels":
		var out []map[string]string
		for _, l := range h.labels {
			out = append(out, map[string]string{"name": l[0], "description": l[1]})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
		out := []map[string]int{}
		for i := range h.open[r.URL.Query().Get("labels")] {
			out = append(out, map[string]int{"number": i + 1})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
		name := strings.TrimPrefix(r.URL.Path, "/repos/o/r/labels/")
		if name == h.refuse {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		h.deleted = append(h.deleted, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// imageDocker is Docker with some tags, each "id\tsize\tmark", and old
// untagged copies marked as the factory's.
type imageDocker struct {
	mu       sync.Mutex
	tags     map[string]string
	dangling string
	removed  []string
}

func (d *imageDocker) run(out io.Writer, name string, args ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(a, "image ls"):
		if !strings.Contains(a, "dangling=true") || !strings.Contains(a, "label="+imageLabel+"=1") {
			return errors.New("unexpected " + a)
		}
		io.WriteString(out, d.dangling)
	case strings.HasPrefix(a, "image inspect --format"):
		info, ok := d.tags[args[len(args)-1]]
		if !ok {
			return errors.New("no such image")
		}
		io.WriteString(out, info+"\n")
	case strings.HasPrefix(a, "image rm"):
		d.removed = append(d.removed, args[2])
	}
	return nil
}

func uninstallSetup(t *testing.T, h *labelHost) (*config.Config, string, string, *imageDocker) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := &imageDocker{tags: map[string]string{"factory-job:latest": "sha256:def456789012345678\t1500000000\t1"}, dangling: "sha256:old456789012aaaa\t2.01GB\n"}
	oldAPI, oldWeb, oldRun, oldAnswers := githubAPI, githubWeb, runCmd, factoryAnswers
	githubAPI, githubWeb, runCmd = srv.URL, "https://github.example", d.run
	factoryAnswers = func(string) bool { return false }
	t.Cleanup(func() { githubAPI, githubWeb, runCmd, factoryAnswers = oldAPI, oldWeb, oldRun, oldAnswers })
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	t.Setenv("FACTORY_GITHUB_APP_ID", "")
	t.Setenv("FACTORY_GITHUB_APP_SLUG", "")
	cfgPath := testConfig(t, "o/r")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(filepath.Dir(cfgPath), "factory.env")
	os.WriteFile(envPath, []byte("GITHUB_TOKEN=ghp_test\n"), 0o600)
	data := cfg.Path(cfg.DataDir)
	os.MkdirAll(filepath.Join(data, "jobs", "1"), 0o755)
	os.WriteFile(filepath.Join(data, "jobs", "1", "log.jsonl"), []byte("{}\n"), 0o644)
	st, err := store.Open(filepath.Join(data, "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	return cfg, cfgPath, envPath, d
}

func newLabelHost() *labelHost {
	return &labelHost{labels: [][2]string{{"bug", ""}, {"factory:ready", "Factory: pick this up"}, {"factory:parked", "Factory: needs a human"}, {"factory:mine", ""}},
		open: map[string]int{"factory:parked": 2}}
}

// The dry run lists only the factory's labels, with their open counts,
// and the images it built, labelled or not, and removes nothing.
func TestUninstallDryRun(t *testing.T) {
	h := newLabelHost()
	cfg, cfgPath, envPath, d := uninstallSetup(t, h)
	var out bytes.Buffer
	if err := uninstall(context.Background(), &out, strings.NewReader(""), cfgPath, cfg, envPath, []string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"deleting a label takes it off every issue and PR that has it",
		"o/r  factory:ready  (0 open)", "o/r  factory:parked  (2 open)",
		"factory-job:latest  1.5GB", "old456789012 (an old copy)  2.01GB",
		"kept: ", "Add --data to remove it too",
		"Revoke the GitHub token", "delete " + cfgPath + " and " + envPath,
		"Nothing was removed (--dry-run).",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Only labels the factory defines: not bug, nor one a person named factory:something.
	if strings.Contains(got, "factory:mine") || strings.Contains(got, " bug ") {
		t.Errorf("listed a label that isn't the factory's:\n%s", got)
	}
	// The tag comes before the old copy, which may need it gone first.
	if strings.Index(got, "factory-job:latest") > strings.Index(got, "old456789012") {
		t.Errorf("old copy listed first:\n%s", got)
	}
	if len(h.deleted) != 0 || len(d.removed) != 0 {
		t.Fatalf("a dry run removed %q %q", h.deleted, d.removed)
	}
}

// Nothing goes without the word, and with it each removal is reported.
func TestUninstallRemovesOnConfirm(t *testing.T) {
	h := newLabelHost()
	h.refuse = "factory:ready"
	cfg, cfgPath, envPath, d := uninstallSetup(t, h)
	ctx := context.Background()
	var out bytes.Buffer
	if err := uninstall(ctx, &out, strings.NewReader("yes\n"), cfgPath, cfg, envPath, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing was removed.") || len(h.deleted) != 0 || len(d.removed) != 0 {
		t.Fatalf("removed without the word: %q %q\n%s", h.deleted, d.removed, out.String())
	}
	out.Reset()
	err := uninstall(ctx, &out, strings.NewReader("o/r\n"), cfgPath, cfg, envPath, nil)
	if err == nil || !strings.Contains(err.Error(), "1 of 4 couldn't be removed") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"removed  label factory:parked on o/r", "FAILED   label factory:ready on o/r: ", "removed  image factory-job:latest", "removed  image old456789012"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !slices.Equal(h.deleted, []string{"factory:parked"}) || !slices.Equal(d.removed, []string{"factory-job:latest", "old456789012"}) {
		t.Fatalf("deleted %q, removed %q", h.deleted, d.removed)
	}
	// Data is a separate choice, and settings are never deleted.
	if _, err := os.Stat(cfg.Path(cfg.DataDir)); err != nil {
		t.Fatal("removed the data without --data")
	}
	for _, p := range []string{cfgPath, envPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("deleted %s", p)
		}
	}
}

func TestUninstallRemovesDataWhenAsked(t *testing.T) {
	cfg, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	var out bytes.Buffer
	if err := uninstall(context.Background(), &out, strings.NewReader("remove\n"), cfgPath, cfg, envPath, []string{"--data"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, err := os.Stat(cfg.Path(cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatalf("data kept: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatal("deleted the config")
	}
}

// A data_dir holding the config isn't the factory's alone to delete.
func TestUninstallKeepsADataDirWithTheConfig(t *testing.T) {
	cfg, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	cfg.DataDir = "."
	var out bytes.Buffer
	err := uninstall(context.Background(), &out, strings.NewReader("remove\n"), cfgPath, cfg, envPath, []string{"--data"})
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") || !strings.Contains(out.String(), "isn't removed: it holds factory.yaml") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatal("deleted the config")
	}
}

// Running jobs, or a running factory, stop it before anything goes.
func TestUninstallRefusesWhileBusy(t *testing.T) {
	h := newLabelHost()
	cfg, cfgPath, envPath, d := uninstallSetup(t, h)
	ctx := context.Background()
	st, err := store.Open(filepath.Join(cfg.Path(cfg.DataDir), "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.EnqueueJob(ctx, store.Job{Kind: "triage", Harness: "claude", Model: "m", BudgetUSD: 1})
	st.ClaimJob(ctx)
	st.Close()
	var out bytes.Buffer
	err = uninstall(ctx, &out, strings.NewReader("remove\n"), cfgPath, cfg, envPath, nil)
	if err == nil || !strings.Contains(err.Error(), "job(s) are running (#"+itoa(id)+")") {
		t.Fatalf("err = %v", err)
	}
	factoryAnswers = func(string) bool { return true }
	err = uninstall(ctx, &out, strings.NewReader("remove\n"), cfgPath, cfg, envPath, nil)
	if err == nil || !strings.Contains(err.Error(), "something is listening on") {
		t.Fatalf("err = %v", err)
	}
	if len(h.deleted) != 0 || len(d.removed) != 0 {
		t.Fatalf("removed %q %q while busy", h.deleted, d.removed)
	}
}

// From the dashboard, the running factory keeps its database and
// sign-ins; the job data goes, and it says to stop the factory.
func TestUninstallFromTheDashboard(t *testing.T) {
	h := newLabelHost()
	cfg, cfgPath, envPath, _ := uninstallSetup(t, h)
	t.Setenv("FACTORY_GITHUB_APP_ID", "12")
	t.Setenv("FACTORY_GITHUB_APP_SLUG", "my-factory")
	t.Setenv("FACTORY_GITHUB_INSTALLATION_ID", "34")
	data := cfg.Path(cfg.DataDir)
	os.WriteFile(filepath.Join(data, "factory.db"), []byte("db"), 0o644)
	busy := ""
	u := newUninstaller(cfg, cfgPath, envPath, true, func(context.Context) string { return busy })
	u.forgeFor = func(repo string) (forge, error) { return newForge(cfg, repo, github.StaticToken("ghp_test")), nil }
	ctx := context.Background()
	p := u.Plan(ctx, "")
	var links []string
	for _, m := range p.Manual {
		links = append(links, m.URL)
	}
	for _, want := range []string{"https://github.example/settings/apps/my-factory", "https://github.example/organizations/o/settings/apps/my-factory", "https://github.example/settings/installations/34"} {
		if !slices.Contains(links, want) {
			t.Errorf("no link %s in %q", want, links)
		}
	}
	if !strings.Contains(p.Data.Note, "The database") {
		t.Errorf("data note = %q", p.Data.Note)
	}
	busy = "Not now: 1 job(s) are running."
	if _, err := u.Remove(ctx, true, ""); err == nil || err.Error() != busy {
		t.Fatalf("err = %v", err)
	}
	busy = ""
	res, err := u.Remove(ctx, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "jobs")); !os.IsNotExist(err) {
		t.Fatal("job data kept")
	}
	if _, err := os.Stat(filepath.Join(data, "factory.db")); err != nil {
		t.Fatal("removed the running factory's database")
	}
	if !strings.HasPrefix(res.Manual[0].Text, "Stop the factory now") {
		t.Fatalf("manual = %+v", res.Manual)
	}
}

// gitlabLabels is a GitLab project whose factory:ready is its group's.
type gitlabLabels struct {
	mu      sync.Mutex
	deleted []string
}

func (g *gitlabLabels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/labels"):
		w.Write([]byte(`[{"name":"factory:ready","description":"Factory: pick this up","is_project_label":false},` +
			`{"name":"factory:parked","description":"Factory: needs a human","is_project_label":true},` +
			`{"name":"FACTORY:RETRY","description":"","is_project_label":true}]`))
	case r.Method == "GET":
		w.Write([]byte(`[]`))
	case r.Method == "DELETE":
		g.deleted = append(g.deleted, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}
}

// A GitLab group's label stays, said as such, and is never reported as
// removed; GitLab names match exactly.
func TestUninstallLeavesGitLabGroupLabels(t *testing.T) {
	g := &gitlabLabels{}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	old := gitlabAPI
	gitlabAPI = srv.URL
	t.Cleanup(func() { gitlabAPI = old })
	cfgPath := testConfig(t, "gitlab.example/team/web")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runner.Kind = "local"
	u := newUninstaller(cfg, cfgPath, filepath.Join(filepath.Dir(cfgPath), "factory.env"), false, nil)
	u.forgeFor = func(repo string) (forge, error) { return newForge(cfg, repo, github.StaticToken("glpat")), nil }
	ctx := context.Background()
	p := u.Plan(ctx, "")
	if len(p.Labels) != 1 || p.Labels[0].Name != "factory:parked" {
		t.Fatalf("labels = %+v", p.Labels)
	}
	if len(p.Kept) != 1 || p.Kept[0].What != "label factory:ready on gitlab.example/team/web" || !strings.Contains(p.Kept[0].Why, "defined in group team") || !strings.Contains(p.Kept[0].Why, "not removed") {
		t.Fatalf("kept = %+v", p.Kept)
	}
	res, err := u.Remove(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Done {
		if strings.Contains(d.What, "factory:ready") || strings.Contains(d.What, "FACTORY:RETRY") {
			t.Fatalf("reported %+v", d)
		}
	}
	if len(g.deleted) != 1 || !strings.HasSuffix(g.deleted[0], "/labels/factory:parked") {
		t.Fatalf("deleted %q", g.deleted)
	}
}

// An intake label you named stays unless setup made it.
func TestUninstallLeavesYourIntakeLabel(t *testing.T) {
	h := &labelHost{labels: [][2]string{{"ready", "Ours, since 2019"}, {"factory:parked", "Factory: needs a human"}}}
	cfg, cfgPath, envPath, _ := uninstallSetup(t, h)
	cfg.Intake.Label = "ready"
	u := newUninstaller(cfg, cfgPath, envPath, false, nil)
	ctx := context.Background()
	p := u.Plan(ctx, "")
	if len(p.Labels) != 1 || p.Labels[0].Name != "factory:parked" || len(p.Kept) != 1 || !strings.Contains(p.Kept[0].Why, "yours to delete by hand") {
		t.Fatalf("labels = %+v, kept %+v", p.Labels, p.Kept)
	}
	if _, err := u.Remove(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.deleted, []string{"factory:parked"}) {
		t.Fatalf("deleted %q", h.deleted)
	}
	// One setup created carries its description, so it goes.
	h.labels = [][2]string{{"ready", "Factory: pick this up"}}
	h.deleted = nil
	if p := u.Plan(ctx, ""); len(p.Labels) != 1 || p.Labels[0].Name != "ready" || len(p.Kept) != 0 {
		t.Fatalf("labels = %+v, kept %+v", p.Labels, p.Kept)
	}
}

// Only the tags the factory builds go, never a repo's own image, and the
// runner image only with the factory's mark.
func TestUninstallImagesAreOnlyTheFactorys(t *testing.T) {
	cfg, cfgPath, envPath, d := uninstallSetup(t, newLabelHost())
	cfg.Repos = append(cfg.Repos,
		config.Repo{Name: "o/own", Image: "mine:1"},                                     // its own image, marked by its FROM
		config.Repo{Name: "o/node", Image: "factory-job-o__node", ImageFrom: "node:22"}) // built by the factory before the mark
	d.tags = map[string]string{
		"factory-job:latest":         "sha256:aaa111222333444\t1500000000\t",
		"mine:1":                     "sha256:bbb111222333444\t900000000\t1",
		"factory-job-o__node:latest": "sha256:ccc111222333444\t1200000000\t",
	}
	u := newUninstaller(cfg, cfgPath, envPath, false, nil)
	u.forgeFor = func(string) (forge, error) { return nil, errors.New("offline") }
	ctx := context.Background()
	p := u.Plan(ctx, "")
	var refs []string
	for _, im := range p.Images {
		refs = append(refs, im.Ref)
	}
	if !slices.Equal(refs, []string{"factory-job-o__node:latest", "old456789012"}) {
		t.Fatalf("images = %q", refs)
	}
	if !slices.ContainsFunc(p.Kept, func(k web.APIUninstallKept) bool {
		return k.What == "image factory-job:latest" && strings.Contains(k.Why, "not marked as built by the factory")
	}) {
		t.Fatalf("kept = %+v", p.Kept)
	}
	if _, err := u.Remove(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.removed, []string{"factory-job-o__node:latest", "old456789012"}) {
		t.Fatalf("removed %q", d.removed)
	}
	// Marked, the runner image goes too.
	d.tags["factory-job:latest"] = "sha256:aaa111222333444\t1500000000\t1"
	if p := u.Plan(ctx, ""); p.Images[0].Ref != "factory-job:latest" {
		t.Fatalf("images = %+v", p.Images)
	}
}

// The data folder goes only when it is plainly the factory's.
func TestUninstallDataMustBeTheFactorys(t *testing.T) {
	cfg, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	u := newUninstaller(cfg, cfgPath, envPath, false, nil)
	data := u.dataDir()
	if err := u.dataRemovable(data); err != nil {
		t.Fatalf("the factory's own folder: %v", err)
	}
	// Something the factory didn't make.
	os.WriteFile(filepath.Join(data, "notes.txt"), []byte("mine"), 0o644)
	if err := u.dataRemovable(data); err == nil || !strings.Contains(err.Error(), "it holds notes.txt, which the factory didn't make") {
		t.Fatalf("err = %v", err)
	}
	os.Remove(filepath.Join(data, "notes.txt"))
	// Home, by its name or through a link.
	t.Setenv("HOME", data)
	if err := u.dataRemovable(data); err == nil || !strings.Contains(err.Error(), "home folder") {
		t.Fatalf("err = %v", err)
	}
	link := filepath.Join(t.TempDir(), "home")
	os.Symlink(data, link)
	t.Setenv("HOME", link)
	if err := u.dataRemovable(data); err == nil || !strings.Contains(err.Error(), "home folder") {
		t.Fatalf("linked home: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	// No database: maybe not the factory's.
	os.Rename(filepath.Join(data, "factory.db"), filepath.Join(t.TempDir(), "f.db"))
	if err := u.dataRemovable(data); err == nil || !strings.Contains(err.Error(), "no factory.db") {
		t.Fatalf("err = %v", err)
	}
	// The dashboard checks too, before removing even its subfolders.
	u.serving = true
	if p := u.Plan(context.Background(), ""); !strings.Contains(p.Data.Blocked, "no factory.db") {
		t.Fatalf("data = %+v", p.Data)
	}
	// Asked for and blocked, it refuses the lot before any label goes.
	h := newLabelHost()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u.forgeFor = func(repo string) (forge, error) {
		return &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}, nil
	}
	if _, err := u.Remove(context.Background(), true, ""); err == nil || !strings.Contains(err.Error(), "nothing was removed") || !strings.Contains(err.Error(), "no factory.db") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "jobs")); err != nil {
		t.Fatal("removed job data from a folder that may not be the factory's")
	}
	if len(h.deleted) != 0 {
		t.Fatalf("deleted %q before refusing", h.deleted)
	}
	// Without the data, the rest still goes.
	if _, err := u.Remove(context.Background(), false, ""); err != nil || len(h.deleted) != 2 {
		t.Fatalf("err = %v, deleted %q", err, h.deleted)
	}
}

// What the factory leaves in its folder, dotfiles and old readiness
// clones included, keeps it the factory's.
func TestUninstallDataAllowsTheFactorysLeftovers(t *testing.T) {
	cfg, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	u := newUninstaller(cfg, cfgPath, envPath, false, nil)
	data := u.dataDir()
	os.WriteFile(filepath.Join(data, ".DS_Store"), nil, 0o644)
	os.MkdirAll(filepath.Join(data, "readiness-123"), 0o755)
	os.WriteFile(filepath.Join(data, "factory.db.bak-v3-20260101T000000"), nil, 0o644)
	if err := u.dataRemovable(data); err != nil {
		t.Fatal(err)
	}
}

// A compare run or readiness check in any process holds a removal off,
// and a lock left by a process that's gone doesn't.
func TestUninstallWaitsForRunsElsewhere(t *testing.T) {
	h := newLabelHost()
	cfg, cfgPath, envPath, _ := uninstallSetup(t, h)
	data := cfg.Path(cfg.DataDir)
	stale := filepath.Join(data, "readiness-9", runFile)
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("999999999"), 0o644)
	if runs := liveRuns(data); len(runs) != 0 {
		t.Fatalf("stale lock counted: %q", runs)
	}
	unlock, err := runLock(filepath.Join(data, "compare", "12-x"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = uninstall(context.Background(), &out, strings.NewReader("remove\n"), cfgPath, cfg, envPath, nil)
	if err == nil || !strings.Contains(err.Error(), "a compare run (12-x) is running") || len(h.deleted) != 0 {
		t.Fatalf("err = %v, deleted %q", err, h.deleted)
	}
	unlock()
	if runs := liveRuns(data); len(runs) != 0 {
		t.Fatalf("released lock counted: %q", runs)
	}
}

// A second Remove while one runs is refused.
func TestUninstallOneRemoveAtATime(t *testing.T) {
	cfg, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	in, out := make(chan struct{}), make(chan struct{})
	u := newUninstaller(cfg, cfgPath, envPath, true, nil)
	u.hold = func() func() { in <- struct{}{}; <-out; return func() {} }
	done := make(chan error, 1)
	go func() { _, err := u.Remove(context.Background(), false, ""); done <- err }()
	<-in
	if _, err := u.Remove(context.Background(), false, ""); err == nil || !strings.Contains(err.Error(), "already removing") {
		t.Fatalf("second Remove = %v", err)
	}
	close(out)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Work that starts while labels go stops the rest.
func TestUninstallChecksBusyBeforeEachStep(t *testing.T) {
	cfg, cfgPath, envPath, d := uninstallSetup(t, newLabelHost())
	calls := 0
	held, released := false, false
	u := newUninstaller(cfg, cfgPath, envPath, true, func(context.Context) string {
		calls++
		if calls > 1 {
			return "Not now: a readiness check is running."
		}
		return ""
	})
	u.hold = func() func() { held = true; return func() { released = true } }
	res, err := u.Remove(context.Background(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !held || !released || len(d.removed) != 0 {
		t.Fatalf("held %v released %v removed %q", held, released, d.removed)
	}
	if last := res.Done[len(res.Done)-1]; last.OK || !strings.Contains(last.Error, "a readiness check is running") {
		t.Fatalf("result = %+v", res.Done)
	}
}

// A count at the cap reads as that many or more.
func TestUninstallShowsACappedCount(t *testing.T) {
	h := newLabelHost()
	h.open["factory:parked"] = 1000
	cfg, cfgPath, envPath, _ := uninstallSetup(t, h)
	var out bytes.Buffer
	if err := uninstall(context.Background(), &out, strings.NewReader(""), cfgPath, cfg, envPath, []string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "factory:parked  (1000+ open)") {
		t.Fatalf("out:\n%s", out.String())
	}
}

// A restart keeps the pid, so a lock with our pid that this process
// didn't take was left by the one before: not live, and swept at start.
func TestOwnPidLockFromBeforeARestartIsStale(t *testing.T) {
	data := t.TempDir()
	left := filepath.Join(data, "readiness-1")
	os.MkdirAll(left, 0o755)
	os.WriteFile(filepath.Join(left, runFile), []byte(itoa(int64(os.Getpid()))), 0o644)
	if runs := liveRuns(data); len(runs) != 0 {
		t.Fatalf("a lock from before the restart counted: %q", runs)
	}
	// One this process holds counts until released.
	held := filepath.Join(data, "readiness-2")
	unlock, err := runLock(held)
	if err != nil {
		t.Fatal(err)
	}
	if runs := liveRuns(data); len(runs) != 1 {
		t.Fatalf("runs = %q", runs)
	}
	sweepReadiness(data)
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatal("left folder kept")
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatal("swept a running check's folder")
	}
	unlock()
	if runs := liveRuns(data); len(runs) != 0 {
		t.Fatalf("runs = %q", runs)
	}
}

// --repo removes one repo's labels, images and files, and the repo from
// factory.yaml; what the other repos use stays.
func TestUninstallRepoRemovesOnlyItsShare(t *testing.T) {
	h := newLabelHost()
	_, cfgPath, envPath, d := uninstallSetup(t, h)
	if err := setRepos(cfgPath, []string{"o/r", "o/other"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d.tags["factory-job-o__r:latest"] = "sha256:abc456789012345678\t900000000\t1"
	data := cfg.Path(cfg.DataDir)
	for _, p := range []string{"cache/o__r/npm", "cache/o__other/npm"} {
		os.MkdirAll(filepath.Join(data, p), 0o755)
	}
	os.MkdirAll(filepath.Join(data, "readiness"), 0o755)
	os.WriteFile(filepath.Join(data, "readiness", "o__r.json"), []byte("{}"), 0o644)
	var out bytes.Buffer
	if err := uninstall(context.Background(), &out, strings.NewReader("o/r\n"), cfgPath, cfg, envPath, []string{"--repo", "o/r"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !slices.Equal(h.deleted, []string{"factory:ready", "factory:parked"}) || !slices.Equal(d.removed, []string{"factory-job-o__r:latest"}) {
		t.Fatalf("deleted %q, removed %q\n%s", h.deleted, d.removed, out.String())
	}
	for p, gone := range map[string]bool{"cache/o__r": true, "readiness/o__r.json": true, "cache/o__other": false, "factory.db": false} {
		if _, err := os.Stat(filepath.Join(data, p)); os.IsNotExist(err) != gone {
			t.Errorf("%s gone = %v, want %v", p, !gone, gone)
		}
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.RepoNames(), []string{"o/other"}) {
		t.Fatalf("repos = %q", after.RepoNames())
	}
}

// The last repo can't leave: that's removing the factory.
func TestUninstallRepoRefusesTheOnlyRepo(t *testing.T) {
	h := newLabelHost()
	cfg, cfgPath, envPath, _ := uninstallSetup(t, h)
	var out bytes.Buffer
	err := uninstall(context.Background(), &out, strings.NewReader("o/r\n"), cfgPath, cfg, envPath, []string{"--repo", "o/r"})
	if err == nil || !strings.Contains(err.Error(), "the only repo") || len(h.deleted) != 0 {
		t.Fatalf("err = %v, deleted %q", err, h.deleted)
	}
}

// The dashboard removes repos before the factory restarts, with the config
// it started with: a second removal keeps the first one out.
func TestUninstallRepoTwiceBeforeARestart(t *testing.T) {
	_, cfgPath, envPath, _ := uninstallSetup(t, newLabelHost())
	if err := setRepos(cfgPath, []string{"o/r", "o/a", "o/b"}); err != nil {
		t.Fatal(err)
	}
	started, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	u := newUninstaller(started, cfgPath, envPath, true, nil)
	for _, repo := range []string{"o/a", "o/b"} {
		if _, err := u.Remove(context.Background(), false, repo); err != nil {
			t.Fatal(err)
		}
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.RepoNames(), []string{"o/r"}) {
		t.Fatalf("repos = %q", after.RepoNames())
	}
	if u.Plan(context.Background(), "o/a").Listed {
		t.Fatal("o/a is still listed after it left factory.yaml")
	}
}
