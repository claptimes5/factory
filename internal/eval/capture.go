package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/pipeline"
	"gopkg.in/yaml.v3"
)

// Fix is a merged change to turn into a replay case.
type Fix struct {
	Repo   string // the case's repo field; empty for the only configured repo
	Source string // where it came from, e.g. "PR #14"
	Title  string // the issue it fixed, as filed
	Body   string
	Base   string // the commit the fix was built on
	Head   string // the fix as merged
	Accept string // the command that proves it
}

// Captured is the case Capture wrote.
type Captured struct {
	Dir   string
	Tests []string // the fix's test files, now in accept/
}

// Capture writes set/name from a merged fix: case.yaml frozen at the
// fix's base, and accept/ holding the fix's own test files as merged, so
// the case passes only when the factory's result passes the tests the
// human's fix came with.
func Capture(ctx context.Context, git *pipeline.Git, repoDir, set, name string, f Fix) (*Captured, error) {
	if f.Base == "" || f.Head == "" || f.Title == "" || f.Accept == "" {
		return nil, errors.New("capture: base, head, title and accept are required")
	}
	if !safeName(name) {
		return nil, fmt.Errorf("capture: %q is not a usable case name", name)
	}
	dir := filepath.Join(set, name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("capture: %s already exists", dir)
	}
	changed, err := git.ChangedFiles(ctx, repoDir, f.Base, f.Head)
	if err != nil {
		return nil, err
	}
	c := &Captured{Dir: dir}
	for _, p := range changed {
		if !gates.IsTest(p) || !safePath(p) {
			continue
		}
		body, ok, err := git.FileAt(ctx, repoDir, f.Head, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // the fix deleted it
		}
		dst := filepath.Join(dir, "accept", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
			return nil, err
		}
		c.Tests = append(c.Tests, p)
	}
	b, err := yaml.Marshal(Case{Repo: f.Repo, Source: f.Source, BaseSHA: f.Base, Title: f.Title, Body: f.Body, Accept: f.Accept})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return c, os.WriteFile(filepath.Join(dir, "case.yaml"), b, 0o644)
}

// safePath keeps a repo path inside accept/.
func safePath(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func safeName(n string) bool { return nameRE.MatchString(n) && !strings.Contains(n, "..") }

// CaseName makes a directory name from an issue title.
func CaseName(prefix, title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return prefix
	}
	return prefix + "-" + s
}

var closesRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// ClosedIssue is the issue a PR body says it closes, or 0.
func ClosedIssue(body string) int {
	m := closesRE.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1]) // can't fail past the \d+ but for overflow, which is 0
	return n
}
