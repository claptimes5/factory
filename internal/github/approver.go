package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Approver says who last added label to issue n, and whether they may
// approve work: write access to the repository (admin, maintain or
// write), or the factory's own identity. Anyone can open an issue on a
// public repository, and an issue template can add labels for them, so a
// label alone proves nothing. who is "" when no one added the label.
func (c *Client) Approver(ctx context.Context, n int, label string) (who string, ok bool, err error) {
	type event struct {
		Event string `json:"event"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
	}
	for page := 1; page <= 10; page++ {
		var evs []event
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/events?%s", c.Repo, n, q.Encode()), nil, &evs); err != nil {
			return "", false, err
		}
		for _, e := range evs { // oldest first: the last one wins
			if e.Event == "labeled" && strings.EqualFold(e.Label.Name, label) {
				who = e.Actor.Login
			}
		}
		if len(evs) < 100 {
			break
		}
	}
	if who == "" {
		return "", false, nil
	}
	if self, err := c.self(ctx); err == nil && strings.EqualFold(self, who) {
		return who, true, nil
	}
	var perm struct {
		Permission string `json:"permission"` // admin, write, read or none; maintain reads as write
	}
	err = c.do(ctx, "GET", fmt.Sprintf("/repos/%s/collaborators/%s/permission", c.Repo, url.PathEscape(who)), nil, &perm)
	if IsNotFound(err) {
		return who, false, nil // not a collaborator, or a bot that isn't the factory
	}
	if err != nil {
		return who, false, err
	}
	return who, perm.Permission == "admin" || perm.Permission == "write", nil
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
		c.selfLogin = slug + "[bot]"
		return c.selfLogin, nil
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, "GET", "/user", nil, &u); err != nil {
		return "", err
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
