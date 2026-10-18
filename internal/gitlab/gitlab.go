// Package gitlab is a GitLab client with the same methods as the GitHub
// one, so the pipeline, setup and the readiness check work on a GitLab
// project the way they do on a GitHub repo. Merge requests stand in for
// pull requests; numbers are the project-scoped iids GitLab shows.
package gitlab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

// Client calls the GitLab REST API (v4) for one project.
type Client struct {
	Project string // group/project or group/subgroup/project
	Host    string // gitlab.com or a self-hosted host
	BaseURL string // default https://<Host>/api/v4; tests point it at a fake
	Tokens  github.TokenSource
	HTTP    *http.Client

	mu  sync.Mutex
	ref string // the default branch, once looked up
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string   { return fmt.Sprintf("gitlab: HTTP %d: %s", e.Status, e.Body) }
func (e *HTTPError) StatusCode() int { return e.Status }

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://" + c.Host + "/api/v4"
}

// project is the API path of the project, its full path escaped as one
// segment.
func (c *Client) project() string { return "/projects/" + url.PathEscape(c.Project) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	b, err := c.raw(ctx, method, path, in)
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) raw(ctx context.Context, method, path string, in any) ([]byte, error) {
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("PRIVATE-TOKEN", tok)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return b, nil
}

type rawIssue struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"` // opened, closed
	Labels      []string `json:"labels"`
}

func (r rawIssue) issue() github.Issue {
	is := github.Issue{Number: r.IID, Title: r.Title, Body: r.Description, Labels: r.Labels, State: "open"}
	if r.State == "closed" {
		// GitLab doesn't say why, so StateReason stays empty.
		is.State = "closed"
	}
	return is
}

// ListIssues returns open issues carrying a label.
func (c *Client) ListIssues(ctx context.Context, label string) ([]github.Issue, error) {
	return c.ListIssuesIn(ctx, label, "open")
}

// ListIssuesIn is ListIssues for a state: "open", "closed" or "all".
func (c *Client) ListIssuesIn(ctx context.Context, label, state string) ([]github.Issue, error) {
	if state == "open" {
		state = "opened"
	}
	var out []github.Issue
	for page := 1; page <= 10; page++ {
		var raw []rawIssue
		q := url.Values{"state": {state}, "labels": {label}, "per_page": {"100"}, "page": {fmt.Sprint(page)}, "order_by": {"created_at"}, "sort": {"asc"}}
		if err := c.do(ctx, "GET", c.project()+"/issues?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			out = append(out, r.issue())
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

// GetIssue fetches one issue.
func (c *Client) GetIssue(ctx context.Context, n int) (*github.Issue, error) {
	var r rawIssue
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/issues/%d", c.project(), n), nil, &r); err != nil {
		return nil, err
	}
	is := r.issue()
	return &is, nil
}

// CreateIssue opens an issue and returns its number.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (int, error) {
	var r rawIssue
	err := c.do(ctx, "POST", c.project()+"/issues", map[string]any{"title": title, "description": body, "labels": strings.Join(labels, ",")}, &r)
	return r.IID, err
}

// AddLabels adds labels to an issue.
func (c *Client) AddLabels(ctx context.Context, n int, labels ...string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/issues/%d", c.project(), n), map[string]any{"add_labels": strings.Join(labels, ",")}, nil)
}

// LabelPR adds labels to a merge request.
func (c *Client) LabelPR(ctx context.Context, n int, labels ...string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), map[string]any{"add_labels": strings.Join(labels, ",")}, nil)
}

// RemoveLabel removes a label from an issue; one that isn't there is
// not an error.
func (c *Client) RemoveLabel(ctx context.Context, n int, label string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/issues/%d", c.project(), n), map[string]any{"remove_labels": label}, nil)
}

// Comment posts a comment on an issue.
func (c *Client) Comment(ctx context.Context, n int, body string) error {
	return c.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/notes", c.project(), n), map[string]any{"body": body}, nil)
}

// EnsureLabel creates a label unless the project (or a group above it)
// has one with that name, and reports whether it created it.
func (c *Client) EnsureLabel(ctx context.Context, name, color, description string) (bool, error) {
	err := c.do(ctx, "GET", c.project()+"/labels/"+url.PathEscape(name)+"?include_ancestor_groups=true", nil, nil)
	if err == nil {
		return false, nil
	}
	if !github.IsNotFound(err) {
		return false, err
	}
	err = c.do(ctx, "POST", c.project()+"/labels", map[string]any{"name": name, "color": "#" + color, "description": description}, nil)
	return err == nil, err
}

// BranchSHA returns a branch's head commit, or "" if it does not exist.
func (c *Client) BranchSHA(ctx context.Context, branch string) (string, error) {
	var r struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := c.do(ctx, "GET", c.project()+"/repository/branches/"+url.PathEscape(branch), nil, &r)
	if github.IsNotFound(err) {
		return "", nil
	}
	return r.Commit.ID, err
}

type rawMR struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
	State  string `json:"state"` // opened, closed, locked, merged
	Draft  bool   `json:"draft"`
	WIP    bool   `json:"work_in_progress"` // before GitLab 14
	SHA    string `json:"sha"`
}

