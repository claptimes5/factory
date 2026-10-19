// Package github is a small REST client for the handful of GitHub calls the
// daemon makes. Authentication is a GitHub App installation token minted
// per use (scoped to one repo, expiring in an hour) or, for development, a
// static token.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issue is the subset of an issue the factory reads.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"-"`
	IsPR   bool     `json:"-"`
	// State is "open" or "closed"; StateReason says why a closed one was
	// closed ("completed", "not_planned").
	State       string `json:"-"`
	StateReason string `json:"-"`
	// Author is who opened it (a login; an app's ends in [bot]).
	Author string `json:"-"`
}

// PR is the subset of a pull request the factory reads.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Draft  bool   `json:"draft"`
	Head   struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	// MergeCommit is the commit the merge made on the base branch (a merge
	// or squash commit), once merged.
	MergeCommit string `json:"merge_commit_sha"`
	// SquashCommit is GitLab's squash commit when the merge made a merge
	// commit too; "" otherwise.
	SquashCommit string `json:"-"`
	// MergedAt is when it merged, once merged.
	MergedAt *time.Time `json:"merged_at"`
}

// TokenSource returns a valid token for the repository.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed token (a PAT or GITHUB_TOKEN), for development.
type StaticToken string

func (s StaticToken) Token(context.Context) (string, error) {
	if s == "" {
		return "", errors.New("github: empty token")
	}
	return string(s), nil
}

// Client calls the GitHub REST API for one repository.
type Client struct {
	Repo    string // owner/name
	BaseURL string // default https://api.github.com
	Tokens  TokenSource
	HTTP    *http.Client
	// WebURL is GitHub's site, for links to files; default https://github.com.
	WebURL string

	selfOnce  sync.Mutex
	previewMu sync.Mutex // one rewrite of the preview branch at a time
	selfLogin string     // who the factory acts as, once looked up
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://api.github.com"
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
	// Limited: GitHub refused for a rate limit, which passes, rather than
	// for a lack of access. It answers those with a 403 as well as a 429.
	Limited bool
}

// RateLimit says whether the request was refused for a rate limit.
func (e *HTTPError) RateLimit() bool { return e.Limited }

func (e *HTTPError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

// StatusCode is the response's HTTP status.
func (e *HTTPError) StatusCode() int { return e.Status }

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	// Any client's error that carries its HTTP status (GitLab's too).
	var sc interface{ StatusCode() int }
	return errors.As(err, &sc) && sc.StatusCode() == 404
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	return doJSON(ctx, c.http(), method, c.base()+path, "token "+tok, in, out)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP()
}

func defaultHTTP() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

var timeNow = time.Now

func doJSON(ctx context.Context, hc *http.Client, method, u, auth string, in, out any) error {
	_, err := doJSONHeader(ctx, hc, method, u, auth, in, out)
	return err
}

// doJSONHeader is doJSON that also returns the response's headers.
func doJSONHeader(ctx context.Context, hc *http.Client, method, u, auth string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		body := strings.TrimSpace(string(b))
		limited := resp.StatusCode == 429 || resp.StatusCode == 403 &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(body), "rate limit"))
		return nil, &HTTPError{Status: resp.StatusCode, Body: body, Limited: limited}
	}
	if out != nil && len(b) > 0 {
		return resp.Header, json.Unmarshal(b, out)
	}
	return resp.Header, nil
}

type rawIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	PullRequest any    `json:"pull_request"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (r rawIssue) issue() Issue {
	is := Issue{Number: r.Number, Title: r.Title, Body: r.Body, IsPR: r.PullRequest != nil, State: r.State, StateReason: r.StateReason, Author: r.User.Login}
	for _, l := range r.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

// ListIssues returns open issues (not PRs) carrying a label.
func (c *Client) ListIssues(ctx context.Context, label string) ([]Issue, error) {
	return c.ListIssuesIn(ctx, label, "open")
}

// ListIssuesIn is ListIssues for a state: "open", "closed" or "all".
func (c *Client) ListIssuesIn(ctx context.Context, label, state string) ([]Issue, error) {
	var out []Issue
	for page := 1; page <= 10; page++ {
		var raw []rawIssue
		q := url.Values{"state": {state}, "labels": {label}, "per_page": {"100"}, "page": {fmt.Sprint(page)}, "sort": {"created"}, "direction": {"asc"}}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/issues?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if is := r.issue(); !is.IsPR {
				out = append(out, is)
			}
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

// GetIssue fetches one issue.
func (c *Client) GetIssue(ctx context.Context, n int) (*Issue, error) {
	var r rawIssue
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, n), nil, &r); err != nil {
		return nil, err
	}
	is := r.issue()
	return &is, nil
}

// CreateIssue opens an issue and returns its number.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (int, error) {
	var r rawIssue
	in := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		in["labels"] = labels // GitHub refuses null
	}
	err := c.do(ctx, "POST", "/repos/"+c.Repo+"/issues", in, &r)
	return r.Number, err
}

// AddLabels adds labels to an issue or PR.
func (c *Client) AddLabels(ctx context.Context, n int, labels ...string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/labels", c.Repo, n), map[string]any{"labels": labels}, nil)
}

// LabelPR adds labels to a pull request.
func (c *Client) LabelPR(ctx context.Context, n int, labels ...string) error {
	return c.AddLabels(ctx, n, labels...)
}

// UnlabelPR is RemoveLabel: a PR is an issue to GitHub's labels API.
func (c *Client) UnlabelPR(ctx context.Context, n int, label string) error {
	return c.RemoveLabel(ctx, n, label)
}

// RemoveLabel removes a label; a label that is not there is not an error.
func (c *Client) RemoveLabel(ctx context.Context, n int, label string) error {
	err := c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/issues/%d/labels/%s", c.Repo, n, url.PathEscape(label)), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Comment posts a comment on an issue or PR.
func (c *Client) Comment(ctx context.Context, n int, body string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, n), map[string]any{"body": body}, nil)
}

// EnsureLabel creates a label unless one with that name exists, and reports
// whether it created it. Existing labels are left as they are.
func (c *Client) EnsureLabel(ctx context.Context, name, color, description string) (bool, error) {
	err := c.do(ctx, "GET", "/repos/"+c.Repo+"/labels/"+url.PathEscape(name), nil, nil)
	if err == nil {
		return false, nil
	}
	if !IsNotFound(err) {
		return false, err
	}
	err = c.do(ctx, "POST", "/repos/"+c.Repo+"/labels", map[string]any{"name": name, "color": color, "description": description}, nil)
	return err == nil, err
}

// ListLabels returns the names of the repo's labels.
func (c *Client) ListLabels(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page <= 10; page++ {
		var raw []struct {
			Name string `json:"name"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/labels?per_page=100&page=%d", c.Repo, page), nil, &raw); err != nil {
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

// BranchSHA returns a branch's head commit, or "" if it does not exist.
func (c *Client) BranchSHA(ctx context.Context, branch string) (string, error) {
	var r struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.do(ctx, "GET", "/repos/"+c.Repo+"/branches/"+url.PathEscape(branch), nil, &r)
	if IsNotFound(err) {
		return "", nil
	}
	return r.Commit.SHA, err
}

// FindPR returns the open PR whose head is branch, or nil.
func (c *Client) FindPR(ctx context.Context, branch string) (*PR, error) {
	owner, _, _ := strings.Cut(c.Repo, "/")
	var prs []PR
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}}
	if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// GetPR fetches a pull request.
func (c *Client) GetPR(ctx context.Context, n int) (*PR, error) {
	var pr PR
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), nil, &pr)
	return &pr, err
}

// CheckRun is one CI check on a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|neutral|cancelled|skipped|timed_out|action_required
	URL        string `json:"html_url"`
	// ID orders a check's runs: a rerun has a higher one.
	ID int64 `json:"id"`
	// Output is what the check said about itself (GitHub's title and
	// summary), when CheckHistory lists it. It is CI output: untrusted.
	Output string `json:"-"`
	// Key and Group are set by CheckHistory. Key names the check the same
	// way on every commit: its workflow and name, or its app's and name,
	// so two workflows' "build" jobs are two checks. Group is the run of
	// CI it belongs to on this commit (a check suite, a GitLab pipeline):
	// a rerun stays in its group, so fail-then-pass within one is a flake.
	Key   string `json:"-"`
	Group string `json:"-"`
}

// CheckRuns lists the CI checks on a commit.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", c.Repo, url.PathEscape(sha)), nil, &out)
	return out.CheckRuns, err
}

