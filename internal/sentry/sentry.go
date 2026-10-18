// Package sentry is a small client for the few Sentry API calls the
// factory makes: unresolved issues of a project, one issue, and an
// issue's latest event. Everything it returns about an error (titles,
// messages, file names) is steered by whoever triggered the error, so
// callers treat it as untrusted.
package sentry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls one Sentry's API with an auth token.
type Client struct {
	BaseURL string // default https://sentry.io
	Token   string
	HTTP    *http.Client
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("sentry: HTTP %d: %s", e.Status, e.Body) }

// StatusCode is the response's HTTP status.
func (e *HTTPError) StatusCode() int { return e.Status }

// RateLimit says whether the request was refused for a rate limit.
func (e *HTTPError) RateLimit() bool { return e.Status == 429 }

// Issue is the part of a Sentry issue the factory reads.
type Issue struct {
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Permalink string `json:"permalink"`
	Status    string `json:"status"` // unresolved, resolved, ignored
	Level     string `json:"level"`
	Count     string `json:"count"` // events, as a string
	UserCount int    `json:"userCount"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
}

// Events is how many events the issue has had.
func (i Issue) Events() int {
	n, _ := strconv.Atoi(i.Count)
	return n
}

// Frame is one stack frame.
type Frame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"absPath"`
	Module   string `json:"module"`
	Function string `json:"function"`
	LineNo   int    `json:"lineNo"`
	InApp    bool   `json:"inApp"`
}

// Exception is one exception of an event, its frames oldest call first.
type Exception struct {
	Type   string
	Value  string
	Frames []Frame
}

// Event is an issue's event: its exceptions, outermost last, as Sentry
// sends them.
type Event struct {
	ID         string
	Exceptions []Exception
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://sentry.io"
}

func (c *Client) do(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		body := strings.TrimSpace(string(b))
		if len(body) > 300 {
			body = body[:300]
		}
		return &HTTPError{Status: resp.StatusCode, Body: body}
	}
	return json.Unmarshal(b, out)
}

// Issues lists a project's unresolved issues, most events (sort freq) or
// most users (sort user) first: the first page, at most 25.
func (c *Client) Issues(ctx context.Context, org, project, sort string) ([]Issue, error) {
	q := url.Values{"query": {"is:unresolved"}, "sort": {sort}, "limit": {"25"}}
	var out []Issue
	err := c.do(ctx, fmt.Sprintf("/api/0/projects/%s/%s/issues/?%s", url.PathEscape(org), url.PathEscape(project), q.Encode()), &out)
	return out, err
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, org, id string) (*Issue, error) {
	var out Issue
	if err := c.do(ctx, fmt.Sprintf("/api/0/organizations/%s/issues/%s/", url.PathEscape(org), url.PathEscape(id)), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LatestEvent reads an issue's latest event.
func (c *Client) LatestEvent(ctx context.Context, org, id string) (*Event, error) {
	var raw struct {
		EventID string `json:"eventID"`
		Entries []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"entries"`
	}
	if err := c.do(ctx, fmt.Sprintf("/api/0/organizations/%s/issues/%s/events/latest/", url.PathEscape(org), url.PathEscape(id)), &raw); err != nil {
		return nil, err
	}
	ev := &Event{ID: raw.EventID}
	for _, e := range raw.Entries {
		if e.Type != "exception" {
			continue
		}
		var data struct {
			Values []struct {
				Type       string `json:"type"`
				Value      string `json:"value"`
				Stacktrace *struct {
					Frames []Frame `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		}
		if json.Unmarshal(e.Data, &data) != nil {
			continue
		}
		for _, v := range data.Values {
			x := Exception{Type: v.Type, Value: v.Value}
			if v.Stacktrace != nil {
				x.Frames = v.Stacktrace.Frames
			}
			ev.Exceptions = append(ev.Exceptions, x)
		}
	}
	return ev, nil
}