func (r rawMR) pr() *github.PR {
	pr := &github.PR{Number: r.IID, URL: r.WebURL, State: "open", Draft: r.Draft || r.WIP}
	switch r.State {
	case "merged":
		pr.State, pr.Merged = "closed", true
	case "closed":
		pr.State = "closed"
	}
	pr.Head.SHA = r.SHA
	return pr
}

// FindPR returns the open merge request from branch, or nil.
func (c *Client) FindPR(ctx context.Context, branch string) (*github.PR, error) {
	var mrs []rawMR
	q := url.Values{"source_branch": {branch}, "state": {"opened"}}
	if err := c.do(ctx, "GET", c.project()+"/merge_requests?"+q.Encode(), nil, &mrs); err != nil {
		return nil, err
	}
	if len(mrs) == 0 {
		return nil, nil
	}
	return mrs[0].pr(), nil
}

// GetPR fetches a merge request.
func (c *Client) GetPR(ctx context.Context, n int) (*github.PR, error) {
	var r rawMR
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), nil, &r); err != nil {
		return &github.PR{}, err
	}
	return r.pr(), nil
}

// CreatePR opens a merge request. A draft's title starts with "Draft:",
// which every GitLab version reads as a draft.
func (c *Client) CreatePR(ctx context.Context, title, body, head, base string, draft bool) (*github.PR, error) {
	if draft {
		title = "Draft: " + title
	}
	var r rawMR
	err := c.do(ctx, "POST", c.project()+"/merge_requests", map[string]any{"title": title, "description": body, "source_branch": head, "target_branch": base}, &r)
	return r.pr(), err
}

// UpdatePRBody replaces a merge request's description.
func (c *Client) UpdatePRBody(ctx context.Context, n int, body string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), map[string]any{"description": body}, nil)
}

// CheckRuns lists the CI jobs on a commit as GitHub-style checks.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]github.CheckRun, error) {
	var st []struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		TargetURL string `json:"target_url"`
	}
	if err := c.do(ctx, "GET", c.project()+"/repository/commits/"+url.PathEscape(sha)+"/statuses?per_page=100", nil, &st); err != nil {
		return nil, err
	}
	out := make([]github.CheckRun, 0, len(st))
	for _, s := range st {
		cr := github.CheckRun{Name: s.Name, URL: s.TargetURL, Status: "completed"}
		switch s.Status {
		case "success":
			cr.Conclusion = "success"
		case "failed":
			cr.Conclusion = "failure"
		case "canceled":
			cr.Conclusion = "cancelled"
		case "skipped":
			cr.Conclusion = "skipped"
		case "manual":
			cr.Conclusion = "action_required"
		case "running":
			cr.Status = "in_progress"
		default: // created, pending, preparing, scheduled, waiting_for_resource
			cr.Status = "queued"
		}
		out = append(out, cr)
	}
	return out, nil
}

// DefaultBranch returns the project's default branch.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := c.do(ctx, "GET", c.project(), nil, &r)
	return r.DefaultBranch, err
}

func (c *Client) defaultRef(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ref != "" {
		return c.ref, nil
	}
	ref, err := c.DefaultBranch(ctx)
	if err != nil {
		return "", err
	}
	if ref == "" {
		return "", &HTTPError{Status: 404, Body: "the project has no default branch (is it empty?)"}
	}
	c.ref = ref
	return ref, nil
}

// ListDir names the entries of a directory on the default branch ("" is
// the root). Directories end in "/".
func (c *Client) ListDir(ctx context.Context, dir string) ([]string, error) {
	ref, err := c.defaultRef(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for page := 1; page <= 10; page++ {
		var list []struct {
			Name string `json:"name"`
			Type string `json:"type"` // tree, blob, commit (a submodule)
		}
		q := url.Values{"ref": {ref}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if dir != "" {
			q.Set("path", strings.TrimSuffix(dir, "/"))
		}
		if err := c.do(ctx, "GET", c.project()+"/repository/tree?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, e := range list {
			if e.Type == "tree" {
				e.Name += "/"
			}
			names = append(names, e.Name)
		}
		if len(list) < 100 {
			break
		}
	}
	return names, nil
}

// ReadFile returns a file's contents on the default branch.
func (c *Client) ReadFile(ctx context.Context, path string) (string, error) {
	ref, err := c.defaultRef(ctx)
	if err != nil {
		return "", err
	}
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, "GET", c.project()+"/repository/files/"+url.PathEscape(path)+"?ref="+url.QueryEscape(ref), nil, &f); err != nil {
		return "", err
	}
	if f.Encoding != "base64" {
		return "", fmt.Errorf("gitlab: %s: unexpected encoding %q", path, f.Encoding)
	}
	b, err := base64.StdEncoding.DecodeString(f.Content)
	return string(b), err
}

// User returns the token's username, to check the token works.
func (c *Client) User(ctx context.Context) (string, error) {
	var u struct {
		Username string `json:"username"`
	}
	err := c.do(ctx, "GET", "/user", nil, &u)
	return u.Username, err
}
