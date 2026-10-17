package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/github"
)

// Git clones and pushes on the daemon's host. Credentials are passed per
// command as an HTTP header and never written into the checkout, so a job
// container that sees the checkout cannot push.
type Git struct {
	RemoteURL string             // e.g. https://github.com/owner/repo.git, or a local path in tests
	Tokens    github.TokenSource // nil for unauthenticated remotes
	Author    string             // "name <email>" for commits the daemon makes
}

func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	pre := []string{}
	if g.Tokens != nil {
		tok, err := g.Tokens.Token(ctx)
		if err != nil {
			return "", err
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
		pre = append(pre, "-c", "http.extraHeader=AUTHORIZATION: basic "+basic)
	}
	cmd := exec.CommandContext(ctx, "git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Checkout clones the remote into dir and checks out branch. If branch does
// not exist on the remote it is created from base. It returns the HEAD sha
// and whether the branch already existed.
func (g *Git) Checkout(ctx context.Context, dir, base, branch string) (string, bool, error) {
	if _, err := g.run(ctx, "", "clone", "--quiet", "--no-tags", g.RemoteURL, dir); err != nil {
		return "", false, err
	}
	name, email := "factory", "factory@localhost"
	if n, e, ok := strings.Cut(g.Author, " <"); ok {
		name, email = n, strings.TrimSuffix(e, ">")
	}
	for k, v := range map[string]string{"user.name": name, "user.email": email} {
		if _, err := g.run(ctx, dir, "config", k, v); err != nil {
			return "", false, err
		}
	}
	existed := false
	if branch != "" && branch != base {
		if _, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
			existed = true
			if _, err := g.run(ctx, dir, "checkout", "--quiet", "-B", branch, "origin/"+branch); err != nil {
				return "", false, err
			}
		} else if _, err := g.run(ctx, dir, "checkout", "--quiet", "-B", branch, "origin/"+base); err != nil {
			return "", false, err
		}
	} else if _, err := g.run(ctx, dir, "checkout", "--quiet", base); err != nil {
		return "", false, err
	}
	sha, err := g.run(ctx, dir, "rev-parse", "HEAD")
	return sha, existed, err
}

// MergeBase returns the merge base of HEAD and origin/base.
func (g *Git) MergeBase(ctx context.Context, dir, base string) (string, error) {
	return g.run(ctx, dir, "merge-base", "HEAD", "origin/"+base)
}

// CommitsSince counts commits on HEAD after sha.
func (g *Git) CommitsSince(ctx context.Context, dir, sha string) (int, error) {
	out, err := g.run(ctx, dir, "rev-list", "--count", sha+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// CommitAll commits any uncommitted changes the builder left behind.
func (g *Git) CommitAll(ctx context.Context, dir, msg string) (bool, error) {
	st, err := g.run(ctx, dir, "status", "--porcelain")
	if err != nil || st == "" {
		return false, err
	}
	if _, err := g.run(ctx, dir, "add", "-A"); err != nil {
		return false, err
	}
	_, err = g.run(ctx, dir, "commit", "--quiet", "--no-verify", "-m", msg)
	return err == nil, err
}

// Diff returns the patch from sha to HEAD.
func (g *Git) Diff(ctx context.Context, dir, sha string) (string, error) {
	return g.run(ctx, dir, "diff", sha, "HEAD")
}

// Push pushes HEAD to branch on the remote.
func (g *Git) Push(ctx context.Context, dir, branch string, force bool) error {
	args := []string{"push", "--quiet", "origin", "HEAD:refs/heads/" + branch}
	if force {
		args = append(args, "--force")
	}
	_, err := g.run(ctx, dir, args...)
	return err
}
