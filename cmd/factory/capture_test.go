package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/eval"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
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
	for _, how := range []string{"merge", "squash", "rebase", "ff"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
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
			// A second commit, so a base taken from the wrong parent lands
			// in the middle of the fix.
			os.MkdirAll(filepath.Join(seed, "testdata"), 0o755)
			os.WriteFile(filepath.Join(seed, "testdata", "cases.json"), []byte("[]\n"), 0o644)
			os.Symlink("add_test.go", filepath.Join(seed, "link_test.go"))
			sh(t, seed, "add", "-A")
			sh(t, seed, "commit", "-qm", "fix data")
			head := sh(t, seed, "rev-parse", "HEAD")

			sh(t, seed, "checkout", "-q", "main")
			if how != "ff" {
				os.WriteFile(filepath.Join(seed, "later.go"), []byte("package x\n"), 0o644)
				sh(t, seed, "add", "-A")
				sh(t, seed, "commit", "-qm", "main moved on")
			}
			switch how {
			case "merge":
				sh(t, seed, "merge", "-q", "--no-ff", "-m", "merge", "fix")
			case "squash":
				sh(t, seed, "merge", "-q", "--squash", "fix")
				sh(t, seed, "commit", "-qm", "squashed")
			case "rebase":
				sh(t, seed, "cherry-pick", base+".."+head)
			case "ff":
				sh(t, seed, "merge", "-q", "--ff-only", "fix")
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
			f.pr.Head.SHA, f.pr.Base.SHA = head, base
			set := filepath.Join(root, "evals")
			c, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(set, "pr-7-add-an-add-function"); c.Dir != want {
				t.Fatalf("dir = %s, want %s", c.Dir, want)
			}
			if strings.Join(c.Tests, ",") != "add_test.go,testdata/cases.json" {
				t.Fatalf("tests = %v", c.Tests)
			}
			notes := strings.Join(c.Notes, "\n")
			if !strings.Contains(notes, "link_test.go: not copied, it's a symlink") || !strings.Contains(notes, "old_test.go: the fix deleted it") {
				t.Fatalf("notes = %s", notes)
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

			if ents, _ := os.ReadDir(set); len(ents) != 1 {
				t.Fatalf("set holds %v", ents) // no temp directory left behind
			}

			// It won't overwrite a case, or capture what isn't merged.
			if _, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", ""); err == nil {
				t.Fatal("captured over an existing case")
			}
			f.pr.Merged = false
			if _, err := capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "other", ""); err == nil || !strings.Contains(err.Error(), "isn't merged") {
				t.Fatalf("err = %v", err)
			}
			// With no issue to read, the PR's own text is the task, and it says so.
			f.pr.Merged, f.pr.Body = true, "Adds Add."
			c, err = capture(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", "go test ./...")
			if err != nil {
				t.Fatal(err)
			}
			cases, _ = eval.Load(set)
			if len(cases) != 2 || cases[1].Title != "factory: #3" || cases[1].Accept != "go test ./..." || !strings.Contains(strings.Join(c.Notes, "\n"), "names no issue") {
				t.Fatalf("cases = %+v, notes = %v", cases, c.Notes)
			}

			// Reviewer cases from the same PR: a clean one, beside the
			// builder's, and no plant, since Add's one line fits none.
			rc, err := captureReview(ctx, cfg, "o/r", 7, f, &pipeline.Git{RemoteURL: remote}, t.TempDir(), set, "", false)
			if err != nil || len(rc.Dirs) != 1 || !strings.HasSuffix(rc.Dirs[0], "-clean") || !strings.Contains(strings.Join(rc.Notes, "\n"), "flip-comparison: nothing") {
				t.Fatalf("review capture %+v %v", rc, err)
			}
			rcs, err := eval.LoadReview(set)
			if err != nil || len(rcs) != 1 || rcs[0].Kind != eval.KindClean || rcs[0].BaseSHA != base {
				t.Fatalf("reviewer cases %+v %v", rcs, err)
			}
			if cases, _ = eval.Load(set); len(cases) != 2 {
				t.Fatalf("builder cases = %+v", cases)
			}
		})
	}
}

// A case runs against a configured repo's remote and gates, so a PR from
// any other repo is refused.
func TestCaptureUnconfiguredRepo(t *testing.T) {
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = capture(context.Background(), cfg, "other/repo", 1, &prForge{}, &pipeline.Git{}, t.TempDir(), t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "isn't in the config") {
		t.Fatalf("err = %v", err)
	}
}

// A factory PR the revert watch saw reverted is captured as a revert
// case, named by its number so a second run skips it.
func TestRevertedPRs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{4, 5} {
		if err := st.RecordMerge(ctx, store.FactoryMerge{Repo: "o/r", PR: n, MergeSHA: fmt.Sprintf("sha%d", n), Title: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.MarkReverted(ctx, "o/r", 5, "abc123", store.Now()); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]bool{4: false, 5: true, 6: false} {
		if got, err := reverted(ctx, st, "o/r", n); err != nil || got != want {
			t.Errorf("reverted(%d) = %v, %v", n, got, err)
		}
	}
	if n := revertCaseName(cfg, store.FactoryMerge{Repo: "o/r", PR: 5}); n != "pr-5" {
		t.Fatalf("name = %s", n)
	}
	// One already captured is skipped without reaching the forge.
	set := t.TempDir()
	os.MkdirAll(filepath.Join(set, "pr-5-revert"), 0o755)
	if err := captureReverts(ctx, cfg, st, set); err != nil {
		t.Fatal(err)
	}
}
