package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

// The owner's checkout's .env reaches the builder and the gates, and never
// the branch, even when the builder's .gitignore un-ignores it.
func TestRepoFilesReachJobsButNotTheBranch(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	host := t.TempDir()
	writeFile(host, ".env", "SECRET=1\n")
	writeFile(host, "config/master.key", "k\n")
	e.p.Cfg.Repos[0].Files = &config.Files{From: host, Paths: []string{".env", "config"}}

	var mu sync.Mutex
	var gateSaw []string
	exec0 := e.p.GateExec
	e.p.GateExec = func(ctx context.Context, workdir, command string) (int, string, error) {
		if b, err := os.ReadFile(filepath.Join(workdir, ".env")); err == nil {
			mu.Lock()
			gateSaw = append(gateSaw, string(b))
			mu.Unlock()
		}
		return exec0(ctx, workdir, command)
	}
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		b, err := os.ReadFile(filepath.Join(work, ".env"))
		if err != nil || string(b) != "SECRET=1\n" {
			t.Errorf("builder's .env = %q, %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(work, "config", "master.key")); err != nil {
			t.Errorf("builder has no config/master.key: %v", err)
		}
		writeFile(work, ".env", "SECRET=changed\n")
		writeFile(work, ".gitignore", "!.env\n")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.PRNumber == 0 {
		t.Fatalf("no PR: %s", it.Stage)
	}
	out, err := exec.Command("git", "-C", e.remote, "ls-tree", "-r", "--name-only", "factory/issue-1").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(string(out))
	if slices.Contains(files, ".env") || slices.Contains(files, "config/master.key") || !slices.Contains(files, "add.go") {
		t.Fatalf("branch has %v", files)
	}
	if len(gateSaw) == 0 || gateSaw[0] != "SECRET=1\n" {
		t.Fatalf("gates saw .env %q", gateSaw)
	}
}

func TestPlaceFilesNeverOverwritesOrFollowsLinks(t *testing.T) {
	from, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(from, "kept", "host\n")
	writeFile(from, "link/secret", "host\n")
	writeFile(from, "new/deep/file", "host\n")
	writeFile(dst, "kept", "repo\n")
	if err := os.Symlink(outside, filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	placed, err := placeFiles(from, dst, []string{"kept", "link/secret", "new/deep/file"})
	if err != nil || !slices.Equal(placed, []string{"new/deep/file"}) {
		t.Fatalf("placed %v, %v", placed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "kept")); string(b) != "repo\n" {
		t.Fatalf("overwrote the repo's file: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err == nil {
		t.Fatal("wrote through the link")
	}
}

func TestWorktreeIncludePicksIgnoredMatches(t *testing.T) {
	ctx := context.Background()
	from := t.TempDir()
	git(t, from, "init", "-q")
	writeFile(from, ".gitignore", ".env\n*.local\n")
	writeFile(from, ".worktreeinclude", ".env\n*.local\nnotes.txt\n")
	writeFile(from, ".env", "x\n")
	writeFile(from, "db/settings.local", "x\n")
	writeFile(from, "notes.txt", "matches but isn't ignored\n")
	writeFile(from, "other.secret", "ignored nowhere\n")
	got, err := worktreeInclude(ctx, from)
	if err != nil || !slices.Equal(got, []string{".env", "db/settings.local"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestEscapePattern(t *testing.T) {
	for in, want := range map[string]string{
		".env":       ".env",
		"a[1]*.txt":  `a\[1\]\*.txt`,
		"#notes":     `\#notes`,
		"!important": `\!important`,
		"trailing ":  `trailing\ `,
	} {
		if got := escapePattern(in); got != want {
			t.Errorf("escapePattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilesConfig(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ yaml, err string }{
		{"files: {from: " + dir + ", paths: [.env]}", ""},
		{"files: {paths: [.env]}", "from is required"},
		{"files: {from: " + dir + "/nope, paths: [.env]}", "is not a folder"},
		{"files: {from: " + dir + ", paths: [../x]}", "inside the repo"},
		{"files: {from: " + dir + ", paths: [.git/config]}", "inside .git"},
		{"files: {from: " + dir + "}", ".worktreeinclude"},
	} {
		_, err := config.Parse([]byte(testConfig + "repos:\n  - name: o/r\n    " + tc.yaml + "\n"))
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.yaml, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: err %v, want %q", tc.yaml, err, tc.err)
		}
	}
}