// CheckHistory lists every run of every check on a commit, reruns
// included, oldest first by ID. Workflow names come from the Actions API
// when the token may read it (actions: read); without it, an Actions
// check is keyed by its job name alone.
func (c *Client) CheckHistory(ctx context.Context, sha string) ([]CheckRun, error) {
	var out []CheckRun
	for page := 1; ; page++ {
		var r struct {
			CheckRuns []struct {
				CheckRun
				Output struct {
					Title   string `json:"title"`
					Summary string `json:"summary"`
				} `json:"output"`
				App struct {
					Slug string `json:"slug"`
				} `json:"app"`
				Suite struct {
					ID int64 `json:"id"`
				} `json:"check_suite"`
			} `json:"check_runs"`
		}
		q := url.Values{"filter": {"all"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?%s", c.Repo, url.PathEscape(sha), q.Encode()), nil, &r); err != nil {
			return nil, err
		}
		for _, cr := range r.CheckRuns {
			run := cr.CheckRun
			run.Output = strings.TrimSpace(strings.TrimSpace(cr.Output.Title) + "\n" + strings.TrimSpace(cr.Output.Summary))
			run.Group = fmt.Sprintf("suite %d", cr.Suite.ID)
			run.Key = run.Name
			if cr.App.Slug != "" && cr.App.Slug != "github-actions" {
				run.Key = cr.App.Slug + ": " + run.Name
			}
			out = append(out, run)
		}
		if len(r.CheckRuns) < 100 {
			break
		}
		if page == 10 {
			return nil, fmt.Errorf("github: %s has over 1000 check runs; not reading a partial list", sha)
		}
	}
	// Name Actions checks for their workflow, so a "build" job in two
	// workflows is two checks. Best effort: without the names it carries on.
	var runs struct {
		Runs []struct {
			Name  string `json:"name"`
			Suite int64  `json:"check_suite_id"`
		} `json:"workflow_runs"`
	}
	q := url.Values{"head_sha": {sha}, "per_page": {"100"}}
	if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/actions/runs?"+q.Encode(), nil, &runs); err == nil {
		wf := map[string]string{}
		for _, r := range runs.Runs {
			wf[fmt.Sprintf("suite %d", r.Suite)] = r.Name
		}
		for i := range out {
			if n := wf[out[i].Group]; n != "" && out[i].Key == out[i].Name {
				out[i].Key = n + " / " + out[i].Name
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CheckLog is the start of what a failed check said: on GitHub, the
// output CheckHistory already read, so no further call.
func (c *Client) CheckLog(_ context.Context, run CheckRun) (string, error) {
	return run.Output, nil
}

// Self is who the factory acts as: its app's bot login, or the token's user.
func (c *Client) Self(ctx context.Context) (string, error) { return c.self(ctx) }

// CloseNotPlanned closes an issue as not planned.
func (c *Client) CloseNotPlanned(ctx context.Context, n int) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, n), map[string]any{"state": "closed", "state_reason": "not_planned"}, nil)
}

// CloseDuplicate closes an issue as a duplicate.
func (c *Client) CloseDuplicate(ctx context.Context, n int) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, n), map[string]any{"state": "closed", "state_reason": "duplicate"}, nil)
}

// CloseIssue closes an issue as completed.
func (c *Client) CloseIssue(ctx context.Context, n int) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, n), map[string]any{"state": "closed", "state_reason": "completed"}, nil)
}

// CreatePR opens a pull request.
func (c *Client) CreatePR(ctx context.Context, title, body, head, base string, draft bool) (*PR, error) {
	var pr PR
	err := c.do(ctx, "POST", "/repos/"+c.Repo+"/pulls", map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": draft}, &pr)
	return &pr, err
}

// UpdatePRBody replaces a PR's description.
func (c *Client) UpdatePRBody(ctx context.Context, n int, body string) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), map[string]any{"body": body}, nil)
}

// AppTokens mints GitHub App installation tokens scoped to one repository
// and caches each until shortly before it expires.
type AppTokens struct {
	AppID string
	// InstallationID is the app's installation on the repo's owner. Left
	// empty, it is looked up on first use.
	InstallationID string
	Key            *rsa.PrivateKey
	Repo           string // owner/name: the token is scoped to this repo only
	BaseURL        string
	HTTP           *http.Client

	mu       sync.Mutex
	token    string
	expires  time.Time
	lookedUp bool // InstallationID came from RepoInstallation
}

// LoadAppKey reads a PEM private key (PKCS#1 or PKCS#8).
func LoadAppKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("github app key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not RSA")
	}
	return rk, nil
}

// JWT returns the app's signed JWT (RS256), valid for nine minutes.
func (a *AppTokens) JWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	hdr := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.AppID})
	unsigned := hdr + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

