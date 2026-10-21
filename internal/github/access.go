package github

import (
	"context"
	"errors"
)

// Denied is something the factory does on a repo that its token may not.
type Denied struct {
	Key  string `json:"key"`  // pr, pr-update, issue, issue-update, comment, label
	What string `json:"what"` // in words: "open pull requests"
	Says string `json:"says"` // what the host said, if it said why
}

// refused says whether err is the host turning the token away, rather
// than a rate limit, a missing thing or a bad request.
func refused(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && !he.Limited && (he.Status == 401 || he.Status == 403)
}

// MissingAccess says what the factory does on the repo that the token
// may not. Each is asked with an empty request, which GitHub refuses
// with a 403 when the token lacks the permission and answers with a 422
// (a field is missing) when it has it, so nothing is created.
func (c *Client) MissingAccess(ctx context.Context) ([]Denied, error) {
	probes := []struct{ key, what, path string }{
		{"pr", "open pull requests", "/pulls"},
		{"issue", "open issues", "/issues"},
		{"label", "create labels", "/labels"},
	}
	var out []Denied
	for _, p := range probes {
		err := c.do(ctx, "POST", "/repos/"+c.Repo+p.path, map[string]any{}, nil)
		if refused(err) {
			out = append(out, Denied{Key: p.key, What: p.what, Says: err.(*HTTPError).Body})
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return out, err
		}
	}
	return out, nil
}
