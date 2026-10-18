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
	Tests []string // the fix's test files and test data, now in accept/
	Notes []string // what wasn't copied, and why
}

// Exists reports whether set already has a case called name.
func Exists(set, name string) bool {
	_, err := os.Stat(filepath.Join(set, name))
	return err == nil
}

// Capture writes set/name from a merged fix: case.yaml frozen at the
// fix's base, and accept/ holding the fix's own test files and test data
// as merged, so the case passes only when the factory's result passes the
// tests the human's fix came with. It writes all of it or nothing.
func Capture(ctx context.Context, git *pipeline.Git, repoDir, set, name string, f Fix) (*Captured, error) {
	if f.Base == "" || f.Head == "" || f.Title == "" || f.Accept == "" {
		return nil, errors.New("capture: base, head, title and accept are required")
	}
	if !safeName(name) {
		return nil, fmt.Errorf("capture: %q is not a usable case name", name)
	}
	dir := filepath.Join(set, name)
	if Exists(set, name) {
		return nil, fmt.Errorf("capture: %s already exists", dir)
	}
	changed, err := git.ChangedFiles(ctx, repoDir, f.Base, f.Head)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(set, 0o755); err != nil {
		return nil, err
	}
	// Built beside it and moved into place, so a failure leaves no partial
	// case to block a retry (Load skips the dot directory meanwhile).
	tmp, err := os.MkdirTemp(set, ".capture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	c := &Captured{Dir: dir}
	for _, p := range changed {
		if !testFile(p) {
			continue
		}
		if !safePath(p) {
			c.Notes = append(c.Notes, fmt.Sprintf("%s: not copied, the path isn't usable", p))
			continue
		}
		mode, err := git.Mode(ctx, repoDir, f.Head, p)
		if err != nil {
			return nil, err
		}
		switch mode {
		case "":
			c.Notes = append(c.Notes, fmt.Sprintf("%s: the fix deleted it, and accept/ can only add files; delete it from the result if it's stale", p))
			continue
		case "100644", "100755":
		default:
			c.Notes = append(c.Notes, fmt.Sprintf("%s: not copied, it's a symlink or submodule; copy what it points at into accept/ by hand", p))
			continue
		}
		body, _, err := git.FileAt(ctx, repoDir, f.Head, p)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(tmp, "accept", filepath.FromSlash(p))
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
	if err := os.WriteFile(filepath.Join(tmp, "case.yaml"), b, 0o644); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	return c, nil
}

// testFile is a test, or data only tests read (fixtures, snapshots, and
// anything else under a test directory), which a fix's tests may need.
// Never app code: laid over the factory's result, the fix itself would
// make every candidate pass. So no bare spec/, which is as often an API
// spec as tests.
func testFile(p string) bool {
	if gates.IsTest(p) || path.Ext(p) == ".snap" {
		return true
	}
	for _, d := range strings.Split(path.Dir(p), "/") {
		switch d {
		case "testdata", "fixtures", "__fixtures__", "__snapshots__", "test", "tests", "__tests__", "e2e", "cypress":
			return true
		}
	}
	return false
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
