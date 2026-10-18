package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Commit is one commit on a branch, as the revert watch reads it.
type Commit struct {
	SHA     string
	Message string
	At      time.Time // when it was committed
}

// MergedPR is a pull request merged into a branch.
type MergedPR struct {
	Number      int
	Title       string
	Body        string
	HeadRef     string
	MergeCommit string
	MergedAt    time.Time
}

// BranchCommits lists the commits on branch committed after since,
// newest first, at most max of them; more says there were more to read.
func (c *Client) BranchCommits(ctx context.Context, branch string, since time.Time, max int) ([]Commit, bool, error) {
	var out []Commit
	for page := 1; ; page++ {
		var raw []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message   string `json:"message"`
				Committer struct {
					Date time.Time `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
		}
		q := url.Values{"sha": {branch}, "since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/commits?"+q.Encode(), nil, &raw); err != nil {
			return nil, false, err
		}
		for _, r := range raw {
			if len(out) == max {
				return out, true, nil
			}
			out = append(out, Commit{SHA: r.SHA, Message: r.Commit.Message, At: r.Commit.Committer.Date})
		}
		if len(raw) < 100 {
			return out, false, nil
		}
	}
}

// MergedPRs lists the PRs merged into branch since, most recently
// updated first, at most max of them.
func (c *Client) MergedPRs(ctx context.Context, branch string, since time.Time, max int) ([]MergedPR, error) {
	var out []MergedPR
	for page := 1; len(out) < max; page++ {
		var raw []struct {
			Number    int        `json:"number"`
			Title     string     `json:"title"`
			Body      string     `json:"body"`
			MergedAt  *time.Time `json:"merged_at"`
			UpdatedAt time.Time  `json:"updated_at"`
			Merge     string     `json:"merge_commit_sha"`
			Head      struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		q := url.Values{"state": {"closed"}, "base": {branch}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"50"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/pulls?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.UpdatedAt.Before(since) {
				return out, nil // sorted by update: the rest are older
			}
			if r.MergedAt == nil || r.MergedAt.Before(since) || len(out) == max {
				continue
			}
			out = append(out, MergedPR{Number: r.Number, Title: r.Title, Body: r.Body, HeadRef: r.Head.Ref, MergeCommit: r.Merge, MergedAt: *r.MergedAt})
		}
		if len(raw) < 50 {
			break
		}
	}
	return out, nil
}

// MergeQueue says whether branch has a merge queue.
func (c *Client) MergeQueue(ctx context.Context, branch string) (bool, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var r struct {
		Data struct {
			Repository *struct {
				MergeQueue *struct {
					ID string `json:"id"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, `query($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) { mergeQueue(branch: $branch) { id } }
}`, map[string]any{"owner": owner, "name": name, "branch": branch}, &r); err != nil {
		return false, err
	}
	if len(r.Errors) > 0 {
		return false, fmt.Errorf("github: %s", r.Errors[0].Message)
	}
	return r.Data.Repository != nil && r.Data.Repository.MergeQueue != nil, nil
}

// QueueMerge marks PR n ready and has GitHub add it to the branch's merge
// queue once its checks pass, if its head is still sha. The queue tests
// it merged with the base branch and the PRs ahead of it, and merges it
// with the queue's own method, so method is unused. If nothing is left
// to wait for, it joins the queue now, once its checks have all passed.
func (c *Client) QueueMerge(ctx context.Context, n int, sha, _ string) error {
	id, err := c.markReady(ctx, n)
	if err != nil {
		return err
	}
	err = c.mutate(ctx, `mutation($id: ID!, $sha: GitObjectID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, expectedHeadOid: $sha}) { clientMutationId }
}`, map[string]any{"id": id, "sha": sha})
	if err != nil && strings.Contains(err.Error(), "clean status") {
		// Nothing left to wait for, so GitHub won't wait: join the queue
		// only once its checks have all passed, as AutoMerge merges.
		runs, cerr := c.CheckRuns(ctx, sha)
		if cerr != nil {
			return cerr
		}
		if !allPassed(runs) {
			return fmt.Errorf("nothing requires CI to pass on this branch and its checks haven't all passed; make your CI checks required in branch protection")
		}
		return c.mutate(ctx, `mutation($id: ID!, $sha: GitObjectID!) {
  enqueuePullRequest(input: {pullRequestId: $id, expectedHeadOid: $sha}) { clientMutationId }
}`, map[string]any{"id": id, "sha": sha})
	}
	return err
}

// markReady takes PR n out of draft and returns its node ID.
func (c *Client) markReady(ctx context.Context, n int) (string, error) {
	var pr struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), nil, &pr); err != nil {
		return "", err
	}
	if pr.Draft {
		if err := c.mutate(ctx, `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`,
			map[string]any{"id": pr.NodeID}); err != nil {
			return "", fmt.Errorf("mark ready: %w", err)
		}
	}
	return pr.NodeID, nil
}

// FirstParent is the first parent of commit sha: for a merge or squash
// commit on a branch, the branch's head before it. "" for a root commit.
func (c *Client) FirstParent(ctx context.Context, sha string) (string, error) {
	var r struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s", c.Repo, url.PathEscape(sha)), nil, &r); err != nil {
		return "", err
	}
	if len(r.Parents) == 0 {
		return "", nil
	}
	return r.Parents[0].SHA, nil
}
