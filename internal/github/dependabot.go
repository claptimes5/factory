package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// DependabotAlert is the part of a Dependabot security alert the factory
// reads.
type DependabotAlert struct {
	Number          int
	State           string // open, fixed, dismissed, auto_dismissed
	URL             string
	Ecosystem       string
	Package         string
	Manifest        string
	GHSA            string
	CVE             string
	Summary         string
	Severity        string // low, medium, high, critical
	VulnerableRange string
	Patched         string // the first patched version, or ""
}

type rawAlert struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	HTMLURL    string `json:"html_url"`
	Dependency struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
	} `json:"dependency"`
	Advisory struct {
		GHSA     string `json:"ghsa_id"`
		CVE      string `json:"cve_id"`
		Summary  string `json:"summary"`
		Severity string `json:"severity"`
	} `json:"security_advisory"`
	Vulnerability struct {
		Severity        string `json:"severity"`
		VulnerableRange string `json:"vulnerable_version_range"`
		FirstPatched    *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
}

func (r rawAlert) alert() DependabotAlert {
	a := DependabotAlert{Number: r.Number, State: r.State, URL: r.HTMLURL, Ecosystem: r.Dependency.Package.Ecosystem, Package: r.Dependency.Package.Name,
		Manifest: r.Dependency.ManifestPath, GHSA: r.Advisory.GHSA, CVE: r.Advisory.CVE, Summary: r.Advisory.Summary,
		Severity: r.Vulnerability.Severity, VulnerableRange: r.Vulnerability.VulnerableRange}
	if a.Severity == "" {
		a.Severity = r.Advisory.Severity
	}
	if r.Vulnerability.FirstPatched != nil {
		a.Patched = r.Vulnerability.FirstPatched.Identifier
	}
	return a
}

// DependabotAlerts lists the repo's open Dependabot alerts of these
// severities, newest first, following the API's pages: at most 1000.
func (c *Client) DependabotAlerts(ctx context.Context, severities []string) ([]DependabotAlert, error) {
	q := url.Values{"state": {"open"}, "per_page": {"100"}, "sort": {"created"}, "direction": {"desc"}}
	if len(severities) > 0 {
		q.Set("severity", strings.Join(severities, ","))
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	var out []DependabotAlert
	next := c.base() + "/repos/" + c.Repo + "/dependabot/alerts?" + q.Encode()
	for page := 0; page < 10 && next != ""; page++ {
		var raw []rawAlert
		h, err := doJSONHeader(ctx, c.http(), "GET", next, "token "+tok, nil, &raw)
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			out = append(out, r.alert())
		}
		next = nextLink(h.Get("Link"), c.base())
	}
	return out, nil
}

// nextLink is a Link header's rel="next" URL, if it's on base: the token
// goes with it.
func nextLink(link, base string) string {
	for _, part := range strings.Split(link, ",") {
		u, rel, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(rel, `rel="next"`) {
			continue
		}
		u = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(u), "<"), ">")
		if strings.HasPrefix(u, base+"/") {
			return u
		}
	}
	return ""
}

// DependabotAlert reads one alert.
func (c *Client) DependabotAlert(ctx context.Context, n int) (*DependabotAlert, error) {
	var r rawAlert
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/dependabot/alerts/%d", c.Repo, n), nil, &r); err != nil {
		return nil, err
	}
	a := r.alert()
	return &a, nil
}

// DependabotPRs lists the open PRs Dependabot opened, by their
// dependabot/ branches: at most 1000.
func (c *Client) DependabotPRs(ctx context.Context) ([]PR, error) {
	var out []PR
	for page := 1; page <= 10; page++ {
		var raw []struct {
			PR
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		q := url.Values{"state": {"open"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, "GET", "/repos/"+c.Repo+"/pulls?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.User.Login == "dependabot[bot]" && strings.HasPrefix(r.Head.Ref, "dependabot/") {
				out = append(out, r.PR)
			}
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}
