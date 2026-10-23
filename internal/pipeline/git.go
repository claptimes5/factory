package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/diffrisk"
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
	AskPassDir string             // where the askpass script lives ("" = a fresh file in os.TempDir)
	// User is the username sent with the token: x-access-token (GitHub's)
	// when empty; GitLab takes oauth2.
	User string

	askOnce sync.Once
	askPath string
	askErr  error
	// partial holds the partial clones' directories: any command in one
	// may fetch a file's contents from the remote, so each gets the
	// credentials, not only clone, fetch and push.
	partial sync.Map
	// trace, when set (tests), sees each command's directory, arguments
	// and environment.
	trace func(dir string, args, env []string)
}

// safe disables every config knob that would make git run a program,
// including a signer set in the host's global config.
var safe = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}

func (g *Git) run(ctx context.Context, dir string, stdout io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	needsAuth := len(args) > 0 && (args[0] == "clone" || args[0] == "fetch" || args[0] == "push")
	if _, ok := g.partial.Load(dir); ok && dir != "" {
		needsAuth = true // checkout, merge and the like fetch missing blobs
	}
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
	if g.trace != nil {
		g.trace(dir, args, cmd.Env)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g *Git) askPass() (string, error) {
	g.askOnce.Do(func() { g.askPath, g.askErr = writeAskPass(g.AskPassDir) })
	return g.askPath, g.askErr
}

const askPassScript = "#!/bin/sh\nprintf '%s\\n' \"$FACTORY_GIT_PASSWORD\"\n" // the password comes from env

// writeAskPass writes the askpass script into dir, and returns its path.
// In a directory of the factory's own (the data directory) it is one file,
// factory-askpass.sh, replaced whole by a rename, so every process and
// every start shares it rather than leaving one behind each. In the shared
// temp directory it is a fresh private file: a predictable path there
// could be planted.
func writeAskPass(dir string) (string, error) {
	path := filepath.Join(dir, "factory-askpass.sh")
	if dir != "" {
		if b, err := os.ReadFile(path); err == nil && string(b) == askPassScript {
			if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == 0o700 {
				sweepAskPass(dir)
				return path, nil
			}
		}
	}
	f, err := os.CreateTemp(dir, "factory-askpass-*.sh")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(askPassScript)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o700)
	}
	if err != nil || dir == "" {
		return f.Name(), err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	sweepAskPass(dir)
	return path, nil
}

// sweepAskPass removes the factory-askpass-*.sh earlier versions left in
// dir, one per start; one an hour old is no other process's mid-write.
func sweepAskPass(dir string) {
	old, _ := filepath.Glob(filepath.Join(dir, "factory-askpass-*.sh"))
	for _, o := range old {
		if fi, err := os.Stat(o); err == nil && time.Since(fi.ModTime()) > time.Hour {
			_ = os.Remove(o)
		}
	}
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
	// Merging is the base commit a merge in progress takes in, and
	// Conflicts the paths it left for the builder to resolve (see
	// MergeIn); the next Ingest commits the merge.
	Merging   string
	Conflicts []string
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
	return g.prepareAt(ctx, repoDir, workDir, base, branch, fresh)
}

// PrepareSync clones the remote into repoDir with branch checked out at
// its remote head, for merging base into it: a partial clone, which reads
// a file's contents only when the checkout or merge needs them, so it
// stays quick on a large repository. (A local remote ignores the filter.)
// Every git command in it then gets the remote's credentials, since any
// may fetch contents; call DoneSync once it's no longer used.
func (g *Git) PrepareSync(ctx context.Context, repoDir, base, branch string) (*Workspace, error) {
	g.partial.Store(repoDir, true)
	return g.prepareAt(ctx, repoDir, "", base, branch, false, "--filter=blob:none")
}

// DoneSync forgets a clone PrepareSync made.
func (g *Git) DoneSync(repoDir string) { g.partial.Delete(repoDir) }

func (g *Git) prepareAt(ctx context.Context, repoDir, workDir, base, branch string, fresh bool, clone ...string) (*Workspace, error) {
	ws := &Workspace{Repo: repoDir}
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return nil, err
	}
	args := append([]string{"clone", "--quiet", "--no-tags"}, clone...)
	if _, err := g.run(ctx, "", nil, append(args, g.remote(), ws.Repo)...); err != nil {
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
		if err := g.cloneWork(ctx, ws, branch); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// cloneWork makes the builder's clone of the daemon's, at ws.Work.
func (g *Git) cloneWork(ctx context.Context, ws *Workspace, branch string) error {
	// --no-local copies only what the branch reaches: a local clone would
	// copy the whole object store, other branches' work included.
	if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--no-local", "--single-branch", "--branch", branch, ws.Repo, ws.Work); err != nil {
		return err
	}
	name, email := g.ident()
	for k, v := range map[string]string{"user.name": name, "user.email": email} {
		if _, err := g.run(ctx, ws.Work, nil, "config", k, v); err != nil {
			return err
		}
	}
	return nil
}

