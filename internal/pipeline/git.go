package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/claptimes5/factory/internal/github"
)

// Git runs every git command the daemon makes, on the host, in
// repositories the daemon owns. The builder never gets a repository the
// daemon later runs git in: it works in a separate clone, and the daemon
// only reads that clone's files (never its .git), so a builder cannot plant
// hooks, fsmonitor commands, remotes or refs that git on the host would
// honor. Credentials reach git through GIT_ASKPASS from the environment of
// the one command that needs them, never on a command line or on disk.
type Git struct {
	RemoteURL  string             // e.g. https://github.com/owner/repo.git, or a local path in tests
	Tokens     github.TokenSource // nil for unauthenticated remotes
	Author     string             // "name <email>" for commits the daemon makes
	AskPassDir string             // where the askpass script is created ("" = os.TempDir)
	// User is the username sent with the token: x-access-token (GitHub's)
	// when empty; GitLab takes oauth2.
	User string

	askOnce sync.Once
	askPath string
	askErr  error
}

// safe disables every config knob that would make git run a program.
var safe = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper="}

func (g *Git) run(ctx context.Context, dir string, stdout io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	needsAuth := len(args) > 0 && (args[0] == "clone" || args[0] == "fetch" || args[0] == "push")
	if needsAuth && g.Tokens != nil {
		tok, err := g.Tokens.Token(ctx)
		if err != nil {
			return "", err
		}
		ap, err := g.askPass()
		if err != nil {
			return "", err
		}
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+ap, "FACTORY_GIT_PASSWORD="+tok)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g *Git) askPass() (string, error) {
	g.askOnce.Do(func() {
		// A fresh, private file: a predictable shared path could be planted.
		f, err := os.CreateTemp(g.AskPassDir, "factory-askpass-*.sh")
		if err != nil {
			g.askErr = err
			return
		}
		// Username is in the URL; the password comes from env.
		_, err = f.WriteString("#!/bin/sh\nprintf '%s\\n' \"$FACTORY_GIT_PASSWORD\"\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.Name(), 0o700)
		}
		g.askPath, g.askErr = f.Name(), err
	})
	return g.askPath, g.askErr
}

func (g *Git) remote() string {
	if g.Tokens != nil && strings.HasPrefix(g.RemoteURL, "https://") {
		user := g.User
		if user == "" {
			user = "x-access-token"
		}
		return "https://" + user + "@" + strings.TrimPrefix(g.RemoteURL, "https://")
	}
	return g.RemoteURL
}

// ident is the name and email the daemon commits with.
func (g *Git) ident() (name, email string) {
	if n, e, ok := strings.Cut(g.Author, " <"); ok {
		return n, strings.TrimSuffix(e, ">")
	}
	return "factory", "factory@localhost"
}

// AuthorEmail is the email on the commits the daemon makes.
func (g *Git) AuthorEmail() string {
	_, e := g.ident()
	return e
}

