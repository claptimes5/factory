package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/claptimes5/factory/internal/diffrisk"
)

// Changes reads each file's status and lines, whatever its name holds,
// and Patch names them so the deleted-test check finds them.
func TestGitChangesAndPatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(dir, "old_test.go", "package x\n\nfunc TestGone(t *testing.T) {}\n")
	writeFile(dir, "keep.go", "package x\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	os.Remove(filepath.Join(dir, "old_test.go"))
	writeFile(dir, "keep.go", "package x\n\nvar A = 1\n")
	writeFile(dir, "a\tb.go", "package x\n")
	writeFile(dir, "nl\nx.go", "package x\n")
	writeFile(dir, "my tests/é_test.go", "package x\n\nfunc TestNew(t *testing.T) {}\n")
	os.WriteFile(filepath.Join(dir, "img.bin"), []byte{0, 1, 2, 0, 3}, 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "head")
	head := git(t, dir, "rev-parse", "HEAD")
	g := &Git{}
	files, err := g.Changes(ctx, dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]diffrisk.File{}
	for _, f := range files {
		got[f.Path] = f
	}
	for path, want := range map[string]diffrisk.File{
		"old_test.go":        {Path: "old_test.go", Status: "D", Deleted: 3},
		"keep.go":            {Path: "keep.go", Status: "M", Added: 2},
		"a\tb.go":            {Path: "a\tb.go", Status: "A", Added: 1},
		"nl\nx.go":           {Path: "nl\nx.go", Status: "A", Added: 1},
		"my tests/é_test.go": {Path: "my tests/é_test.go", Status: "A", Added: 3},
		"img.bin":            {Path: "img.bin", Status: "A", Binary: true},
	} {
		if got[path] != want {
			t.Errorf("%q: %+v, want %+v", path, got[path], want)
		}
	}
	if len(files) != 6 {
		t.Fatalf("files: %+v", files)
	}
	patch, err := g.Patch(ctx, dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	// Deleted whole, it's named by its file, not its test.
	a := diffrisk.Rate(diffrisk.Change{Files: files, Patch: patch, Before: func(string) (string, bool, error) { return "", false, nil },
		After: func(string) (string, bool, error) { return "", false, nil }}, diffrisk.Rules{NeedsPerson: diffrisk.Kinds})
	if len(a.Reasons) != 1 || a.Reasons[0].Text(diffrisk.Code) != "deletes tests: `old_test.go`" {
		t.Fatalf("reasons %+v", a.Reasons)
	}
}
