package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

// BranchCommits lists the commits on branch committed after since,
// newest first, at most max of them; more says there were more to read.
func (c *Client) BranchCommits(ctx context.Context, branch string, since time.Time, max int) ([]github.Commit, bool, error) {
	var out []github.Commit
	for page := 1; ; page++ {
		var raw []struct {
			ID      string    `json:"id"`
			Message string    `json:"message"`
			At      time.Time `json:"committed_date"`
		}
		q := url.Values{"ref_name": {branch}, "since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", c.project()+"/repository/commits?"+q.Encode(), nil, &raw); err != nil {
			return nil, false, err
		}
		for _, r := range raw {
			if len(out) == max {
				return out, true, nil
			}
			out = append(out, github.Commit{SHA: r.ID, Message: r.Message, At: r.At})
		}
		if len(raw) < 100 {
			return out, false, nil
		}
	}
}

// MergedPRs lists the merge requests merged into branch since, most
// recently updated first, at most max of them.
func (c *Client) MergedPRs(ctx context.Context, branch string, since time.Time, max int) ([]github.MergedPR, error) {
	var out []github.MergedPR
	for page := 1; len(out) < max; page++ {
		var raw []struct {
			IID      int        `json:"iid"`
			Title    string     `json:"title"`
			Desc     string     `json:"description"`
			Source   string     `json:"source_branch"`
			Merge    string     `json:"merge_commit_sha"`
			Squash   string     `json:"squash_commit_sha"`
			MergedAt *time.Time `json:"merged_at"`
		}
		q := url.Values{"state": {"merged"}, "target_branch": {branch}, "updated_after": {since.UTC().Format(time.RFC3339)},
			"order_by": {"updated_at"}, "sort": {"desc"}, "per_page": {"50"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", c.project()+"/merge_requests?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.MergedAt == nil || r.MergedAt.Before(since) || len(out) == max {
				continue
			}
			sha := r.Merge
			if sha == "" {
				sha = r.Squash
			}
			out = append(out, github.MergedPR{Number: r.IID, Title: r.Title, Body: r.Desc, HeadRef: r.Source, MergeCommit: sha, MergedAt: *r.MergedAt})
		}
		if len(raw) < 50 {
			break
		}
	}
	return out, nil
}

// MergeQueue says whether the project's merge requests are tested merged
// with their target branch before they merge: with merged results
// pipelines on, each pipeline runs on the request merged with the target,
// and with merge trains on too (a Premium feature), the train retests it
// with the requests ahead of it just before it merges. Either way the
// factory needn't bring the branch up to date itself, and it couldn't
// follow its CI if it did: those pipelines don't run on the branch's
// commit. Trains without merged results pipelines don't run at all.
func (c *Client) MergeQueue(ctx context.Context, _ string) (bool, error) {
	var p struct {
		MergedResults bool `json:"merge_pipelines_enabled"`
	}
	err := c.do(ctx, "GET", c.project(), nil, &p)
	return p.MergedResults, err
}

// QueueMerge sets merge request n to merge, which on a project with
// merge trains adds it to the train once its pipeline passes. Its
// pipeline there is a merged results one, run on a commit merging sha
// into the target branch, which AutoMerge accepts for sha.
func (c *Client) QueueMerge(ctx context.Context, n int, sha, method string) error {
	return c.AutoMerge(ctx, n, sha, method)
}

// FirstParent is the first parent of commit sha: for a merge or squash
// commit on a branch, the branch's head before it. "" for a root commit.
func (c *Client) FirstParent(ctx context.Context, sha string) (string, error) {
	ps, err := c.parents(ctx, sha)
	if err != nil || len(ps) == 0 {
		return "", err
	}
	return ps[0], nil
}

// parents are commit sha's parents, first first.
func (c *Client) parents(ctx context.Context, sha string) ([]string, error) {
	var r struct {
		Parents []string `json:"parent_ids"`
	}
	err := c.do(ctx, "GET", c.project()+"/repository/commits/"+url.PathEscape(sha), nil, &r)
	return r.Parents, err
}