// HasBranch reports whether the remote had branch when repo was cloned.
func (g *Git) HasBranch(ctx context.Context, repo, branch string) bool {
	_, err := g.run(ctx, repo, nil, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	return err == nil
}

// Authors lists the author emails of the commits on to after from.
func (g *Git) Authors(ctx context.Context, repo, from, to string) ([]string, error) {
	out, err := g.run(ctx, repo, nil, "log", "--format=%ae", from+".."+to)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// Workspace is one job's checkouts.
type Workspace struct {
	Repo    string // daemon-owned clone; never writable by an agent
	Work    string // the builder's clone (implement jobs only)
	BaseSHA string // tip of the base branch when the job started
	Start   string // HEAD of the job's branch before the agent ran
	Existed bool   // the job's branch already existed on the remote
	// Placed are the files.from files copied into Work, which Ingest
	// never commits.
	Placed []string
}

// Prepare clones the remote into dir/repo and checks out branch (created
// from base when it does not exist, or reset to base when fresh is set).
// With builder set it also makes the builder's separate clone in dir/work.
func (g *Git) Prepare(ctx context.Context, dir, base, branch string, fresh, builder bool) (*Workspace, error) {
	work := ""
	if builder {
		work = filepath.Join(dir, "work")
	}
	return g.PrepareAt(ctx, filepath.Join(dir, "repo"), work, base, branch, fresh)
}

// PrepareAt is Prepare with the daemon's clone at repoDir and the
// builder's at workDir ("" for none), for a workspace's repos side by side.
func (g *Git) PrepareAt(ctx context.Context, repoDir, workDir, base, branch string, fresh bool) (*Workspace, error) {
	ws := &Workspace{Repo: repoDir}
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--no-tags", g.remote(), ws.Repo); err != nil {
		return nil, err
	}
	name, email := g.ident()
	for k, v := range map[string]string{"user.name": name, "user.email": email} {
		if _, err := g.run(ctx, ws.Repo, nil, "config", k, v); err != nil {
			return nil, err
		}
	}
	var err error
	if ws.BaseSHA, err = g.run(ctx, ws.Repo, nil, "rev-parse", "refs/remotes/origin/"+base); err != nil {
		return nil, err
	}
	target := "origin/" + base
	if branch != "" && branch != base {
		if _, err := g.run(ctx, ws.Repo, nil, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
			ws.Existed = true
			if !fresh {
				target = "origin/" + branch
			}
		}
	} else {
		branch = base
	}
	if _, err := g.run(ctx, ws.Repo, nil, "checkout", "--quiet", "-B", branch, target); err != nil {
		return nil, err
	}
	if ws.Start, err = g.run(ctx, ws.Repo, nil, "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	if workDir != "" {
		ws.Work = workDir
		// --no-local copies only what the branch reaches: a local clone would
		// copy the whole object store, other branches' work included.
		if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--no-local", "--single-branch", "--branch", branch, ws.Repo, ws.Work); err != nil {
			return nil, err
		}
		for k, v := range map[string]string{"user.name": name, "user.email": email} {
			if _, err := g.run(ctx, ws.Work, nil, "config", k, v); err != nil {
				return nil, err
			}
		}
	}
	return ws, nil
}

// Ingest commits the builder's working tree, read as plain files, into the
// daemon's repository and returns the new HEAD. changed is false when the
// builder left the tree identical to where it started.
func (g *Git) Ingest(ctx context.Context, ws *Workspace, msg string) (sha string, changed bool, err error) {
	gd := []string{"--git-dir=" + filepath.Join(ws.Repo, ".git"), "--work-tree=" + ws.Work}
	if _, err := g.run(ctx, ws.Repo, nil, append(gd, "add", "-A", "--", ".")...); err != nil {
		return "", false, err
	}
	// info/exclude keeps them out of add -A, but a builder's .gitignore
	// can override it, so take them back out by name.
	if len(ws.Placed) > 0 {
		args := append(gd, "rm", "-r", "--cached", "--quiet", "--ignore-unmatch", "--")
		for _, f := range ws.Placed {
			args = append(args, ":(literal)"+f)
		}
		if _, err := g.run(ctx, ws.Repo, nil, args...); err != nil {
			return "", false, err
		}
	}
	if _, err := g.run(ctx, ws.Repo, nil, append(gd, "diff", "--no-ext-diff", "--cached", "--quiet")...); err == nil {
		return ws.Start, false, nil
	}
	if _, err := g.run(ctx, ws.Repo, nil, append(gd, "commit", "--quiet", "--no-verify", "-m", msg)...); err != nil {
		return "", false, err
	}
	sha, err = g.run(ctx, ws.Repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	// Bring the daemon's own work tree in line with its new HEAD.
	_, err = g.run(ctx, ws.Repo, nil, "reset", "--quiet", "--hard", sha)
	return sha, true, err
}

// Export writes the tree of sha into dst as plain files (no .git). It uses
// a scratch index and checkout-index rather than git archive, so a builder's
// .gitattributes (export-ignore) cannot hide files from the gates.
func (g *Git) Export(ctx context.Context, repo, sha, dst string) error {
	abs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "factory-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	index := filepath.Join(tmp, "index")
	gd := "--git-dir=" + filepath.Join(repo, ".git")
	for _, args := range [][]string{{gd, "read-tree", sha}, {gd, "checkout-index", "-a", "-f", "--prefix=" + abs + "/"}} {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, safe...), args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[1], err, out)
		}
	}
	return nil
}

// Show writes one file of a commit to dst.
func (g *Git) Show(ctx context.Context, repo, sha, path, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = g.run(ctx, repo, f, "show", sha+":"+path)
	return err
}

// MergeBase returns the merge base of two commits.
func (g *Git) MergeBase(ctx context.Context, repo, a, b string) (string, error) {
	return g.run(ctx, repo, nil, "merge-base", a, b)
}

