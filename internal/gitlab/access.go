package gitlab

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/claptimes5/factory/internal/github"
)

// MissingAccess says what the factory does on the project that the
// token may not. Each is asked with an empty request, which GitLab
// refuses with a 403 when the token lacks the permission (a fine-grained
// token without it, or too low a role) and answers with a 400 (a field
// is missing) or a 404 when it has it, so nothing is created.
func (c *Client) MissingAccess(ctx context.Context) ([]github.Denied, error) {
	probes := []struct{ key, what, method, path string }{
		{"pr", "open merge requests", "POST", "/merge_requests"},
		{"pr-update", "update merge requests", "PUT", "/merge_requests/0"},
		{"issue", "open issues", "POST", "/issues"},
		{"issue-update", "label and close issues", "PUT", "/issues/0"},
		{"comment", "comment on issues and merge requests", "POST", "/issues/0/notes"},
		{"label", "create labels", "POST", "/labels"},
	}
	var out []github.Denied
	for _, p := range probes {
		err := c.do(ctx, p.method, c.project()+p.path, map[string]any{}, nil)
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			var why struct {
				Description string `json:"error_description"`
				Message     string `json:"message"`
			}
			_ = json.Unmarshal([]byte(he.Body), &why)
			says := why.Description
			if says == "" {
				says = why.Message
			}
			out = append(out, github.Denied{Key: p.key, What: p.what, Says: says})
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return out, err
		}
	}
	return out, nil
}
