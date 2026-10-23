package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
)

// passF1 passes a fix round, ruling the first round's finding resolved.
const passF1 = `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"handled"}],"challenged_assumptions":[]}`

// A fix round builds on its branch with what merged into main since,
// so a fix that landed there in between reaches it.
func TestFixRoundBuildsOnTodaysBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(7)
	first := func(w string) error {
		if err := goodBuild(w); err != nil {
			return err
		}
		pushTo(t, e.remote, map[string]string{"fixed.go": "package fixture\n\n// Fixed on main meanwhile.\nvar Fixed = true\n"})
		return nil
	}
	fix := func(w string) error {
		if _, err := os.Stat(filepath.Join(w, "fixed.go")); err != nil {
			return err // still on the old base
		}
		writeFile(w, "add.go", "package fixture\n\n// Add adds, overflow and all.\nfunc Add(a, b int) int { return a + b }\n")
		return commitIn(w, "fix")
	}
	e.claude.builds = []func(string) error{first, fix}
	e.claude.implVerdict, e.claude.verifies = answerF1, []string{confirmF1}
	e.codex.reviews = []string{block, passF1}
	e.p.Drain(ctx)
	if st := e.item(t, 7).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	if !strings.Contains(e.claude.prompts["implement"][1], "The factory merged main (now at") {
		t.Fatalf("the builder wasn't told:\n%s", e.claude.prompts["implement"][1])
	}
	tip := git(t, e.remote, "rev-parse", "main")
	git(t, e.remote, "merge-base", "--is-ancestor", tip, "factory/issue-7")
	if got := git(t, e.remote, "log", "--format=%s", "factory/issue-7"); !strings.Contains(got, "Merge main into factory/issue-7") {
		t.Fatalf("branch history = %q", got)
	}
}

// A merge that conflicts goes to the builder to resolve; its resolution
// is the merge commit. One that leaves a marker fails the build.
func TestFixRoundResolvesAConflictWithMain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(8)
	first := func(w string) error {
		writeFile(w, "doc.go", "// Package fixture is ours.\npackage fixture\n")
		if err := goodBuild(w); err != nil {
			return err
		}
		pushTo(t, e.remote, map[string]string{"doc.go": "// Package fixture is theirs.\npackage fixture\n"})
		return nil
	}
	leave := func(w string) error { // touches another file, leaves the markers
		writeFile(w, "add.go", "package fixture\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n")
		return commitIn(w, "fix")
	}
	resolve := func(w string) error {
		b, _ := os.ReadFile(filepath.Join(w, "doc.go"))
		if !strings.Contains(string(b), "<<<<<<< ") {
			t.Errorf("no conflict to resolve in doc.go:\n%s", b)
		}
		writeFile(w, "doc.go", "// Package fixture is both of ours.\npackage fixture\n")
		writeFile(w, "add.go", "package fixture\n\n// Add adds, overflow and all.\nfunc Add(a, b int) int { return a + b }\n")
		return commitIn(w, "fix")
	}
	e.claude.builds = []func(string) error{first, leave, resolve}
	e.claude.implVerdict, e.claude.verifies = answerF1, []string{confirmF1}
	e.codex.reviews = []string{block, passF1}
	e.p.Drain(ctx)
	if st := e.item(t, 8).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	jobs := implJobs(t, e, e.item(t, 8).ID)
	if len(jobs) != 3 || !strings.Contains(jobs[1].Error, "builder left conflict markers in doc.go from merging main") {
		t.Fatalf("jobs: %d, the marked one: %q", len(jobs), jobs[1].Error)
	}
	if p := e.claude.prompts["implement"][2]; !strings.Contains(p, "The merge conflicted") || !strings.Contains(p, "- doc.go") {
		t.Fatalf("prompt:\n%s", p)
	}
	tip := git(t, e.remote, "rev-parse", "main")
	if parents := strings.Fields(git(t, e.remote, "log", "-1", "--format=%P", "factory/issue-8")); len(parents) != 2 || parents[1] != tip {
		t.Fatalf("head's parents = %v, main %s", parents, tip)
	}
	if got := git(t, e.remote, "show", "factory/issue-8:doc.go"); !strings.Contains(got, "both of ours") {
		t.Fatalf("doc.go = %q", got)
	}
}

// Work a stopped build kept is continued on today's base too.
func TestContinueBuildsOnTodaysBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.addIssue(9)
	finish := func(w string) error {
		if _, err := os.Stat(filepath.Join(w, "fixed.go")); err != nil {
			return err // still on the old base
		}
		return finishBuild(w)
	}
	e.claude.builds = []func(string) error{halfBuild, finish}
	e.claude.stops = []string{"turns"}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	stopped := implJobs(t, e, e.item(t, 9).ID)[0]
	pushTo(t, e.remote, map[string]string{"fixed.go": "package fixture\n\nvar Fixed = true\n"})
	if err := e.p.Continue(ctx, stopped.ID, 0.5, ""); err != nil {
		t.Fatal(err)
	}
	e.p.Drain(ctx)
	if st := e.item(t, 9).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s (%s)", st, implJobs(t, e, e.item(t, 9).ID)[0].Error)
	}
	tip := git(t, e.remote, "rev-parse", "main")
	git(t, e.remote, "merge-base", "--is-ancestor", tip, "factory/issue-9")
	for _, f := range []string{"add.go", "add_test.go", "fixed.go"} {
		git(t, e.remote, "cat-file", "-e", "factory/issue-9:"+f)
	}
}

