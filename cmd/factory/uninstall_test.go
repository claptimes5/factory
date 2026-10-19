package main

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// labelHost is a GitHub with some labels, the factory's and others.
type labelHost struct {
	mu      sync.Mutex
	labels  []string
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
			out = append(out, map[string]string{"name": l})
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

// imageDocker is Docker with one old labelled copy, and the job image
// built before the label existed.
type imageDocker struct {
	mu      sync.Mutex
	removed []string
}

func (d *imageDocker) run(out io.Writer, name string, args ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(a, "image ls"):
		io.WriteString(out, "old456789012\t<none>:<none>\t2.01GB\n")
	case strings.HasPrefix(a, "image inspect --format") && strings.HasSuffix(a, " factory-job:latest"):
		io.WriteString(out, "sha256:def456789012345678\t1500000000\n")
	case strings.HasPrefix(a, "image inspect"):
		return io.EOF
	case strings.HasPrefix(a, "image rm"):
		d.removed = append(d.removed, args[2])
	}
	return nil
}

func uninstallSetup(t *testing.T, h *labelHost) (*config.Config, string, string, *imageDocker) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := &imageDocker{}
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
	return cfg, cfgPath, envPath, d
}

func newLabelHost() *labelHost {
	return &labelHost{labels: []string{"bug", "factory:ready", "factory:parked", "factory:mine"}, open: map[string]int{"factory:parked": 2}}
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
	if err == nil || !strings.Contains(out.String(), "holds more than the factory's data") {
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
	if err == nil || !strings.Contains(err.Error(), "the factory is running") {
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
	p := u.Plan(ctx)
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
	if _, err := u.Remove(ctx, true); err == nil || err.Error() != busy {
		t.Fatalf("err = %v", err)
	}
	busy = ""
	res, err := u.Remove(ctx, true)
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
