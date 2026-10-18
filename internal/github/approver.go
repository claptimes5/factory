package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Approval is who last added a label to an issue and whether that starts
// work. Anyone can open an issue on a public repository, and an issue
// template can add labels for them, so a label alone proves nothing.
type Approval struct {
	By string // who last added the label; "" when no one did
	// OK: By may approve work (write access, or the factory itself).
	OK bool
	// Edited: the title or text changed after By added the label, so what
	// they approved isn't what the issue says now.
	Edited bool
	// Issue is the title and body read with the label, as they are now.
	Issue Issue
}

const approvalQuery = `query($owner: String!, $name: String!, $n: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $n) {
      title
      body
      lastEditedAt
      timelineItems(last: 100, itemTypes: [LABELED_EVENT, RENAMED_TITLE_EVENT]) {
        nodes {
          __typename
          ... on LabeledEvent { createdAt label { name } actor { __typename login } }
          ... on RenamedTitleEvent { createdAt }
        }
      }
    }
  }
}`

// Approver reads, in one query so they belong together, the issue's text
// and who last added label (the newest events, however long the issue's
// history). Bots other than the factory never approve: a workflow that
// labels issues can be started by the issue's author.
func (c *Client) Approver(ctx context.Context, n int, label string) (Approval, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var r struct {
		Data struct {
			Repository *struct {
				Issue *struct {
					Title         string     `json:"title"`
					Body          string     `json:"body"`
					LastEditedAt  *time.Time `json:"lastEditedAt"`
					TimelineItems struct {
						Nodes []struct {
							Type      string    `json:"__typename"`
							CreatedAt time.Time `json:"createdAt"`
							Label     *struct {
								Name string `json:"name"`
							} `json:"label"`
							Actor *struct {
								Type  string `json:"__typename"`
								Login string `json:"login"`
							} `json:"actor"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, approvalQuery, map[string]any{"owner": owner, "name": name, "n": n}, &r); err != nil {
		return Approval{}, err
	}
	if len(r.Errors) > 0 {
		return Approval{}, fmt.Errorf("github: %s", r.Errors[0].Message)
	}
	if r.Data.Repository == nil || r.Data.Repository.Issue == nil {
		return Approval{}, &HTTPError{Status: 404, Body: fmt.Sprintf("issue %d not found", n)}
	}
	is := r.Data.Repository.Issue
	a := Approval{Issue: Issue{Number: n, Title: is.Title, Body: is.Body}}
	var at, renamed time.Time
	for _, e := range is.TimelineItems.Nodes { // oldest first: the last one wins
		switch {
		case e.Type == "LabeledEvent" && e.Label != nil && strings.EqualFold(e.Label.Name, label):
			a.By, at = "", e.CreatedAt
			if e.Actor != nil {
				a.By = e.Actor.Login
				if e.Actor.Type == "Bot" && !strings.HasSuffix(a.By, "[bot]") {
					// As REST names it, and never the same as a user's login.
					a.By += "[bot]"
				}
			}
		case e.Type == "RenamedTitleEvent":
			renamed = e.CreatedAt
		}
	}
	if at.IsZero() || a.By == "" {
		return a, nil
	}
	// Timestamps are to the second: an edit in the label's second counts.
	a.Edited = !renamed.IsZero() && !renamed.Before(at) || is.LastEditedAt != nil && !is.LastEditedAt.Before(at)
	self, err := c.self(ctx)
	if err != nil {
		return a, err // unsure who the factory is: decide on a later poll
	}
	if strings.EqualFold(self, a.By) {
		a.OK = true
		return a, nil
	}
	var perm struct {
		Permission string `json:"permission"` // admin, write, read or none; maintain reads as write
	}
	err = c.do(ctx, "GET", fmt.Sprintf("/repos/%s/collaborators/%s/permission", c.Repo, url.PathEscape(a.By)), nil, &perm)
	if IsNotFound(err) {
		return a, nil // not a collaborator, or a bot that isn't the factory
	}
	if err != nil {
		return a, err
	}
	a.OK = perm.Permission == "admin" || perm.Permission == "write"
	return a, nil
}

// graphql posts a query to the GraphQL API beside the REST base: on GitHub
// Enterprise that's /api/graphql next to /api/v3.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := c.base() + "/graphql"
	if b, ok := strings.CutSuffix(c.base(), "/api/v3"); ok {
		u = b + "/api/graphql"
	}
	return doJSON(ctx, c.http(), "POST", u, "bearer "+tok, map[string]any{"query": query, "variables": vars}, out)
}

// self is the login the factory acts as: the app's bot, or the token's user.
func (c *Client) self(ctx context.Context) (string, error) {
	c.selfOnce.Lock()
	defer c.selfOnce.Unlock()
	if c.selfLogin != "" {
		return c.selfLogin, nil
	}
	if app, ok := c.Tokens.(*AppTokens); ok {
		slug, err := app.Slug(ctx)
		if err != nil {
			return "", err
		}
		if slug == "" {
			return "", errors.New("github: the app has no slug")
		}
		c.selfLogin = slug + "[bot]"
		return c.selfLogin, nil
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("github: the token has no user")
	}
	c.selfLogin = u.Login
	return c.selfLogin, nil
}

// Slug is the app's name in URLs; its bot user is <slug>[bot].
func (a *AppTokens) Slug(ctx context.Context) (string, error) {
	jwt, err := a.JWT(timeNow())
	if err != nil {
		return "", err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	hc := a.HTTP
	if hc == nil {
		hc = defaultHTTP()
	}
	var r struct {
		Slug string `json:"slug"`
	}
	if err := doJSON(ctx, hc, "GET", base+"/app", "Bearer "+jwt, nil, &r); err != nil {
		return "", err
	}
	return r.Slug, nil
}
