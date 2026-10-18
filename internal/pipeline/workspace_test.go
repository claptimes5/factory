package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// wsEnv is two repos in one workspace: o/r (the API, merged first) and
// o/web (the app that calls it).
type wsEnv struct {
	*env
	web *env
}

func setupWorkspace(t *testing.T) *wsEnv {
	t.Helper()
	e := setup(t)
	cfg, err := config.Parse([]byte(strings.Replace(testConfig, "repo: o/r\n", `repos:
  - o/r
  - name: o/web
    gates: {required: [built], commands: {built: test -f client.txt}}
workspaces:
  app: [o/r, o/web]
`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir, cfg.PromptsDir, cfg.SkillsDir = e.p.Cfg.DataDir, e.p.Cfg.PromptsDir, e.p.Cfg.SkillsDir
	cfg.Budgets.MinFreeDiskGB = e.p.Cfg.Budgets.MinFreeDiskGB
	e.p.Cfg = cfg
	web := setup(t)
	clients := map[string]RepoClients{
		"o/r":   {GH: e.gh, Git: e.p.Git},
		"o/web": {GH: web.gh, Git: web.p.Git},
	}
	e.p.ClientsFor = func(repo string) RepoClients { return clients[strings.ToLower(repo)] }
	return &wsEnv{e, web}
}

// bothBuild changes the API and the app that calls it.
func bothBuild(work string) error {
	if err := goodBuild(filepath.Join(work, "r")); err != nil {
		return err
	}
	return webBuild("calls Add\n")(work)
}

func webBuild(body string) func(string) error {
	return func(work string) error {
		dir := filepath.Join(work, "web")
		if err := writeFile(dir, "client.txt", body); err != nil {
			return err
		}
		return commitIn(dir, "client")
	}
}

func gateNames(t *testing.T, e *env, jobID int64) string {
	t.Helper()
	gs, _ := e.p.Store.GateResults(context.Background(), jobID)
	var names []string
	for _, g := range gs {
		names = append(names, g.Gate)
	}
	return strings.Join(names, ",")
}

// An issue in a workspace is one change across its repos: both checked out
// side by side, gates in each, one review of the combined diff, a linked
// draft PR per repo, and done only when every PR merged.
func TestWorkspaceChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	// Round one changes both repos and is blocked; the fix round changes
	// only the app, so the API keeps round one's commit.
	w.claude.builds = []func(string) error{bothBuild, webBuild("calls Add, checked\n")}
	w.codex.reviews = []string{block, resolveF1}

	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("item = %+v", it)
	}
	branch := "factory/r-1"
	for _, remote := range []string{w.remote, w.web.remote} {
		if got := git(t, remote, "log", "--format=%s", "-1", branch); !strings.HasPrefix(got, "factory: o/r#1 ") {
			t.Fatalf("%s: branch head = %q", remote, got)
		}
	}
	if got := git(t, w.web.remote, "show", branch+":client.txt"); got != "calls Add, checked" {
		t.Fatalf("app = %q", got)
	}
	if git(t, w.remote, "show", branch+":add.go") == "" {
		t.Fatal("the API lost round one's change")
	}

	// The builder saw both repos and each one's checks.
	prompt := w.claude.prompts["implement"][0]
	for _, want := range []string{"- `r/`: o/r", "- `web/`: o/web", "In `web/`: run `test -f client.txt`;"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("implement prompt missing %q:\n%s", want, prompt)
		}
	}
	// The fix round's gates still cover both repos.
	jobs, _ := w.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if len(jobs) != 2 {
		t.Fatalf("%d implement jobs", len(jobs))
	}
	want := "r: diff_guard,r: proves_fix,r: test,r: types,web: built,web: diff_guard,web: proves_fix"
	if got := gateNames(t, w.env, jobs[0].ID); got != want {
		t.Fatalf("gates = %s\nwant    %s", got, want)
	}
	heads, _ := w.p.Store.JobHeads(ctx, jobs[0].ID)
	if len(heads) != 2 || heads[0].Repo != "o/r" || heads[1].Repo != "o/web" {
		t.Fatalf("heads = %+v", heads)
	}
	// The reviewer read one diff, every path under its repo's folder.
	review := w.codex.prompts["review"][1]
	for _, want := range []string{"+++ b/r/add.go", "+++ b/web/client.txt", "`web/` is o/web"} {
		if !strings.Contains(review, want) {
			t.Errorf("review prompt missing %q", want)
		}
	}

	// A PR in each repo, linked, with only the last to merge closing the issue.
	api, app := w.gh.prs[branch], w.web.gh.prs[branch]
	if api == nil || app == nil {
		t.Fatalf("PRs: api %v app %v", api, app)
	}
	order := "1. [o/r#101](https://example/pr) (this PR)\n2. [o/web#101](https://example/pr)\n"
	if b := w.gh.bodies[api.Number]; !strings.HasPrefix(b, "Part of o/r#1\n") || !strings.Contains(b, order) {
		t.Errorf("API PR body:\n%s", b)
	}
	if b := w.web.gh.bodies[app.Number]; !strings.HasPrefix(b, "Closes o/r#1\n") || !strings.Contains(b, "2. [o/web#101](https://example/pr) (this PR)") {
		t.Errorf("app PR body:\n%s", b)
	}
	if !hasLabel(w.web.gh.prLabels[app.Number], LabelAwaitingYou) || !hasLabel(w.web.gh.prLabels[app.Number], "factory:risk-low") {
		t.Errorf("app PR labels = %v", w.web.gh.prLabels[app.Number])
	}
	prs, _ := w.p.Store.ItemPRs(ctx, it.ID)
	if len(prs) != 2 || prs[0] != (store.RepoPR{Repo: "o/r", Number: 101}) || prs[1] != (store.RepoPR{Repo: "o/web", Number: 101}) {
		t.Fatalf("item PRs = %+v", prs)
	}

	// The API merges first: still waiting on the app.
	api.Merged, api.State, api.Head.SHA = true, "closed", git(t, w.remote, "rev-parse", branch)
	if err := w.p.closeOut(ctx); err != nil {
		t.Fatal(err)
	}
	if it := w.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("done with one PR open: %s", it.Stage)
	}
	app.Merged, app.State, app.Head.SHA = true, "closed", git(t, w.web.remote, "rev-parse", branch)
	if err := w.p.closeOut(ctx); err != nil {
		t.Fatal(err)
	}
	if it := w.item(t, 1); it.Stage != state.Done {
		t.Fatalf("stage = %s", it.Stage)
	}
}

