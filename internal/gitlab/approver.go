package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

// developer is GitLab's Developer access level, the least that may
// approve work.
const developer = 30

// maxEventPages bounds the label events read; an issue with more can't
// be approved here (the newest event might be past them).
const maxEventPages = 50

// Approver says who last added label to issue n and whether that starts
// work: Developer access or above (inherited from a group counts), or the
// factory's own user. The text is read after the label, then the issue's
// history is checked for an edit since the label: an edit in between
// counts too, so what's returned is what they approved unless Edited.
func (c *Client) Approver(ctx context.Context, n int, label string) (github.Approval, error) {
	type event struct {
		Action    string    `json:"action"` // add or remove
		CreatedAt time.Time `json:"created_at"`
		Label     *struct {
			Name string `json:"name"`
		} `json:"label"`
		User *struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	var a github.Approval
	id := 0
	var at time.Time
	for page := 1; ; page++ {
		if page > maxEventPages {
			return a, fmt.Errorf("gitlab: issue %d has over %d label events; too many to tell who added %s", n, maxEventPages*100, label)
		}
		var evs []event
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/resource_label_events?%s", c.project(), n, q.Encode()), nil, &evs); err != nil {
			return a, err
		}
		for _, e := range evs { // oldest first: the last one wins
			if e.Action == "add" && e.Label != nil && e.User != nil && strings.EqualFold(e.Label.Name, label) {
				a.By, id, at = e.User.Username, e.User.ID, e.CreatedAt
			}
		}
		if len(evs) < 100 {
			break
		}
	}
	is, err := c.GetIssue(ctx, n)
	if err != nil {
		return a, err
	}
	a.Issue = *is
	if a.By == "" {
		return a, nil
	}
	if a.Edited, err = c.editedSince(ctx, n, at); err != nil {
		return a, err
	}
	self, err := c.self(ctx)
	if err != nil {
		return a, err // unsure who the factory is: decide on a later poll
	}
	if strings.EqualFold(self, a.By) {
		a.OK = true
		return a, nil
	}
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err = c.do(ctx, "GET", fmt.Sprintf("%s/members/all/%d", c.project(), id), nil, &m)
	if github.IsNotFound(err) {
		return a, nil // not a member
	}
	if err != nil {
		return a, err
	}
	a.OK = m.AccessLevel >= developer
	return a, nil
}

// editedSince reports whether the issue's title or description changed
// after t, from the system notes GitLab writes for each change.
func (c *Client) editedSince(ctx context.Context, n int, t time.Time) (bool, error) {
	type note struct {
		Body      string    `json:"body"`
		System    bool      `json:"system"`
		CreatedAt time.Time `json:"created_at"`
	}
	for page := 1; page <= maxEventPages; page++ {
		var notes []note
		q := url.Values{"sort": {"desc"}, "order_by": {"created_at"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/notes?%s", c.project(), n, q.Encode()), nil, &notes); err != nil {
			return false, err
		}
		for _, x := range notes { // newest first
			if !x.CreatedAt.After(t) {
				return false, nil
			}
			if x.System && (strings.HasPrefix(x.Body, "changed the description") || strings.HasPrefix(x.Body, "changed title")) {
				return true, nil
			}
		}
		if len(notes) < 100 {
			return false, nil
		}
	}
	return false, fmt.Errorf("gitlab: issue %d has too many notes to check for edits", n)
}

func (c *Client) self(ctx context.Context) (string, error) {
	c.mu.Lock()
	known := c.selfName
	c.mu.Unlock()
	if known != "" {
		return known, nil
	}
	u, err := c.User(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.selfName = u
	c.mu.Unlock()
	return u, nil
}
