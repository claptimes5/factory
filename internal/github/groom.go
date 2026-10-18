package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// OpenIssue is an open issue as backlog grooming reads it.
type OpenIssue struct {
	Issue
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time // the last activity: a comment, an edit, a label
	LinkedPRs int       // open PRs (merge requests on GitLab) that would close it
}

// Comment is one comment on an issue.
type Comment struct {
	Author string // as REST names it: an app's bot ends in [bot]
	Body   string
}

const issueFields = `number title body state createdAt updatedAt
        author { __typename login }
        labels(first: 50) { nodes { name } }
        closedByPullRequestsReferences(first: 1) { totalCount }`

const openIssuesQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 100, after: $after, orderBy: {field: CREATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { ` + issueFields + ` }
    }
  }
}`

const issueDetailQuery = `query($owner: String!, $name: String!, $n: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $n) { ` + issueFields + ` }
  }
}`

type gqlIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    *struct {
		Type  string `json:"__typename"`
		Login string `json:"login"`
	} `json:"author"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Closing struct {
		TotalCount int `json:"totalCount"`
	} `json:"closedByPullRequestsReferences"`
}

func (n gqlIssue) open() OpenIssue {
	o := OpenIssue{Issue: Issue{Number: n.Number, Title: n.Title, Body: n.Body, State: strings.ToLower(n.State)}, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, LinkedPRs: n.Closing.TotalCount}
	if o.State == "" {
		o.State = "open"
	}
	if n.Author != nil {
		o.Author = n.Author.Login
		if n.Author.Type == "Bot" && !strings.HasSuffix(o.Author, "[bot]") {
			o.Author += "[bot]" // as REST names it
		}
	}
	for _, l := range n.Labels.Nodes {
		o.Labels = append(o.Labels, l.Name)
	}
	return o
}

type gqlErrors []struct {
	Message string `json:"message"`
}

// OpenIssues lists up to max open issues, newest first, with the open PRs
// that would close each.
func (c *Client) OpenIssues(ctx context.Context, max int) ([]OpenIssue, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var out []OpenIssue
	var after any
	for len(out) < max {
		var r struct {
			Data struct {
				Repository *struct {
					Issues struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []gqlIssue `json:"nodes"`
					} `json:"issues"`
				} `json:"repository"`
			} `json:"data"`
			Errors gqlErrors `json:"errors"`
		}
		if err := c.graphql(ctx, openIssuesQuery, map[string]any{"owner": owner, "name": name, "after": after}, &r); err != nil {
			return nil, err
		}
		if len(r.Errors) > 0 {
			return nil, fmt.Errorf("github: %s", r.Errors[0].Message)
		}
		if r.Data.Repository == nil {
			return nil, &HTTPError{Status: 404, Body: "repository " + c.Repo + " not found"}
		}
		is := r.Data.Repository.Issues
		for _, n := range is.Nodes {
			out = append(out, n.open())
		}
		if !is.PageInfo.HasNextPage || is.PageInfo.EndCursor == "" {
			break
		}
		after = is.PageInfo.EndCursor
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// IssueDetail reads one issue as OpenIssues does, open or closed.
func (c *Client) IssueDetail(ctx context.Context, n int) (*OpenIssue, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var r struct {
		Data struct {
			Repository *struct {
				Issue *gqlIssue `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
		Errors gqlErrors `json:"errors"`
	}
	if err := c.graphql(ctx, issueDetailQuery, map[string]any{"owner": owner, "name": name, "n": n}, &r); err != nil {
		return nil, err
	}
	if r.Data.Repository == nil || r.Data.Repository.Issue == nil {
		if len(r.Errors) > 0 && !strings.Contains(strings.ToLower(r.Errors[0].Message), "could not resolve") {
			return nil, fmt.Errorf("github: %s", r.Errors[0].Message)
		}
		return nil, &HTTPError{Status: 404, Body: fmt.Sprintf("issue %d not found", n)}
	}
	o := r.Data.Repository.Issue.open()
	return &o, nil
}

// IssueComments lists an issue's comments, oldest first, up to 300.
func (c *Client) IssueComments(ctx context.Context, n int) ([]Comment, error) {
	var out []Comment
	for page := 1; page <= 3; page++ {
		var raw []struct {
			Body string `json:"body"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/comments?%s", c.Repo, n, q.Encode()), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			cm := Comment{Body: r.Body}
			if r.User != nil {
				cm.Author = r.User.Login
			}
			out = append(out, cm)
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}
