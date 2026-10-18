package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/eval"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
)

// prForge is the slice of a forge capture uses.
type prForge struct {
	forge
	pr     github.PR
	issues map[int]github.Issue
}

func (f *prForge) GetPR(_ context.Context, n int) (*github.PR, error) {
	pr := f.pr
	return &pr, nil
}

func (f *prForge) GetIssue(_ context.Context, n int) (*github.Issue, error) {
	is := f.issues[n]
	return &is, nil
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A merged PR becomes a case frozen where its branch left main, with the
// fix's tests as the hidden acceptance files and the issue it closed as
// the task, however it was merged.
func TestCaptureMergedPR(t *testing.T) {
	for _, how := range []string{"merge", "squash"} {
		t.Run(how, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			seed := filepath.Join(root, "seed")
			os.MkdirAll(seed, 0o755)
			sh(t, seed, "init", "-q", "-b", "main")
			os.WriteFile(filepath.Join(seed, "add.go"), []byte("package x\n"), 0o644)
			os.WriteFile(filepath.Join(seed, "old_test.go"), []byte("package x\n"), 0o644)
			sh(t, seed, "add", "-A")
			sh(t, seed, "commit", "-qm", "seed")
			base := sh(t, seed, "rev-parse", "HEAD")

			sh(t, seed, "checkout", "-qb", "fix")
			os.WriteFile(filepath.Join(seed, "add.go"), []byte("package x\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
			os.WriteFile(filepath.Join(seed, "add_test.go"), []byte("package x\n\n// TestAdd\n"), 0o644)
			os.Remove(filepath.Join(seed, "old_test.go"))
			sh(t, seed, "add", "-A")
			sh(t, seed, "commit", "-qm", "fix")
			head := sh(t, seed, "rev-parse", "HEAD")

			sh(t, seed, "checkout", "-q", "main")
			os.WriteFile(filepath.Join(seed, "later.go"), []byte("package x\n"), 0o644)
			sh(t, seed, "add", "-A")
			sh(t, seed, "commit", "-qm", "main moved on")
			if how == "merge" {
				sh(t, seed, "merge", "-q", "--no-ff", "-m", "merge", "fix")
			} else {
				sh(t, seed, "merge", "-q", "--squash", "fix")
				sh(t, seed, "commit", "-qm", "squashed")
			}
			merge := sh(t, seed, "rev-parse", "HEAD")
			sh(t, seed, "update-ref", "refs/pull/7/head", head)
			sh(t, seed, "branch", "-qD", "fix") // deleted after merge, as usual
			remote := filepath.Join(root, "remote.git")
			sh(t, root, "clone", "-q", "--bare", seed, remote)
			sh(t, remote, "update-ref", "refs/pull/7/head", head)

			cfg, err := config.Load(testConfig(t, "o/r"))
			if err != nil {
				t.Fatal(err)
			}
			f := &prForge{issues: map[int]github.Issue{3: {Number: 3, Title: "Add an Add function!", Body: "We need Add."}}}
			f.pr = github.PR{Number: 7, Merged: true, Title: "factory: #3", Body: "Closes #3\n\nBuilt by the factory.", MergeCommit: merge}
			f.pr.Head.SHA = head
			set := filepath.Join(root, "evals")
			c, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(set, "pr-7-add-an-add-function"); c.Dir != want {
				t.Fatalf("dir = %s, want %s", c.Dir, want)
			}
			if len(c.Tests) != 1 || c.Tests[0] != "add_test.go" {
				t.Fatalf("tests = %v", c.Tests)
			}
			if b, _ := os.ReadFile(filepath.Join(c.Dir, "accept", "add_test.go")); string(b) != "package x\n\n// TestAdd\n" {
				t.Fatalf("accept file = %q", b)
			}
			cases, err := eval.Load(set)
			if err != nil || len(cases) != 1 {
				t.Fatalf("load: %v %v", cases, err)
			}
			k := cases[0]
			if k.BaseSHA != base || k.Title != "Add an Add function!" || k.Body != "We need Add." || k.Accept != cfg.Gates.Commands["test"] || k.Source != "PR #7" || k.Repo != "" {
				t.Fatalf("case = %+v (base %s)", k, base)
			}

			// It won't overwrite a case, or capture what isn't merged.
			if _, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", ""); err == nil {
				t.Fatal("captured over an existing case")
			}
			f.pr.Merged = false
			if _, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "other", ""); err == nil || !strings.Contains(err.Error(), "isn't merged") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
