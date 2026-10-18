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
	from := p.Cfg.FilesFrom(f)
	if len(f.Paths) == 0 {
		rel, err := worktreeInclude(ctx, from)
		return from, rel, err
	}
	var rel []string
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
			rel = append(rel, c)
			continue
		}
		err = filepath.WalkDir(filepath.Join(from, c), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			r, _ := filepath.Rel(from, path)
			rel = append(rel, filepath.ToSlash(r))
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
	list := func(args ...string) (map[string]bool, error) {
		cmd := exec.CommandContext(ctx, "git", append(append(append([]string{}, safe...), "-C", from, "ls-files", "-z", "--others", "--ignored"), args...)...)
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
	match, err := list("--exclude-from=" + filepath.Join(from, ".worktreeinclude"))
	if err != nil {
		return nil, err
	}
	ignored, err := list("--exclude-standard")
	if err != nil {
		return nil, err
	}
	var rel []string
	for f := range match {
		if ignored[f] {
			rel = append(rel, f)
		}
	}
	slices.Sort(rel)
	return rel, nil
}

// placeFiles copies rel from from into dst, a fresh checkout or export, and
// returns the ones it placed. It never overwrites what dst already has
// (the repo's own files win), nor writes through a symlink in dst, which
// a builder's commit could have planted to point anywhere on the host.
func placeFiles(from, dst string, rel []string) ([]string, error) {
	var placed []string
	for _, r := range rel {
		ok, err := placeFile(filepath.Join(from, r), dst, r)
		if err != nil {
			return placed, fmt.Errorf("files: %s: %w", r, err)
		}
		if ok {
			placed = append(placed, r)
		}
	}
	return placed, nil
}

func placeFile(src, dst, rel string) (bool, error) {
	parts := strings.Split(rel, "/")
	dir := dst
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
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
	out, err := os.OpenFile(filepath.Join(dir, parts[len(parts)-1]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()|0o600)
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

// copyFilesInto places a repo's files in a gate's or builder's folder.
func (p *Pipeline) copyFilesInto(ctx context.Context, repo, dst string) ([]string, error) {
	from, rel, err := p.hostFiles(ctx, repo)
	if err != nil || from == "" {
		return nil, err
	}
	return placeFiles(from, dst, rel)
}