func (a *AppTokens) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	owner, name, _ := strings.Cut(a.Repo, "/")
	if a.InstallationID == "" {
		id, err := a.RepoInstallation(ctx, a.Repo)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("github: the app is not installed on %s; install it on %s and select %s", a.Repo, owner, a.Repo)
		}
		a.InstallationID, a.lookedUp = id, true
	}
	var r struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	perms := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read",
		"checks": "read"} // checks: the dashboard's CI panel
	u := base + "/app/installations/" + a.InstallationID + "/access_tokens"
	err = doJSON(ctx, hc, "POST", u, "Bearer "+jwt, map[string]any{"repositories": []string{name}, "permissions": perms}, &r)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusUnprocessableEntity {
		// GitHub refuses the whole token when the app lacks a permission we ask
		// for. Apps set up before Checks was needed still work, minus the CI panel.
		delete(perms, "checks")
		err = doJSON(ctx, hc, "POST", u, "Bearer "+jwt, map[string]any{"repositories": []string{name}, "permissions": perms}, &r)
	}
	if errors.As(err, &he) && he.Status == http.StatusNotFound && a.lookedUp {
		// The app was uninstalled since the lookup; look again next time.
		a.InstallationID, a.lookedUp = "", false
	}
	if err != nil {
		return "", err
	}
	a.token, a.expires = r.Token, r.ExpiresAt
	return a.token, nil
}

// ListDir names the entries of a directory on the default branch ("" is
// the root). Directories end in "/".
func (c *Client) ListDir(ctx context.Context, dir string) ([]string, error) {
	var list []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/contents/"+escapePath(dir), nil, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, e := range list {
		if e.Type == "dir" {
			e.Name += "/"
		}
		names = append(names, e.Name)
	}
	return names, nil
}

// ReadFile returns a file's contents on the default branch.
func (c *Client) ReadFile(ctx context.Context, path string) (string, error) {
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/contents/"+escapePath(path), nil, &f); err != nil {
		return "", err
	}
	if f.Encoding != "base64" {
		return "", fmt.Errorf("github: %s: unexpected encoding %q", path, f.Encoding)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	return string(b), err
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// Private says whether the repository is private. An internal repo (an
// enterprise's, readable by all its members) reports private: true, so
// only visibility "private" counts; a reply without a visibility is an
// error, and the caller assumes public.
func (c *Client) Private(ctx context.Context) (bool, error) {
	var r struct {
		Visibility string `json:"visibility"`
	}
	if err := c.do(ctx, "GET", "/repos/"+c.Repo, nil, &r); err != nil {
		return false, err
	}
	switch r.Visibility {
	case "private":
		return true, nil
	case "internal", "public":
		return false, nil
	}
	return false, fmt.Errorf("github: %s: no visibility in the reply", c.Repo)
}

// DefaultBranch returns the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := c.do(ctx, "GET", "/repos/"+c.Repo, nil, &r)
	return r.DefaultBranch, err
}

// CreatedApp is what GitHub returns once an app made from a manifest exists.
type CreatedApp struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
	PEM     string `json:"pem"`
	Owner   struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ConvertManifest exchanges the code GitHub sends back after you create an
// app from a manifest for the app's ID and private key. It needs no token.
func ConvertManifest(ctx context.Context, base, code string) (*CreatedApp, error) {
	if base == "" {
		base = "https://api.github.com"
	}
	var app CreatedApp
	err := doJSON(ctx, &http.Client{Timeout: 30 * time.Second}, "POST", base+"/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &app)
	return &app, err
}

// RepoInstallation returns the ID of the app's installation that covers
// the repo owner/name, or "" if none does.
func (a *AppTokens) RepoInstallation(ctx context.Context, repo string) (string, error) {
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	owner, name, _ := strings.Cut(repo, "/")
	var in struct {
		ID int64 `json:"id"`
	}
	err = doJSON(ctx, &http.Client{Timeout: 30 * time.Second}, "GET", base+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/installation", "Bearer "+jwt, nil, &in)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(in.ID, 10), nil
}

// FindInstallation returns the ID of the app's installation on the given
// account, or "" if the app is not installed there yet.
func (a *AppTokens) FindInstallation(ctx context.Context, account string) (string, error) {
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var list []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := doJSON(ctx, &http.Client{Timeout: 30 * time.Second}, "GET", base+"/app/installations?per_page=100", "Bearer "+jwt, nil, &list); err != nil {
		return "", err
	}
	for _, in := range list {
		if strings.EqualFold(in.Account.Login, account) {
			return strconv.FormatInt(in.ID, 10), nil
		}
	}
	return "", nil
}

// IsOrg reports whether a GitHub account is an organization.
func IsOrg(ctx context.Context, base, login string) (bool, error) {
	if base == "" {
		base = "https://api.github.com"
	}
	var u struct {
		Type string `json:"type"`
	}
	err := doJSON(ctx, &http.Client{Timeout: 30 * time.Second}, "GET", base+"/users/"+url.PathEscape(login), "", nil, &u)
	return u.Type == "Organization", err
}