// FetchBase fetches the remote's base into the daemon's clone, makes its
// tip ws.BaseSHA, and says whether the checked-out branch is behind it.
func (g *Git) FetchBase(ctx context.Context, ws *Workspace, base string) (bool, error) {
	ref := "refs/remotes/origin/" + base
	if _, err := g.run(ctx, ws.Repo, nil, "fetch", "--quiet", "--no-tags", g.remote(), "+refs/heads/"+base+":"+ref); err != nil {
		return false, err
	}
	tip, err := g.run(ctx, ws.Repo, nil, "rev-parse", ref)
	if err != nil {
		return false, err
	}
	ws.BaseSHA = tip
	return !g.IsAncestor(ctx, ws.Repo, tip, "HEAD"), nil
}

// MergeIn merges the base FetchBase fetched into the checked-out branch.
// A clean merge is committed with msg and becomes ws.Start. One that
// conflicts is left in progress, in ws.Merging and ws.Conflicts: the next
// Ingest commits the builder's resolution as the merge. Any other failure
// leaves the branch as it was.
func (g *Git) MergeIn(ctx context.Context, ws *Workspace, base, msg string) error {
	_, err := g.run(ctx, ws.Repo, nil, "merge", "--quiet", "--no-ff", "--no-edit", "-m", msg, "refs/remotes/origin/"+base)
	if err == nil {
		ws.Start, err = g.run(ctx, ws.Repo, nil, "rev-parse", "HEAD")
		return err
	}
	if head, conflicts := g.PendingMerge(ctx, ws.Repo); len(conflicts) > 0 {
		ws.Merging, ws.Conflicts = head, conflicts
		return nil
	}
	_, _ = g.run(ctx, ws.Repo, nil, "merge", "--abort")
	return err
}

// PendingMerge is the commit a merge in progress in repo takes in ("" for
// none), and the paths it left conflicted.
func (g *Git) PendingMerge(ctx context.Context, repo string) (string, []string) {
	head, err := g.run(ctx, repo, nil, "rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	if err != nil {
		return "", nil
	}
	var out strings.Builder
	if _, err := g.run(ctx, repo, &out, "diff", "--name-only", "-z", "--diff-filter=U"); err != nil || out.Len() == 0 {
		return head, nil
	}
	return head, strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
}

// Remerge marks the merge of ws.Merging in progress again in repo, after
// an Ingest committed it and Unstep took the commit back, so the next
// Ingest commits a merge again.
func (g *Git) Remerge(ctx context.Context, ws *Workspace) error {
	if ws.Merging == "" {
		return nil
	}
	// git won't update-ref a pseudoref, so write it as a merge does.
	path, err := g.run(ctx, ws.Repo, nil, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(ws.Merging+"\n"), 0o644)
}

