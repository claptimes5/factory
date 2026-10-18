// Package gittest keeps the host's git config out of a package's tests.
// Tests commit in throwaway repos, often many at once, and a host's global
// commit.gpgsign would send every one of those commits to its signing
// program, which then fails them under load. A global config of the tests'
// own gives the identity the host's would have; a repo's own config still
// overrides it, as the daemon's commits need.
package gittest

import (
	"os"
	"path/filepath"
	"testing"
)

// Main runs a package's tests with a git config of their own. Call it
// from TestMain.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gittest-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		panic(err)
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": cfg, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	return m.Run()
}
