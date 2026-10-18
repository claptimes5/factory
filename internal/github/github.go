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
}

// PR is the subset of a pull request the factory reads.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Draft  bool   `json:"draft"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
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
}

func (e *HTTPError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == 404
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
	return &http.Client{Timeout: 30 * time.Second}
}

func doJSON(ctx context.Context, hc *http.Client, method, u, auth string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

type rawIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	PullRequest any    `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r rawIssue) issue() Issue {
	is := Issue{Number: r.Number, Title: r.Title, Body: r.Body, IsPR: r.PullRequest != nil}
	for _, l := range r.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

// ListIssues returns open issues (not PRs) carrying a label.
func (c *Client) ListIssues(ctx context.Context, label string) ([]Issue, error) {
	var out []Issue
	for page := 1; page <= 10; page++ {
		var raw []rawIssue
		q := url.Values{"state": {"open"}, "labels": {label}, "per_page": {"100"}, "page": {fmt.Sprint(page)}, "sort": {"created"}, "direction": {"asc"}}
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
	err := c.do(ctx, "POST", "/repos/"+c.Repo+"/issues", map[string]any{"title": title, "body": body, "labels": labels}, &r)
	return r.Number, err
}

// AddLabels adds labels to an issue or PR.
func (c *Client) AddLabels(ctx context.Context, n int, labels ...string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/labels", c.Repo, n), map[string]any{"labels": labels}, nil)
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
}

// CheckRuns lists the CI checks on a commit.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", c.Repo, url.PathEscape(sha)), nil, &out)
	return out.CheckRuns, err
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

	mu      sync.Mutex
	token   string
	expires time.Time
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
		id, err := a.FindInstallation(ctx, owner)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("github: the app is not installed on %s; install it and add %s", owner, a.Repo)
		}
		a.InstallationID = id
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
	if err != nil {
		return "", err
	}
	a.token, a.expires = r.Token, r.ExpiresAt
	return a.token, nil
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