// RebuildWork replaces the builder's clone with a new one of the daemon's
// branch, after a merge changed it. While a merge is in progress the new
// clone also gets the daemon's merged files, conflict markers and all.
func (g *Git) RebuildWork(ctx context.Context, ws *Workspace) error {
	branch, err := g.run(ctx, ws.Repo, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(ws.Work); err != nil {
		return err
	}
	if err := g.cloneWork(ctx, ws, branch); err != nil {
		return err
	}
	if ws.Merging == "" {
		return nil
	}
	var out strings.Builder
	if _, err := g.run(ctx, ws.Repo, &out, "diff", "--name-only", "-z", "--no-renames", "HEAD"); err != nil {
		return err
	}
	for _, f := range strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00") {
		if f != "" {
			if err := copyPath(filepath.Join(ws.Repo, f), filepath.Join(ws.Work, f)); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyPath makes dst what src is: a file, a symlink, or gone.
func copyPath(src, dst string) error {
	fi, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if fi.Mode()&os.ModeSymlink != 0 {
		to, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(to, dst)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, fi.Mode().Perm())
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
	ws.Merging, ws.Conflicts = "", nil
	// Bring the daemon's own work tree in line with its new HEAD.
	_, err = g.run(ctx, ws.Repo, nil, "reset", "--quiet", "--hard", sha)
	return sha, true, err
}

// Unstep moves the branch of repo back to sha and leaves the index and
// work tree as they are, so the next Ingest commits them again on sha.
func (g *Git) Unstep(ctx context.Context, repo, sha string) error {
	_, err := g.run(ctx, repo, nil, "reset", "--quiet", "--soft", sha)
	return err
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

// Patch is the patch between two commits as the diff's rating reads it:
// renames not followed, and paths unquoted where git allows (a name with
// a quote, a backslash or a control character is still C-quoted).
func (g *Git) Patch(ctx context.Context, repo, from, to string) (string, error) {
	var out strings.Builder // not trimmed: a path may end in a space
	_, err := g.run(ctx, repo, &out, "-c", "core.quotePath=false", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", from, to)
	return out.String(), err
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

// errTooBig is a file FileAtMost won't read.
var errTooBig = errors.New("file too big")

// FileAtMost is FileAt for a regular file of at most max bytes: a bigger
// one is errTooBig, unread, and a link or a folder isn't there.
func (g *Git) FileAtMost(ctx context.Context, repo, sha, path string, max int64) (string, bool, error) {
	// "<mode> blob <object> <size>\t<path>"
	ls, err := g.run(ctx, repo, nil, "ls-tree", "-l", sha, "--", path)
	if err != nil {
		return "", false, err
	}
	meta, _, _ := strings.Cut(ls, "\t")
	f := strings.Fields(meta)
	if len(f) != 4 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
		return "", false, nil
	}
	if size, err := strconv.ParseInt(f[3], 10, 64); err != nil || size > max {
		return "", true, errTooBig
	}
	var b strings.Builder
	if _, err := g.run(ctx, repo, &b, "cat-file", "blob", f[2]); err != nil {
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

// Changes lists the files changed between two commits with their status
// and line counts. Renames aren't followed: one is a delete and an add.
func (g *Git) Changes(ctx context.Context, repo, from, to string) ([]diffrisk.File, error) {
	// -z: raw paths, never C-quoted; to a buffer, so no path is trimmed.
	var st, ns strings.Builder
	if _, err := g.run(ctx, repo, &st, "diff", "--no-ext-diff", "--no-renames", "--name-status", "-z", from, to); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, repo, &ns, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--numstat", "-z", from, to); err != nil {
		return nil, err
	}
	var files []diffrisk.File
	byPath := map[string]int{}
	f := strings.Split(strings.TrimSuffix(st.String(), "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		byPath[f[i+1]] = len(files)
		files = append(files, diffrisk.File{Path: f[i+1], Status: f[i][:1]})
	}
	for _, rec := range strings.Split(ns.String(), "\x00") {
		added, rest, _ := strings.Cut(rec, "\t")
		deleted, path, ok := strings.Cut(rest, "\t")
		i, known := byPath[path]
		if !ok || !known {
			continue
		}
		if added == "-" {
			files[i].Binary = true
			continue
		}
		files[i].Added, _ = strconv.Atoi(added)
		files[i].Deleted, _ = strconv.Atoi(deleted)
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

// MergeInto merges ref into the checked-out branch of repo with a merge
// commit, and returns it. On a conflict it leaves the branch as it was.
func (g *Git) MergeInto(ctx context.Context, repo, ref, msg string) (string, error) {
	if _, err := g.run(ctx, repo, nil, "merge", "--quiet", "--no-ff", "--no-edit", "-m", msg, ref); err != nil {
		_, _ = g.run(ctx, repo, nil, "merge", "--abort")
		return "", err
	}
	return g.run(ctx, repo, nil, "rev-parse", "HEAD")
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

// CloneShallow makes a bare copy of only the tip of the remote's branch.
func (g *Git) CloneShallow(ctx context.Context, dst, branch string) error {
	_, err := g.run(ctx, "", nil, "clone", "--quiet", "--bare", "--depth", "1", "--single-branch", "--branch", branch, "--no-tags", g.remote(), dst)
	return err
}

// GrepHit is a line that matched.
type GrepHit struct {
	Path string
	Line int
	Text string
}

// Grep lists the lines of the files git tracks at sha that hold any of
// the fixed strings, leaving out binary files and the excluded pathspecs.
// It reads at most max of them, stopping git there, and says whether
// there were more.
func (g *Git) Grep(ctx context.Context, repo, sha string, fixed, exclude []string, max int) ([]GrepHit, bool, error) {
	args := []string{"grep", "-I", "-n", "-z", "--no-color", "--full-name", "-F"}
	for _, f := range fixed {
		args = append(args, "-e", f)
	}
	args = append(args, sha, "--", ".")
	for _, x := range exclude {
		args = append(args, ":(exclude,glob)"+x)
	}
	w := &grepWriter{prefix: sha + ":", max: max}
	_, err := g.run(ctx, repo, w, args...)
	if w.full {
		return w.hits, true, nil // git stopped when the writer did
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, false, nil // nothing matched
		}
		return nil, false, err
	}
	w.line(w.rest) // a last line with no newline
	return w.hits, false, nil
}

// errGrepFull stops git grep once enough lines are read.
var errGrepFull = errors.New("enough lines")

// grepWriter parses git grep -z output as it comes: sha:path NUL line
// NUL text, one per line.
type grepWriter struct {
	prefix string
	max    int
	hits   []GrepHit
	rest   []byte
	full   bool
}

func (w *grepWriter) Write(b []byte) (int, error) {
	if w.full {
		return 0, errGrepFull
	}
	w.rest = append(w.rest, b...)
	for {
		i := bytes.IndexByte(w.rest, '\n')
		if i < 0 {
			break
		}
		w.line(w.rest[:i])
		w.rest = w.rest[i+1:]
		if len(w.hits) > w.max {
			w.hits, w.full = w.hits[:w.max], true
			return 0, errGrepFull
		}
	}
	return len(b), nil
}

func (w *grepWriter) line(l []byte) {
	parts := strings.SplitN(string(l), "\x00", 3)
	if len(parts) != 3 {
		return
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	w.hits = append(w.hits, GrepHit{Path: strings.TrimPrefix(parts[0], w.prefix), Line: n, Text: parts[2]})
}
