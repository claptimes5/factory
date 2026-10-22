package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

// A repo set up on a public image with no services gets its Dockerfile's
// test stage, its CI's args, the master key from its checkout, and its
// compose database with the settings that find it.
func TestFixAddsWhatTheSandboxLacks(t *testing.T) {
	checkout := t.TempDir()
	os.MkdirAll(filepath.Join(checkout, "config"), 0o755)
	os.WriteFile(filepath.Join(checkout, "config", "master.key"), []byte("k"), 0o600)
	cfg := &config.Config{}
	r := config.Repo{Name: "o/api", ImageFrom: "ruby:3.4-bookworm", Files: &config.Files{From: checkout}, Env: map[string]string{"TZ": "UTC"}}
	s := &Suggestion{
		ImageBuild: &config.ImageBuild{Target: "test", Args: map[string]string{"RAILS_ENV": "test"}},
		Secrets:    []string{"railsmasterkey", "npmrc"},
		Services:   map[string]config.Service{"db": {Image: "postgres"}},
		Env:        map[string]string{"PGHOST": "db", "TZ": "Europe/Paris"},
	}
	out, changes := Fix(cfg, r, s)
	if out.ImageFrom != "" || out.ImageBuild.Target != "test" || out.ImageBuild.Secrets["railsmasterkey"] != "config/master.key" || out.ImageBuild.Secrets["npmrc"] != "" {
		t.Fatalf("image: from %q build %+v", out.ImageFrom, out.ImageBuild)
	}
	if out.Services["db"].Image != "postgres" || out.Env["PGHOST"] != "db" || out.Env["TZ"] != "UTC" || r.Env["PGHOST"] != "" {
		t.Fatalf("services %v env %v (was %v)", out.Services, out.Env, r.Env)
	}
	want := []string{
		"Build the job image from the repo's Dockerfile, stage test, instead of on ruby:3.4-bookworm, so the checks have what the app has installed",
		"Build it with RAILS_ENV=test, as the repo's CI does",
		"Give its build the secret railsmasterkey from config/master.key in your checkout",
		"Start db (postgres) for every build and check",
		"Set PGHOST=db, so the app finds it",
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changes:\n%s", strings.Join(changes, "\n"))
	}

	// Services of its own stay as they are, and what it has needs nothing.
	r = out
	r.Services = map[string]config.Service{"postgres": {Image: "postgres:16"}}
	r.Env = map[string]string{"DATABASE_URL": "postgres://postgres@postgres/test"}
	if again, changes := Fix(cfg, r, s); len(changes) != 0 || len(again.Services) != 1 || again.Env["PGHOST"] != "" {
		t.Fatalf("changed %v: %+v", changes, again)
	}
}
