package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

type rawDetail struct {
	rawIssue
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Author    *struct {
		Username string `json:"username"`
	} `json:"author"`
	MRs int `json:"merge_requests_count"`
}

func (r rawDetail) open() github.OpenIssue {
	o := github.OpenIssue{Issue: r.issue(), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LinkedPRs: r.MRs}
	if r.Author != nil {
		o.Author = r.Author.Username
	}
	return o
}

// OpenIssues lists up to max open issues, newest first, with how many
// merge requests are related to each.
func (c *Client) OpenIssues(ctx context.Context, max int) ([]github.OpenIssue, error) {
	var out []github.OpenIssue
	for page := 1; len(out) < max && page <= 10; page++ {
		var raw []rawDetail
		q := url.Values{"state": {"opened"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}, "order_by": {"created_at"}, "sort": {"desc"}}
		if err := c.do(ctx, "GET", c.project()+"/issues?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			out = append(out, r.open())
		}
		if len(raw) < 100 {
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// IssueDetail reads one issue as OpenIssues does, open or closed.
func (c *Client) IssueDetail(ctx context.Context, n int) (*github.OpenIssue, error) {
	var r rawDetail
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d", c.project(), n), nil, &r); err != nil {
		return nil, err
	}
	o := r.open()
	return &o, nil
}

// IssueComments lists an issue's comments, oldest first, up to 300.
// System notes (label changes and the like) are left out.
func (c *Client) IssueComments(ctx context.Context, n int) ([]github.Comment, error) {
	var out []github.Comment
	for page := 1; page <= 3; page++ {
		var raw []struct {
			Body   string `json:"body"`
			System bool   `json:"system"`
			Author *struct {
				Username string `json:"username"`
			} `json:"author"`
		}
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}, "sort": {"asc"}, "order_by": {"created_at"}}
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/notes?%s", c.project(), n, q.Encode()), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.System {
				continue
			}
			cm := github.Comment{Body: r.Body}
			if r.Author != nil {
				cm.Author = r.Author.Username
			}
			out = append(out, cm)
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}
