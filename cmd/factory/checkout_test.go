package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

// A checkout whose remote is the repo is found a few folders down, past
// other checkouts and hidden folders.
func TestFindCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(dir, remote string) {
		p := filepath.Join(home, dir)
		os.MkdirAll(p, 0o755)
		if out, err := exec.Command("git", "-C", p, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		exec.Command("git", "-C", p, "remote", "add", "origin", remote).Run()
	}
	mk("code/other", "git@gitlab.com:team/other.git")
	mk(".cache/api", "https://gitlab.com/team/api_backend.git")
	mk("development/team/api_backend", "git@gitlab.com:team/api_backend.git")
	rc := config.Repo{Name: "gitlab.com/team/api_backend", Host: "gitlab.com"}
	if got := findCheckout(rc); got != filepath.Join(home, "development/team/api_backend") {
		t.Fatalf("found %q", got)
	}
	if got := findCheckout(config.Repo{Name: "o/missing"}); got != "" {
		t.Fatalf("found %q for a repo with no checkout", got)
	}
}
