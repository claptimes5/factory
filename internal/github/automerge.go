package github

import (
	"context"
	"fmt"
	"strings"
)

// AutoMerge marks PR n ready and has GitHub merge it once its required
// checks and reviews pass, but only while its head is still sha: a push
// after the factory verified it cancels the merge. method is merge, squash
// or rebase. If the PR can already merge, it merges now.
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
		// Nothing left to wait for, so GitHub won't queue it: merge now.
		return c.do(ctx, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", c.Repo, n), map[string]any{"sha": sha, "merge_method": method}, nil)
	}
	return err
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
