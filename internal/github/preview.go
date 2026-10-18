package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// PreviewBranch holds a repo's PR screenshots, since GitHub has no API
// for a comment's images. It is an orphan branch of one commit with no
// parent, rewritten on each change, so an image removed from it leaves
// its history too; and it has no workflows, so updating it runs no CI.
const PreviewBranch = "factory/previews"

// ErrNotRemovable is a forge refusing, for good, to remove uploads: the
// factory stops trying and says so rather than claim they're gone.
var ErrNotRemovable = errors.New("the forge won't remove these uploads")

// File is one file to upload.
type File struct {
	Name string
	Data []byte
}

// Upload is an uploaded file: the link for a comment, and Ref to remove
// it by.
type Upload struct {
	Name, URL, Ref string
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// UploadPreview adds files to the preview branch under pr-<n>/<key>/.
func (c *Client) UploadPreview(ctx context.Context, n int, key string, files []File) ([]Upload, error) {
	c.previewMu.Lock()
	defer c.previewMu.Unlock()
	var add []treeEntry
	var out []Upload
	for _, f := range files {
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := c.do(ctx, "POST", "/repos/"+c.Repo+"/git/blobs", map[string]any{"content": base64.StdEncoding.EncodeToString(f.Data), "encoding": "base64"}, &blob); err != nil {
			return nil, err
		}
		p := fmt.Sprintf("pr-%d/%s/%s", n, key, f.Name)
		add = append(add, treeEntry{Path: p, Mode: "100644", Type: "blob", SHA: blob.SHA})
		out = append(out, Upload{Name: f.Name, Ref: p, URL: c.web() + "/" + c.Repo + "/blob/" + PreviewBranch + "/" + escapePath(p) + "?raw=true"})
	}
	return out, c.rewritePreviews(ctx, add, nil)
}

// RemovePreview takes uploads off the preview branch, by their Ref, and
// the branch itself once nothing is left on it.
func (c *Client) RemovePreview(ctx context.Context, _ int, refs []string) error {
	c.previewMu.Lock()
	defer c.previewMu.Unlock()
	drop := map[string]bool{}
	for _, r := range refs {
		drop[r] = true
	}
	return c.rewritePreviews(ctx, nil, drop)
}

func (c *Client) web() string {
	if c.WebURL != "" {
		return strings.TrimSuffix(c.WebURL, "/")
	}
	return "https://github.com"
}

// rewritePreviews makes the branch's one commit anew: what it holds,
// less drop, plus add.
func (c *Client) rewritePreviews(ctx context.Context, add []treeEntry, drop map[string]bool) error {
	refPath := "/repos/" + c.Repo + "/git/refs/heads/" + PreviewBranch
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.do(ctx, "GET", "/repos/"+c.Repo+"/git/ref/heads/"+PreviewBranch, nil, &ref)
	exists := err == nil
	if err != nil && !IsNotFound(err) {
		return err
	}
	var entries []treeEntry
	adding := map[string]bool{}
	for _, e := range add {
		adding[e.Path] = true
	}
	if exists {
		var commit struct {
			Message string `json:"message"`
			Tree    struct {
				SHA string `json:"sha"`
			} `json:"tree"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/git/commits/"+ref.Object.SHA, nil, &commit); err != nil {
			return err
		}
		// Only ever the factory's own commit: a branch of yours by that
		// name keeps its history.
		if len(commit.Parents) > 0 || strings.TrimSpace(commit.Message) != previewMessage {
			return fmt.Errorf("%w: github: %s holds commits that aren't the factory's screenshots, so it is left alone; rename that branch to let the factory use it", ErrNotRemovable, PreviewBranch)
		}
		var tree struct {
			Tree      []treeEntry `json:"tree"`
			Truncated bool        `json:"truncated"`
		}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/git/trees/"+commit.Tree.SHA+"?recursive=1", nil, &tree); err != nil {
			return err
		}
		if tree.Truncated {
			return fmt.Errorf("%w: github: %s holds too many files to rewrite; delete the branch to start it over", ErrNotRemovable, PreviewBranch)
		}
		for _, e := range tree.Tree {
			if e.Type == "blob" && !drop[e.Path] && !adding[e.Path] {
				entries = append(entries, e)
			}
		}
		if len(add) == 0 && len(entries) == len(tree.Tree)-countTrees(tree.Tree) {
			return nil // nothing to drop
		}
	}
	entries = append(entries, add...)
	if len(entries) == 0 {
		if !exists {
			return nil
		}
		err := c.do(ctx, "DELETE", refPath, nil, nil)
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, "POST", "/repos/"+c.Repo+"/git/trees", map[string]any{"tree": entries}, &tree); err != nil {
		return err
	}
	if err := c.do(ctx, "POST", "/repos/"+c.Repo+"/git/commits", map[string]any{
		"message": previewMessage, "tree": tree.SHA, "parents": []string{}}, &commit); err != nil {
		return err
	}
	if exists {
		return c.do(ctx, "PATCH", refPath, map[string]any{"sha": commit.SHA, "force": true}, nil)
	}
	err = c.do(ctx, "POST", "/repos/"+c.Repo+"/git/refs", map[string]any{"ref": "refs/heads/" + PreviewBranch, "sha": commit.SHA}, nil)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == 422 {
		// git can't have both factory and factory/previews.
		if sha, _ := c.BranchSHA(ctx, "factory"); sha != "" {
			return fmt.Errorf("github: a branch named factory blocks creating %s; rename it to let the factory keep screenshots", PreviewBranch)
		}
	}
	return err
}

// previewMessage is the preview branch's one commit's message, by which
// the factory knows the branch is its own.
const previewMessage = "factory: PR screenshots"

func countTrees(es []treeEntry) int {
	n := 0
	for _, e := range es {
		if e.Type != "blob" {
			n++
		}
	}
	return n
}

// UpsertPRComment edits comment id on PR n, or posts a new one when id is
// 0 or the comment is gone, and returns its id.
func (c *Client) UpsertPRComment(ctx context.Context, n int, id int64, body string) (int64, error) {
	if id != 0 {
		err := c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/comments/%d", c.Repo, id), map[string]any{"body": body}, nil)
		if !IsNotFound(err) {
			return id, err
		}
	}
	var r struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, n), map[string]any{"body": body}, &r)
	return r.ID, err
}
