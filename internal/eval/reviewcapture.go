package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/claptimes5/factory/internal/pipeline"
	"gopkg.in/yaml.v3"
)

// ReviewCaptured is what CaptureReview wrote.
type ReviewCaptured struct {
	Dirs  []string
	Notes []string
}

// CaptureReview writes reviewer cases from a merged change, each in its
// own directory under set, beside the builder's cases: name-clean (or
// name-revert, when revert is set: the change was later reverted, so the
// reviewer should have blocked it), and for a change that stood, one
// name-plant-<mutation> for each mutation that fits a line it added to Go
// code. Each holds change.patch, the change from its base. A case that
// already exists is left as it is.
func CaptureReview(ctx context.Context, git *pipeline.Git, repoDir, set, name string, f Fix, revert bool) (*ReviewCaptured, error) {
	if f.Base == "" || f.Head == "" || f.Title == "" {
		return nil, errors.New("capture: base, head and title are required")
	}
	diff, err := git.Diff(ctx, repoDir, f.Base, f.Head)
	if err != nil {
		return nil, err
	}
	if diff == "" {
		return nil, errors.New("capture: the change is empty")
	}
	diff += "\n" // as git apply wants it, which Diff trims
	out := &ReviewCaptured{}
	base := ReviewCase{Repo: f.Repo, Source: f.Source, BaseSHA: f.Base, Title: f.Title, Body: f.Body}
	write := func(n string, c ReviewCase) error {
		if !safeName(n) {
			return fmt.Errorf("capture: %q is not a usable case name", n)
		}
		if Exists(set, n) {
			out.Notes = append(out.Notes, filepath.Join(set, n)+" already exists; left as it is")
			return nil
		}
		dir, err := writeReviewCase(set, n, c, diff)
		if err == nil {
			out.Dirs = append(out.Dirs, dir)
		}
		return err
	}
	if revert {
		c := base
		c.Kind = KindRevert
		return out, write(name+"-revert", c)
	}
	c := base
	c.Kind = KindClean
	if err := write(name+"-clean", c); err != nil {
		return out, err
	}
	added := AddedLines(diff)
	files := make([]string, 0, len(added))
	for file := range added {
		if Plantable(file) && safePath(file) {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	for _, m := range Mutations {
		planted := false
		for _, file := range files {
			src, ok, err := git.FileAt(ctx, repoDir, f.Head, file)
			if err != nil {
				return out, err
			}
			if !ok {
				continue
			}
			if _, line, err := PlantBug(src, added[file], m); err == nil {
				c := base
				c.Kind, c.Plant = KindPlanted, &Plant{Mutation: m, File: file, Line: line}
				if err := write(name+"-plant-"+m, c); err != nil {
					return out, err
				}
				planted = true
				break
			}
		}
		if !planted {
			out.Notes = append(out.Notes, m+": nothing the change added to Go code fits it")
		}
	}
	return out, nil
}

// writeReviewCase writes set/name whole or not at all, as Capture does.
func writeReviewCase(set, name string, c ReviewCase, diff string) (string, error) {
	if err := os.MkdirAll(set, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(set, ".capture-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	b, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "case.yaml"), b, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, PatchFile), []byte(diff), 0o644); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	dir := filepath.Join(set, name)
	return dir, os.Rename(tmp, dir)
}
