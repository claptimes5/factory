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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
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

	mu       sync.Mutex
	ref      string // the default branch, once looked up
	selfName string // the token's user, once looked up
	logsOpen *bool  // whether anyone who can read issues can read job logs, once looked up
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string   { return fmt.Sprintf("gitlab: HTTP %d: %s", e.Status, e.Body) }
func (e *HTTPError) StatusCode() int { return e.Status }

// RateLimit says whether GitLab refused for a rate limit.
func (e *HTTPError) RateLimit() bool { return e.Status == 429 }

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
	resp, err := c.send(ctx, method, path, in)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// send makes a request with in as its JSON body; a response it returns is
// a 2xx, its body for the caller to read and close.
func (c *Client) send(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.sendBody(ctx, method, path, body, ct)
}

// upload is a request with a body of its own type, such as a form, and
// its response's body.
func (c *Client) upload(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	resp, err := c.sendBody(ctx, method, path, body, contentType)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// sendBody is send with the body as given.
func (c *Client) sendBody(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		// Authorization, unlike PRIVATE-TOKEN, is dropped if a redirect
		// leaves the host. GitLab takes every kind of token this way.
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

type rawIssue struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"` // opened, closed
	Labels      []string `json:"labels"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (r rawIssue) issue() github.Issue {
	is := github.Issue{Number: r.IID, Title: r.Title, Body: r.Description, Labels: r.Labels, State: "open", Author: r.Author.Username}
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
	in := map[string]any{"title": title, "description": body}
	if len(labels) > 0 {
		in["labels"] = strings.Join(labels, ",")
	}
	err := c.do(ctx, "POST", c.project()+"/issues", in, &r)
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

// UnlabelPR removes a label from a merge request; one that isn't there is
// not an error.
func (c *Client) UnlabelPR(ctx context.Context, n int, label string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), map[string]any{"remove_labels": label}, nil)
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

// ListLabels returns the names of the labels the project can use, its
// groups' included.
func (c *Client) ListLabels(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page <= 10; page++ {
		var raw []struct {
			Name string `json:"name"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/labels?include_ancestor_groups=true&per_page=100&page=%d", c.project(), page), nil, &raw); err != nil {
			return nil, err
		}
		for _, l := range raw {
			out = append(out, l.Name)
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

// DeleteLabel deletes a project label, which takes it off every issue
// and merge request that has it. A label already gone is not an error; a
// group's label can only be deleted in the group.
func (c *Client) DeleteLabel(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", c.project()+"/labels/"+url.PathEscape(name), nil, nil)
	if github.IsNotFound(err) {
		return nil
	}
	return err
}

// CountOpenLabeled counts the open issues and merge requests with a
// label, up to 1000 of each.
func (c *Client) CountOpenLabeled(ctx context.Context, label string) (int, error) {
	n := 0
	for _, kind := range []string{"issues", "merge_requests"} {
		for page := 1; page <= 10; page++ {
			var raw []struct {
				IID int `json:"iid"`
			}
			q := url.Values{"state": {"opened"}, "labels": {label}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
			if err := c.do(ctx, "GET", c.project()+"/"+kind+"?"+q.Encode(), nil, &raw); err != nil {
				return 0, err
			}
			n += len(raw)
			if len(raw) < 100 {
				break
			}
		}
	}
	return n, nil
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
	Title  string `json:"title"`
	Desc   string `json:"description"`
	Merge  string `json:"merge_commit_sha"`
	Squash string `json:"squash_commit_sha"`
	// MergedAt is when it merged, once merged.
	MergedAt *time.Time `json:"merged_at"`
	Refs     struct {
		Base string `json:"base_sha"`
	} `json:"diff_refs"`
	// They differ for a merge request from a fork.
	SourceProject int `json:"source_project_id"`
	TargetProject int `json:"target_project_id"`
}

func (r rawMR) pr() *github.PR {
	pr := &github.PR{Number: r.IID, URL: r.WebURL, State: "open", Draft: r.Draft || r.WIP}
	switch r.State {
	case "merged":
		pr.State, pr.Merged = "closed", true
	case "closed":
		pr.State = "closed"
	}
	pr.Head.SHA, pr.Base.SHA, pr.Title, pr.Body, pr.MergedAt = r.SHA, r.Refs.Base, r.Title, r.Desc, r.MergedAt
	// A fast-forward merge makes no commit of its own; a squash makes one.
	pr.MergeCommit = r.Merge
	if pr.MergeCommit == "" {
		pr.MergeCommit = r.Squash
	} else if r.Squash != r.Merge {
		pr.SquashCommit = r.Squash // a revert may name either
	}
	return pr
}

// FindPR returns the open merge request from branch in the project
// itself, or nil. One from a fork's branch of the same name isn't the
// factory's.
func (c *Client) FindPR(ctx context.Context, branch string) (*github.PR, error) {
	var mrs []rawMR
	q := url.Values{"source_branch": {branch}, "state": {"opened"}}
	if err := c.do(ctx, "GET", c.project()+"/merge_requests?"+q.Encode(), nil, &mrs); err != nil {
		return nil, err
	}
	for _, mr := range mrs {
		if mr.SourceProject == mr.TargetProject {
			return mr.pr(), nil
		}
	}
	return nil, nil
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
	type status struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Status       string `json:"status"`
		TargetURL    string `json:"target_url"`
		AllowFailure bool   `json:"allow_failure"`
	}
	var st []status
	for page := 1; page <= 10; page++ {
		var batch []status
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", c.project()+"/repository/commits/"+url.PathEscape(sha)+"/statuses?"+q.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		st = append(st, batch...)
		if len(batch) < 100 {
			break
		}
	}
	out := make([]github.CheckRun, 0, len(st))
	for _, s := range st {
		out = append(out, checkRun(s.ID, s.Name, s.Status, s.TargetURL, s.AllowFailure, ""))
	}
	return out, nil
}

// infraFailures are why a job can fail that say nothing about the code:
// the runner, not the job, broke.
var infraFailures = []string{"runner_system_failure", "stuck_or_timeout_failure", "scheduler_failure", "data_integrity_failure", "api_failure", "runner_unsupported", "unknown_failure"}

func checkRun(id int64, name, status, url string, allowFailure bool, reason string) github.CheckRun {
	cr := github.CheckRun{ID: id, Name: name, URL: url, Status: "completed"}
	switch status {
	case "success":
		cr.Conclusion = "success"
	case "failed":
		cr.Conclusion = "failure"
		if allowFailure {
			cr.Conclusion = "neutral"
		} else if slices.Contains(infraFailures, reason) {
			cr.Conclusion = "cancelled" // no result: rerun it, don't fix the code
		}
	case "canceled":
		cr.Conclusion = "cancelled"
	case "skipped":
		cr.Conclusion = "skipped"
	case "manual":
		cr.Conclusion = "action_required"
	case "running", "canceling":
		cr.Status = "in_progress"
	default: // created, pending, preparing, scheduled, waiting_for_resource
		cr.Status = "queued"
	}
	return cr
}

// CheckHistory lists every job of every pipeline on a commit, retries
// included, oldest first by ID. A job's group is its pipeline; its key is
// its name, with the pipeline's source unless that's a push, so a
// scheduled pipeline's "test" isn't the push pipeline's.
func (c *Client) CheckHistory(ctx context.Context, sha string) ([]github.CheckRun, error) {
	var pipes []struct {
		ID     int64  `json:"id"`
		Source string `json:"source"`
	}
	q := url.Values{"sha": {sha}, "per_page": {"100"}}
	if err := c.do(ctx, "GET", c.project()+"/pipelines?"+q.Encode(), nil, &pipes); err != nil {
		return nil, err
	}
	var out []github.CheckRun
	for _, pl := range pipes {
		for page := 1; ; page++ {
			var jobs []struct {
				ID            int64  `json:"id"`
				Name          string `json:"name"`
				Status        string `json:"status"`
				WebURL        string `json:"web_url"`
				AllowFailure  bool   `json:"allow_failure"`
				FailureReason string `json:"failure_reason"`
			}
			q := url.Values{"include_retried": {"true"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
			if err := c.do(ctx, "GET", fmt.Sprintf("%s/pipelines/%d/jobs?%s", c.project(), pl.ID, q.Encode()), nil, &jobs); err != nil {
				return nil, err
			}
			for _, j := range jobs {
				cr := checkRun(j.ID, j.Name, j.Status, j.WebURL, j.AllowFailure, j.FailureReason)
				cr.Key, cr.Group = j.Name, fmt.Sprintf("pipeline %d", pl.ID)
				if pl.Source != "" && pl.Source != "push" {
					cr.Key += " (" + pl.Source + ")"
				}
				out = append(out, cr)
			}
			if len(jobs) < 100 {
				break
			}
			if page == 10 {
				return nil, fmt.Errorf("gitlab: pipeline %d has over 1000 jobs; not reading a partial list", pl.ID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// logTail is how much of the end of a job's log CheckLog reads.
const logTail = 64 << 10

// CheckLog is the end of a CI job's log, where the failure usually is,
// read to its end but kept to the last 64 KiB. It's "" when the project's
// issues are public but its job logs aren't: quoting one in an issue would
// publish it.
func (c *Client) CheckLog(ctx context.Context, run github.CheckRun) (string, error) {
	open, err := c.logsReadable(ctx)
	if err != nil || !open {
		return "", err
	}
	resp, err := c.send(ctx, "GET", fmt.Sprintf("%s/jobs/%d/trace", c.project(), run.ID), nil)
	if github.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	t := &tailBuffer{max: logTail}
	if _, err := io.Copy(t, io.LimitReader(resp.Body, 256<<20)); err != nil {
		return "", err
	}
	return cleanTrace(strings.ToValidUTF8(string(t.bytes()), "")), nil
}

// logsReadable says whether everyone who can read the project's issues
// can read its job logs too: a private project, or public job logs.
func (c *Client) logsReadable(ctx context.Context) (bool, error) {
	c.mu.Lock()
	known := c.logsOpen
	c.mu.Unlock()
	if known != nil {
		return *known, nil
	}
	var p struct {
		Visibility string `json:"visibility"`
		PublicJobs bool   `json:"public_jobs"`
	}
	if err := c.do(ctx, "GET", c.project(), nil, &p); err != nil {
		return false, err
	}
	open := p.Visibility == "private" || p.PublicJobs
	c.mu.Lock()
	c.logsOpen = &open
	c.mu.Unlock()
	return open, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) bytes() []byte { return t.buf }

var (
	ansi    = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	section = regexp.MustCompile(`section_(start|end):[0-9]+:[A-Za-z0-9_.-]+(\[[^\]]*\])?\r?`)
)

// cleanTrace drops a job log's colors and section markers, and keeps the
// last line written over each carriage return.
func cleanTrace(s string) string {
	s = section.ReplaceAllString(ansi.ReplaceAllString(s, ""), "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndex(strings.TrimRight(l, "\r"), "\r"); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}

// Self is who the factory acts as: the token's user.
func (c *Client) Self(ctx context.Context) (string, error) { return c.self(ctx) }

// CloseIssue closes an issue.
func (c *Client) CloseIssue(ctx context.Context, n int) error {
	return c.do(ctx, "PUT", fmt.Sprintf("%s/issues/%d", c.project(), n), map[string]any{"state_event": "close"}, nil)
}

// Private says whether the project is private. Internal projects are
// readable by every account on the instance, so they aren't; a reply
// without a visibility is an error, so the caller assumes public.
func (c *Client) Private(ctx context.Context) (bool, error) {
	var r struct {
		Visibility string `json:"visibility"`
	}
	if err := c.do(ctx, "GET", c.project(), nil, &r); err != nil {
		return false, err
	}
	switch r.Visibility {
	case "private":
		return true, nil
	case "internal", "public":
		return false, nil
	}
	return false, fmt.Errorf("gitlab: %s: no visibility in the reply", c.Project)
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

// AutoMerge takes the merge request out of draft and has GitLab merge it
// when its pipeline succeeds, but only while its head is still sha.
// method squash squashes; merge and rebase follow the project's setting.
// With no pipeline, or one that finished without passing, GitLab would
// merge at once, so those are refused.
func (c *Client) AutoMerge(ctx context.Context, n int, sha, method string) error {
	var r struct {
		rawMR
		Pipeline *struct {
			SHA    string `json:"sha"`
			Ref    string `json:"ref"`
			Status string `json:"status"`
		} `json:"head_pipeline"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), nil, &r); err != nil {
		return err
	}
	tested := r.Pipeline != nil && r.Pipeline.SHA == sha
	if r.Pipeline != nil && !tested {
		ok, err := c.mergedResults(ctx, n, r.Pipeline.Ref, r.Pipeline.SHA, sha)
		if err != nil {
			return err
		}
		tested = ok
	}
	if !tested {
		return fmt.Errorf("no pipeline has run on the verified commit, so GitLab would merge it untested")
	}
	// A pipeline that already finished without passing would not hold the
	// merge back: GitLab merges at once unless "Pipelines must succeed" is on.
	switch r.Pipeline.Status {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "success":
	default:
		return fmt.Errorf("the verified commit's pipeline is %s", r.Pipeline.Status)
	}
	if t, ok := undraft(r.Title); ok {
		if err := c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d", c.project(), n), map[string]any{"title": t}, nil); err != nil {
			return fmt.Errorf("mark ready: %w", err)
		}
	}
	// auto_merge is GitLab 17.11's name; older versions read the other.
	return c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d/merge", c.project(), n), map[string]any{
		"sha": sha, "squash": method == "squash", "auto_merge": true, "merge_when_pipeline_succeeds": true,
	}, nil)
}

// mergedResults says whether a pipeline is a merged results (or merge
// train) pipeline for merge request n that tested sha: it ran at the
// request's merge or train ref, on a commit that merges sha into the
// target branch, so sha is one of that commit's parents.
func (c *Client) mergedResults(ctx context.Context, n int, ref, pipelineSHA, sha string) (bool, error) {
	if ref != fmt.Sprintf("refs/merge-requests/%d/merge", n) && ref != fmt.Sprintf("refs/merge-requests/%d/train", n) {
		return false, nil
	}
	parents, err := c.parents(ctx, pipelineSHA)
	if err != nil {
		return false, err
	}
	return slices.Contains(parents, sha), nil
}

// undraft strips a draft prefix from a merge request title.
func undraft(title string) (string, bool) {
	for _, p := range []string{"Draft:", "[Draft]", "(Draft)", "WIP:", "[WIP]"} {
		if len(title) >= len(p) && strings.EqualFold(title[:len(p)], p) {
			return strings.TrimSpace(title[len(p):]), true
		}
	}
	return title, false
}

// CancelAutoMerge turns off merge-when-pipeline-succeeds, if it's on.
func (c *Client) CancelAutoMerge(ctx context.Context, n int) error {
	err := c.do(ctx, "POST", fmt.Sprintf("%s/merge_requests/%d/cancel_merge_when_pipeline_succeeds", c.project(), n), nil, nil)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == 406 { // it wasn't on
		return nil
	}
	return err
}
