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
	RemoteURL string             // e.g. https://github.com/owner/repo.git, or a local path in tests
	Tokens    github.TokenSource // nil for unauthenticated remotes
	Author    string             // "name <email>" for commits the daemon makes
	AskPass   string             // path of an askpass script; created on first use
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
	if g.AskPass == "" {
		g.AskPass = filepath.Join(os.TempDir(), "factory-askpass.sh")
	}
	if _, err := os.Stat(g.AskPass); err == nil {
		return g.AskPass, nil
	}
	// Username is in the URL (x-access-token); the password comes from env.
	err := os.WriteFile(g.AskPass, []byte("#!/bin/sh\nprintf '%s\\n' \"$FACTORY_GIT_PASSWORD\"\n"), 0o700)
	return g.AskPass, err
}

func (g *Git) remote() string {
	if g.Tokens != nil && strings.HasPrefix(g.RemoteURL, "https://") {
		return "https://x-access-token@" + strings.TrimPrefix(g.RemoteURL, "https://")
	}
	return g.RemoteURL
}

// Workspace is one job's checkouts.
type Workspace struct {
	Repo    string // daemon-owned clone; never writable by an agent
	Work    string // the builder's clone (implement jobs only)
	BaseSHA string // tip of the base branch when the job started
	Start   string // HEAD of the job's branch before the agent ran
	Existed bool   // the job's branch already existed on the remote
}

// Prepare clones the remote into dir/repo and checks out branch (created
// from base when it does not exist, or reset to base when fresh is set).
// With builder set it also makes the builder's separate clone in dir/work.
func (g *Git) Prepare(ctx context.Context, dir, base, branch string, fresh, builder bool) (*Workspace, error) {
	ws := &Workspace{Repo: filepath.Join(dir, "repo")}
	if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--no-tags", g.remote(), ws.Repo); err != nil {
		return nil, err
	}
	name, email := "factory", "factory@localhost"
	if n, e, ok := strings.Cut(g.Author, " <"); ok {
		name, email = n, strings.TrimSuffix(e, ">")
	}
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
	if builder {
		ws.Work = filepath.Join(dir, "work")
		if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--no-hardlinks", "--branch", branch, ws.Repo, ws.Work); err != nil {
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

// ChangedFiles lists paths changed between two commits.
func (g *Git) ChangedFiles(ctx context.Context, repo, from, to string) ([]string, error) {
	out, err := g.run(ctx, repo, nil, "diff", "--no-ext-diff", "--name-only", "--no-renames", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
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
