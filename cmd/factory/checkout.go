package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
)

// findCheckout looks for a checkout of rc on this machine, a few folders
// under the home folder, where people keep code, so setup can say where
// files git doesn't track could come from. "" when there's none.
func findCheckout(rc config.Repo) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	want := strings.ToLower(strings.TrimSuffix(rc.Path(), ".git"))
	found := ""
	// ponytail: a bounded walk of the home folder; a setting for where code lives if it misses.
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		depth := strings.Count(strings.TrimPrefix(path, home), string(filepath.Separator))
		name := d.Name()
		if depth > 4 || path != home && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			return nil
		}
		out, _ := exec.Command("git", "-C", path, "config", "--get-regexp", `^remote\..*\.url$`).Output()
		for _, l := range strings.Split(strings.ToLower(string(out)), "\n") {
			u := strings.TrimSuffix(strings.TrimSpace(l), ".git")
			if strings.HasSuffix(u, "/"+want) || strings.HasSuffix(u, ":"+want) {
				found = path
				return filepath.SkipAll
			}
		}
		return filepath.SkipDir // a checkout's own folders hold no other checkouts worth finding
	})
	return found
}
