package web

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/stack"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// An item parked because its checks had no database shows the change the
// repo's files suggest; applying saves it, restarts and retries.
func TestFixSetup(t *testing.T) {
	base, path, s, fr := settingsSetup(t, "127.0.0.1:8080")
	b, _ := os.ReadFile(path)
	b = regexp.MustCompile(`(?m)^(# )?repo: .*$`).ReplaceAll(b, []byte("repos:\n  - {name: o/r, image_from: ruby:3.4-bookworm}"))
	os.WriteFile(path, b, 0o640)
	ctx := context.Background()
	it, _, _ := s.Store.UpsertItem(ctx, "o/r", 41, "Add a report")
	id, _ := s.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "haiku"})
	s.Store.ClaimJob(ctx)
	s.Store.FinishJob(ctx, id, store.Result{State: state.Failed, Error: "GATES: FAIL test=fail"})
	s.Store.SetStage(ctx, it.ID, state.Parked, "daemon", "The checks can't run in the factory's sandbox")
	s.Choices = func(context.Context, int64) []APIChoice { return []APIChoice{{Action: "fix_setup", Available: true}} }
	s.SetupPark = func(_ context.Context, w *store.WorkItem) (pipeline.Park, bool) {
		return pipeline.Park{CantFix: true, Setup: "service", Line: "port 5432 failed: Connection refused (PG::ConnectionBad)"}, w.ID == it.ID
	}
	s.Detect = func(context.Context, string) (*stack.Suggestion, error) {
		return &stack.Suggestion{Services: map[string]config.Service{"db": {Image: "postgres"}}, Env: map[string]string{"PGHOST": "db"},
			Notes: []string{"Its services come from docker-compose.yml."}}, nil
	}
	u := base + "/api/v1/items/" + strconv.FormatInt(it.ID, 10) + "/setup-fix"

	var fix APISetupFix
	getJSON(t, u, &fix)
	if !strings.Contains(fix.Problem, "isn't running") || fix.Line == "" || strings.Join(fix.Changes, "|") != "Start db (postgres) for every build and check|Set PGHOST=db, so the app finds it" ||
		!strings.Contains(fix.After, "PGHOST: db") || strings.Contains(fix.Before, "PGHOST") || !fix.Writable || len(fix.Notes) != 1 {
		t.Fatalf("fix = %+v", fix)
	}
	if code, body := postJSON(t, u, "", `{"version":"old"}`); code != 409 || !strings.Contains(body, "changed since") {
		t.Fatalf("stale version: %d %s", code, body)
	}
	code, body := postJSON(t, u, "", `{"version":"`+fix.Version+`"}`)
	var res APIActionResult
	json.Unmarshal([]byte(body), &res)
	if code != 200 || !res.OK || !strings.Contains(res.Message, "Its retry is queued") {
		t.Fatalf("apply: %d %s", code, body)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.RepoConf("o/r")
	if r.Services["db"].Image != "postgres" || r.Env["PGHOST"] != "db" || r.ImageFrom != "ruby:3.4-bookworm" {
		t.Fatalf("saved %+v", r)
	}
	if !fr.pending || strings.Join(s.Actions.(*fakeActions).calls, ",") != "retry" {
		t.Fatalf("restart %v, calls %v", fr.pending, s.Actions.(*fakeActions).calls)
	}
	// Applied, there's nothing left to change.
	getJSON(t, u, &fix)
	if len(fix.Changes) != 0 {
		t.Fatalf("after: %v", fix.Changes)
	}
}
