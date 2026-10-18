package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
)

// hostFiles lists a repo's files.paths (or what its .worktreeinclude picks)
// as files relative to the checkout they come from, which it also returns.
// A repo with no files gets "".
func (p *Pipeline) hostFiles(ctx context.Context, repo string) (string, []string, error) {
	f := p.repoConf(repo).Files
	if f == nil {
		return "", nil, nil
	}
	from, err := p.Cfg.FilesFrom(f)
	if err != nil {
		return "", nil, err
	}
	if fi, err := os.Stat(from); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("files: from %s is not a folder the factory can read", from)
	}
	if len(f.Paths) == 0 {
		rel, err := worktreeInclude(ctx, from)
		return from, rel, err
	}
	// Files git tracks in from are the repo's: a commit under test that
	// deletes one must not get the owner's copy back.
	tracked, _ := gitList(ctx, from, "ls-files", "-z")
	var rel []string
	add := func(r string) {
		if !tracked[r] {
			rel = append(rel, r)
		}
	}
	for _, raw := range f.Paths {
		c, err := config.CleanFilePath(raw)
		if err != nil {
			return "", nil, err
		}
		// Stat, not Lstat: from is the owner's own checkout, and a .env
		// there may well be a link to where they keep it.
		fi, err := os.Stat(filepath.Join(from, c))
		if err != nil {
			return "", nil, fmt.Errorf("files: %s is not in %s", c, from)
		}
		if !fi.IsDir() {
			if fi.Mode().IsRegular() {
				add(c)
			}
			continue
		}
		// WalkDir doesn't follow a link at its root, so walk where it
		// points, and name what it finds by the path under from.
		root, err := filepath.EvalSymlinks(filepath.Join(from, c))
		if err != nil {
			return "", nil, err
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			// Only files, or links to files: a socket or a FIFO (a
			// running server's) would fail or hang the copy.
			if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			r, _ := filepath.Rel(root, path)
			add(underDir(c, filepath.ToSlash(r)))
			return nil
		})
		if err != nil {
			return "", nil, err
		}
	}
	slices.Sort(rel)
	return from, slices.Compact(rel), nil
}

// worktreeInclude is what Claude Code copies into a new worktree: the files
// in from that match its .worktreeinclude and are gitignored.
func worktreeInclude(ctx context.Context, from string) ([]string, error) {
	inc := filepath.Join(from, ".worktreeinclude")
	if _, err := os.Stat(inc); err != nil {
		return nil, fmt.Errorf("files: %s has no .worktreeinclude, and files.paths is empty", from)
	}
	match, err := gitList(ctx, from, "ls-files", "-z", "--others", "--ignored", "--exclude-from="+inc)
	if err != nil {
		return nil, err
	}
	ignored, err := gitList(ctx, from, "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var rel []string
	for f := range match {
		// A nested repo comes back as its folder.
		if ignored[f] && !strings.HasSuffix(f, "/") {
			rel = append(rel, f)
		}
	}
	slices.Sort(rel)
	return rel, nil
}

// gitList runs a NUL-separated git listing in from, the owner's checkout.
// It names from as safe: the daemon often runs as its own user.
func gitList(ctx context.Context, from string, args ...string) (map[string]bool, error) {
	all := append(append(append([]string{}, safe...), "-c", "safe.directory="+from, "-C", from), args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("files: listing %s: %v: %s", from, err, strings.TrimSpace(stderr.String()))
	}
	set := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			set[f] = true
		}
	}
	return set, nil
}

// underDir joins a files.paths folder and a path under it.
func underDir(dir, rel string) string {
	if rel == "." {
		return dir
	}
	return dir + "/" + rel
}

// placeFiles copies rel from from into dst, a fresh checkout or export, and
// returns the ones it placed. It never overwrites what dst already has
// (the repo's own files win), and works through an os.Root, so a symlink
// a builder's commit planted in dst can't send a write anywhere else.
func placeFiles(from, dst string, rel []string) ([]string, error) {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var placed []string
	for _, r := range rel {
		ok, err := placeFile(root, filepath.Join(from, r), r)
		if err != nil {
			return placed, fmt.Errorf("files: %s: %w", r, err)
		}
		if ok {
			placed = append(placed, r)
		}
	}
	return placed, nil
}

func placeFile(root *os.Root, src, rel string) (bool, error) {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		fi, err := root.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(dir, 0o755); err != nil {
				return false, err
			}
		case err != nil:
			return false, err
		case !fi.IsDir():
			// A file or a link where the path needs a folder: the
			// repo has something of its own here.
			return false, nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, nil
	}
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()|0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}

// excludeFiles adds placed files to a clone's .git/info/exclude, so git
// status leaves them out.
func excludeFiles(repoDir string, placed []string) error {
	if len(placed) == 0 {
		return nil
	}
	path := filepath.Join(repoDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("\n# Copied in by the factory from files.from; never committed.\n")
	for _, r := range placed {
		b.WriteString("/" + escapePattern(r) + "\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// escapePattern makes a path a literal gitignore pattern.
func escapePattern(p string) string {
	var b strings.Builder
	for i, r := range p {
		if strings.ContainsRune(`*?[]\`, r) || (i == 0 && (r == '!' || r == '#')) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	if strings.HasSuffix(p, " ") {
		return strings.TrimSuffix(b.String(), " ") + `\ `
	}
	return b.String()
}

// CopyFiles places a repo's files (files.from) in dst, for a check run
// outside a job: readiness, or an eval's acceptance check.
func (p *Pipeline) CopyFiles(ctx context.Context, repo, dst string) error {
	_, err := p.copyFilesInto(ctx, repo, dst)
	return err
}

// copyFilesInto places a repo's files in a gate's or builder's folder.
func (p *Pipeline) copyFilesInto(ctx context.Context, repo, dst string) ([]string, error) {
	from, rel, err := p.hostFiles(ctx, repo)
	if err != nil || from == "" {
		return nil, err
	}
	return placeFiles(from, dst, rel)
}
