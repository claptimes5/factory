package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
)

// trustSetup serves the API over a factory.yaml for o/r at level 3
// (low risk up to size S), with merged PRs of the given sizes, all low
// risk, made the given number of days ago.
func trustSetup(t *testing.T, sizes []string) (string, *Server, *store.Store, string) {
	t.Helper()
	_, st, _, s := apiSetup(t)
	src, err := os.ReadFile("../../factory.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	src = regexp.MustCompile(`(?m)^# repo: .*$`).ReplaceAll(src, []byte("repo: o/r\nauto_merge: {up_to: low, max_size: S}"))
	path := filepath.Join(t.TempDir(), "factory.yaml")
	os.WriteFile(path, src, 0o640)
	s.ConfigPath, s.Listen, s.StartedConfig = path, "127.0.0.1:0", ConfigVersion(src)
	s.SetTrust = func(_ context.Context, repo string, a *config.AutoMerge) error { return config.SetRepoAutoMerge(path, repo, a) }
	ctx := context.Background()
	for i, size := range sizes {
		risk := "low"
		it, _, _ := st.UpsertItem(ctx, "o/r", 100+i, fmt.Sprintf("Change %d", i))
		st.UpdateItem(ctx, it.ID, store.ItemUpdate{Risk: &risk, Complexity: &size})
		at := time.Now().Add(-time.Duration(i) * time.Hour).UTC().Format(store.TimeFormat)
		st.RecordMerge(ctx, store.FactoryMerge{Repo: "o/r", PR: 100 + i, ItemID: it.ID, MergeSHA: fmt.Sprintf("m%d", i), MergedAt: at})
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httpServer(t, h), s, st, path
}

func TestTrustLevelsAndPracticeRuns(t *testing.T) {
	t.Parallel()
	// 20 low-risk changes up to M: enough for level 4, with none gone wrong.
	sizes := strings.Split(strings.Repeat("XS,S,M,M,", 5), ",")[:20]
	srv, _, st, path := trustSetup(t, sizes)
	var tr APITrust
	getJSON(t, srv+"/api/v1/trust", &tr)
	r := tr.Repos[0]
	if r.Level != 3 || r.Custom || r.Merged != 20 || len(tr.AlwaysAsks) == 0 || !tr.CanChange {
		t.Fatalf("trust = %+v (always asks %v, can change %v)", r, tr.AlwaysAsks, tr.CanChange)
	}
	practice := map[int]APIPractice{}
	for _, p := range r.Practice {
		practice[p.Level] = p
	}
	if practice[2].WouldMerge != 5 || practice[3].WouldMerge != 10 || practice[4].WouldMerge != 20 || practice[5].WouldMerge != 20 {
		t.Fatalf("practice runs = %+v", r.Practice)
	}
	if r.Suggest != 4 || !strings.Contains(r.SuggestWhy, "Level 4 would have merged 20 changes") {
		t.Fatalf("suggest = %d %q", r.Suggest, r.SuggestWhy)
	}
	// One reverted change it would have merged: not yet.
	st.MarkReverted(context.Background(), "o/r", 103, "commit abc", store.Now())
	getJSON(t, srv+"/api/v1/trust", &tr)
	if r := tr.Repos[0]; r.Suggest != 0 || r.Reverted != 1 || !strings.Contains(r.SuggestWhy, "went wrong") {
		t.Fatalf("after a revert: %+v", r)
	}

	// Raising it writes the repo's own auto_merge, applied on restart.
	code, body := postJSON(t, srv+"/api/v1/trust", "", `{"repo":"o/r","level":4}`)
	if code != 200 || !strings.Contains(body, "level 4") {
		t.Fatalf("set: %d %s", code, body)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if rc, _ := cfg.RepoConf("o/r"); rc.AutoMerge.UpTo != "low" || rc.AutoMerge.MaxSize != "M" {
		t.Fatalf("o/r's auto_merge = %+v", rc.AutoMerge)
	}
	var after struct{ Trust APITrust }
	json.Unmarshal([]byte(body), &after)
	if after.Trust.Repos[0].Level != 4 || !after.Trust.Pending {
		t.Fatalf("after: %+v pending %v", after.Trust.Repos[0], after.Trust.Pending)
	}
	if code, _ := postJSON(t, srv+"/api/v1/trust", "", `{"repo":"o/r","level":6}`); code != 400 {
		t.Fatalf("a level not built yet was taken: %d", code)
	}
	// Level 1 turns auto-merge off for the repo.
	postJSON(t, srv+"/api/v1/trust", "", `{"repo":"o/r","level":1}`)
	cfg, _ = config.Load(path)
	if rc, _ := cfg.RepoConf("o/r"); rc.AutoMerge.On() {
		t.Fatalf("level 1 left auto-merge on: %+v", rc.AutoMerge)
	}
}

// A tripped breaker steps a repo back: no suggestion until it's reset.
func TestTrustStepsBack(t *testing.T) {
	t.Parallel()
	srv, s, _, _ := trustSetup(t, []string{"XS"})
	s.Breakers = func(context.Context) []APIBreaker {
		return []APIBreaker{{Repo: "o/r", On: true, Tripped: true, Reason: "2 reverts"}}
	}
	var tr APITrust
	getJSON(t, srv+"/api/v1/trust", &tr)
	if r := tr.Repos[0]; !r.SteppedBack || r.Why != "2 reverts" || r.Suggest != 0 || !strings.Contains(r.SuggestWhy, "Reset it first") {
		t.Fatalf("stepped back = %+v", r)
	}
}