// CommitsSince counts commits on to after from.
func (g *Git) CommitsSince(ctx context.Context, repo, from, to string) (int, error) {
	out, err := g.run(ctx, repo, nil, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// Diff returns the patch between two commits.
func (g *Git) Diff(ctx context.Context, repo, from, to string) (string, error) {
	return g.run(ctx, repo, nil, "diff", "--no-ext-diff", "--no-textconv", from, to)
}

// DiffIn is Diff with every path under folder/, for a workspace's
// combined diff.
func (g *Git) DiffIn(ctx context.Context, repo, from, to, folder string) (string, error) {
	return g.run(ctx, repo, nil, "diff", "--no-ext-diff", "--no-textconv", "--src-prefix=a/"+folder+"/", "--dst-prefix=b/"+folder+"/", from, to)
}

// FileAt returns a file's contents at sha, and false if it isn't there.
func (g *Git) FileAt(ctx context.Context, repo, sha, path string) (string, bool, error) {
	// ls-tree prints nothing for a missing path, and fails only for a bad
	// commit, which is an error, not "no file".
	ls, err := g.run(ctx, repo, nil, "ls-tree", "--name-only", sha, "--", path)
	if err != nil {
		return "", false, err
	}
	if ls == "" {
		return "", false, nil
	}
	var b strings.Builder
	if _, err := g.run(ctx, repo, &b, "show", sha+":"+path); err != nil {
		return "", false, err
	}
	return b.String(), true, nil
}

// ChangedFiles lists paths changed between two commits.
func (g *Git) ChangedFiles(ctx context.Context, repo, from, to string) ([]string, error) {
	// -z: raw paths, never C-quoted (a quoted name matches no feature).
	// Written to a buffer, so run doesn't trim a path's leading space.
	var out strings.Builder
	if _, err := g.run(ctx, repo, &out, "diff", "--no-ext-diff", "--name-only", "--no-renames", "-z", from, to); err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out.String(), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// Push pushes exactly sha (not whatever HEAD is) to branch on the remote.
func (g *Git) Push(ctx context.Context, repo, sha, branch string, force bool) error {
	args := []string{"push", "--quiet", g.remote(), sha + ":refs/heads/" + branch}
	if force {
		args = append(args, "--force")
	}
	_, err := g.run(ctx, repo, nil, args...)
	return err
}

// Freeze deletes every ref of a bare repository but keep, and prunes what
// is no longer reachable, so nothing cloned from it can hold other work.
func (g *Git) Freeze(ctx context.Context, bare, keep string) error {
	gd := "--git-dir=" + bare
	refs, err := g.run(ctx, "", nil, gd, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if ref != keep {
			if _, err := g.run(ctx, "", nil, gd, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	if _, err := g.run(ctx, "", nil, gd, "reflog", "expire", "--expire=now", "--all"); err != nil {
		return err
	}
	_, err = g.run(ctx, "", nil, gd, "gc", "--quiet", "--prune=now")
	return err
}

// FetchRef fetches one ref from the remote (a pull request's head, for
// example) and returns the commit it points at.
func (g *Git) FetchRef(ctx context.Context, repo, ref string) (string, error) {
	// Into a ref of its own, not FETCH_HEAD, which a fetch can leave stale.
	if _, err := g.run(ctx, repo, nil, "fetch", "--quiet", "--no-tags", g.remote(), "+"+ref+":refs/factory/fetched"); err != nil {
		return "", err
	}
	return g.run(ctx, repo, nil, "rev-parse", "--verify", "refs/factory/fetched^{commit}")
}

// IsAncestor reports whether commit a is an ancestor of (or is) b.
func (g *Git) IsAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := g.run(ctx, repo, nil, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Mode is a path's git mode at sha (100644, 120000 for a symlink, 160000
// for a submodule), or "" when it isn't there.
func (g *Git) Mode(ctx context.Context, repo, sha, path string) (string, error) {
	out, err := g.run(ctx, repo, nil, "ls-tree", sha, "--", path)
	if err != nil || out == "" {
		return "", err
	}
	mode, _, _ := strings.Cut(out, " ")
	return mode, nil
}

// CloneBare makes a bare copy of the remote at dst.
func (g *Git) CloneBare(ctx context.Context, dst string) error {
	_, err := g.run(ctx, "", nil, "clone", "--quiet", "--bare", "--no-tags", g.remote(), dst)
	return err
}