// A change that needs only one repo of the workspace opens only its PR, and
// a PR closed unmerged parks the item.
func TestWorkspaceOneRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	w.claude.builds = []func(string) error{webBuild("hi\n")}
	w.codex.reviews = []string{pass}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("item = %+v", it)
	}
	if w.gh.prs["factory/r-1"] != nil {
		t.Fatal("a PR for the repo the change left alone")
	}
	app := w.web.gh.prs["factory/r-1"]
	if b := w.web.gh.bodies[app.Number]; !strings.HasPrefix(b, "Closes o/r#1\n\nBuilt by the factory") {
		t.Fatalf("app PR body:\n%s", b)
	}
	jobs, _ := w.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if got := gateNames(t, w.env, jobs[0].ID); got != "web: built,web: diff_guard,web: proves_fix" {
		t.Fatalf("gates = %s", got)
	}
	app.State = "closed"
	if err := w.p.closeOut(ctx); err != nil {
		t.Fatal(err)
	}
	if it := w.item(t, 1); it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
}

// A restart between the build and its review resumes with every repo's
// head, so the review neither parks on a "moved" branch nor opens one PR.
func TestWorkspaceResumeAfterImplement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	w.claude.builds = []func(string) error{bothBuild}
	w.codex.reviews = []string{pass, pass}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, 1)
	impl, _ := w.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	// As if the daemon died right after the implement job finished.
	if err := w.p.Store.SetStage(ctx, it.ID, state.Implementing, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.p.resume(ctx, w.item(t, 1), impl[:1]); err != nil {
		t.Fatal(err)
	}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := w.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	if prs, _ := w.p.Store.ItemPRs(ctx, it.ID); len(prs) != 2 {
		t.Fatalf("PRs = %+v", prs)
	}
}

// The builder can write the folder that holds a workspace's checkouts; a
// checkout it swaps for a symlink is refused, never committed from.
func TestWorkspaceRefusesSymlinkedCheckout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	secret := t.TempDir()
	writeFile(secret, "client.txt", "host file\n")
	w.claude.builds = []func(string) error{func(work string) error {
		if err := os.RemoveAll(filepath.Join(work, "web")); err != nil {
			return err
		}
		return os.Symlink(secret, filepath.Join(work, "web"))
	}}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, 1)
	jobs, _ := w.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	first := jobs[len(jobs)-1]
	if first.State != state.Failed || !strings.Contains(first.Error, "replaced web") {
		t.Fatalf("job = %s %q", first.State, first.Error)
	}
	if out, err := exec.Command("git", "-C", w.web.remote, "rev-parse", "--verify", "--quiet", "factory/r-1").Output(); err == nil {
		t.Fatalf("pushed %s", out)
	}
}

// A rebuild that no longer touches a repo marks that repo's old PR as
// superseded, and stops tracking it.
func TestWorkspaceRetiresDroppedPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	w.claude.builds = []func(string) error{bothBuild}
	w.codex.reviews = []string{pass}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, 1)
	api := w.gh.prs["factory/r-1"]
	// The rebuild touches only the app.
	in := ReviewInput{Head: "x", Heads: []store.RepoHead{{Repo: "o/web", SHA: "x"}}, Gates: "GATES: PASS"}
	if err := w.p.Store.SetStage(ctx, it.ID, state.Implementing, "test", ""); err != nil {
		t.Fatal(err)
	}
	w.p.Store.SetStage(ctx, it.ID, state.Reviewing, "test", "")
	if err := w.p.openPR(ctx, w.item(t, 1), *w.gh.issues[1], in); err != nil {
		t.Fatal(err)
	}
	if b := w.gh.bodies[api.Number]; !strings.HasPrefix(b, "**Superseded: don't merge this.**") {
		t.Fatalf("old API PR body:\n%s", b)
	}
	if hasLabel(w.gh.prLabels[api.Number], LabelAwaitingYou) {
		t.Fatal("the superseded PR still waits on you")
	}
	if prs, _ := w.p.Store.ItemPRs(ctx, it.ID); len(prs) != 1 || prs[0].Repo != "o/web" {
		t.Fatalf("PRs = %+v", prs)
	}
}
