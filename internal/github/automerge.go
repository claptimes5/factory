package github

import (
	"context"
	"fmt"
	"strings"
)

// AutoMerge marks PR n ready and has GitHub merge it once its required
// checks and reviews pass. GitHub refuses if the head has moved from sha;
// it does not cancel for a later push by someone with write access, so the
// factory cancels before it pushes again (CancelAutoMerge). method is
// merge, squash or rebase. If nothing is left to wait for, it merges now,
// but only when sha has CI checks and all of them passed: GitHub would
// otherwise merge a repo with no required checks untested.
func (c *Client) AutoMerge(ctx context.Context, n int, sha, method string) error {
	var pr struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), nil, &pr); err != nil {
		return err
	}
	if pr.Draft {
		if err := c.mutate(ctx, `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`,
			map[string]any{"id": pr.NodeID}); err != nil {
			return fmt.Errorf("mark ready: %w", err)
		}
	}
	err := c.mutate(ctx, `mutation($id: ID!, $method: PullRequestMergeMethod!, $sha: GitObjectID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method, expectedHeadOid: $sha}) { clientMutationId }
}`, map[string]any{"id": pr.NodeID, "method": strings.ToUpper(method), "sha": sha})
	if err != nil && strings.Contains(err.Error(), "clean status") {
		// Nothing left to wait for, so GitHub won't queue it.
		runs, cerr := c.CheckRuns(ctx, sha)
		if cerr != nil {
			return cerr
		}
		if !allPassed(runs) {
			return fmt.Errorf("nothing requires CI to pass on this branch and its checks haven't all passed; make your CI checks required in branch protection")
		}
		return c.do(ctx, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", c.Repo, n), map[string]any{"sha": sha, "merge_method": method}, nil)
	}
	return err
}

// allPassed says whether there are checks and every one succeeded.
func allPassed(runs []CheckRun) bool {
	for _, r := range runs {
		if r.Status != "completed" || (r.Conclusion != "success" && r.Conclusion != "neutral" && r.Conclusion != "skipped") {
			return false
		}
	}
	return len(runs) > 0
}

// CancelAutoMerge turns auto-merge off on PR n, if it's on.
func (c *Client) CancelAutoMerge(ctx context.Context, n int) error {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var r struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ID               string    `json:"id"`
					AutoMergeRequest *struct{} `json:"autoMergeRequest"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, `query($owner: String!, $name: String!, $n: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $n) { id autoMergeRequest { enabledAt } } }
}`, map[string]any{"owner": owner, "name": name, "n": n}, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("github: %s", r.Errors[0].Message)
	}
	if r.Data.Repository == nil || r.Data.Repository.PullRequest == nil {
		return fmt.Errorf("github: PR %d not found", n)
	}
	pr := r.Data.Repository.PullRequest
	if pr.AutoMergeRequest == nil {
		return nil // off already: merged, closed, or turned off by hand
	}
	return c.mutate(ctx, `mutation($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`,
		map[string]any{"id": pr.ID})
}

// mutate runs a GraphQL mutation and turns its errors into one.
func (c *Client) mutate(ctx context.Context, query string, vars map[string]any) error {
	var r struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, query, vars, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("github: %s", r.Errors[0].Message)
	}
	return nil
}