// A continue that stops again while it resolves a merge keeps the merge in
// progress: the next continue picks up the resolution so far, not a merge
// started over on top of it, is still told the conflicts, and its
// resolution is the merge commit.
func TestContinueStoppedMidMergeKeepsTheMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.addIssue(10)
	half := func(w string) error {
		writeFile(w, "doc.go", "// Package fixture is ours.\npackage fixture\n")
		return halfBuild(w)
	}
	partly := func(w string) error { // resolves doc.go, then stops again
		return writeFile(w, "doc.go", "// Package fixture is both of ours.\npackage fixture\n")
	}
	finish := func(w string) error {
		if b, _ := os.ReadFile(filepath.Join(w, "doc.go")); !strings.Contains(string(b), "both of ours") || strings.Contains(string(b), "<<<<<<< ") {
			t.Errorf("the resolution so far went missing:\n%s", b)
		}
		return finishBuild(w)
	}
	e.claude.builds = []func(string) error{half, partly, finish}
	e.claude.stops = []string{"turns", "turns"}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	pushTo(t, e.remote, map[string]string{"doc.go": "// Package fixture is theirs.\npackage fixture\n"})
	for range 2 {
		stopped := implJobs(t, e, e.item(t, 10).ID)[0]
		if err := e.p.Continue(ctx, stopped.ID, 0.5, ""); err != nil {
			b, _ := os.ReadFile(filepath.Join(e.p.jobDir(stopped.ID), "stop.json"))
			t.Fatalf("%v: job %d %s %q\n%s", err, stopped.ID, stopped.State, stopped.Error, b)
		}
		e.p.Drain(ctx)
	}
	if st := e.item(t, 10).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s (%s)", st, implJobs(t, e, e.item(t, 10).ID)[0].Error)
	}
	if p := e.claude.prompts["implement"][2]; !strings.Contains(p, "The merge conflicted") || !strings.Contains(p, "- doc.go") {
		t.Fatalf("the last continue wasn't told the conflicts:\n%s", p)
	}
	tip := git(t, e.remote, "rev-parse", "main")
	if parents := strings.Fields(git(t, e.remote, "log", "-1", "--format=%P", "factory/issue-10")); len(parents) != 2 || parents[1] != tip {
		t.Fatalf("head's parents = %v, main %s", parents, tip)
	}
	if got := git(t, e.remote, "show", "factory/issue-10:doc.go"); !strings.Contains(got, "both of ours") {
		t.Fatalf("doc.go = %q", got)
	}
}

// A continue cancelled while it resolves a merge gives the checkout back
// mid-merge; the next continue goes on with that resolution too.
func TestCancelledContinueMidMergeKeepsTheMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.addIssue(11)
	half := func(w string) error {
		writeFile(w, "doc.go", "// Package fixture is ours.\npackage fixture\n")
		return halfBuild(w)
	}
	partlyThenCancel := func(w string) error {
		writeFile(w, "doc.go", "// Package fixture is both of ours.\npackage fixture\n")
		_ = e.p.Cancel(ctx, implJobs(t, e, e.item(t, 11).ID)[0].ID)
		return nil
	}
	finish := func(w string) error {
		if b, _ := os.ReadFile(filepath.Join(w, "doc.go")); !strings.Contains(string(b), "both of ours") || strings.Contains(string(b), "<<<<<<< ") {
			t.Errorf("the resolution so far went missing:\n%s", b)
		}
		return finishBuild(w)
	}
	e.claude.builds = []func(string) error{half, partlyThenCancel, finish}
	e.claude.stops = []string{"turns"}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	pushTo(t, e.remote, map[string]string{"doc.go": "// Package fixture is theirs.\npackage fixture\n"})
	for range 2 {
		last := implJobs(t, e, e.item(t, 11).ID)[0]
		if err := e.p.Continue(ctx, last.ID, 0, ""); err != nil {
			t.Fatal(err)
		}
		e.p.Drain(ctx)
	}
	if st := e.item(t, 11).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s (%s)", st, implJobs(t, e, e.item(t, 11).ID)[0].Error)
	}
	if p := e.claude.prompts["implement"][2]; !strings.Contains(p, "The merge conflicted") || !strings.Contains(p, "- doc.go") {
		t.Fatalf("the last continue wasn't told the conflicts:\n%s", p)
	}
	tip := git(t, e.remote, "rev-parse", "main")
	if parents := strings.Fields(git(t, e.remote, "log", "-1", "--format=%P", "factory/issue-11")); len(parents) != 2 || parents[1] != tip {
		t.Fatalf("head's parents = %v, main %s", parents, tip)
	}
}
