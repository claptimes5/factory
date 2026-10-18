package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/claptimes5/factory/internal/github"
)

// developer is GitLab's Developer access level, the least that may
// approve work.
const developer = 30

// Approver says who last added label to issue n, and whether they may
// approve work: Developer access or above (inherited from a group
// counts), or the factory's own user. who is "" when no one added it.
func (c *Client) Approver(ctx context.Context, n int, label string) (who string, ok bool, err error) {
	type event struct {
		Action string `json:"action"` // add or remove
		Label  *struct {
			Name string `json:"name"`
		} `json:"label"`
		User *struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	id := 0
	for page := 1; page <= 10; page++ {
		var evs []event
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/resource_label_events?%s", c.project(), n, q.Encode()), nil, &evs); err != nil {
			return "", false, err
		}
		for _, e := range evs { // oldest first: the last one wins
			if e.Action == "add" && e.Label != nil && e.User != nil && strings.EqualFold(e.Label.Name, label) {
				who, id = e.User.Username, e.User.ID
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
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err = c.do(ctx, "GET", fmt.Sprintf("%s/members/all/%d", c.project(), id), nil, &m)
	if github.IsNotFound(err) {
		return who, false, nil // not a member
	}
	if err != nil {
		return who, false, err
	}
	return who, m.AccessLevel >= developer, nil
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
