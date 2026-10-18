// Package notify sends short messages to one configured channel: an ntfy
// topic (phone push), a Slack incoming webhook or a Discord webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Notifier sends a message.
type Notifier interface {
	Send(ctx context.Context, title, msg string) error
}

// New builds a notifier from a URL. The scheme picks the channel:
//
//	https://ntfy.sh/<topic>                     ntfy (any URL whose host contains "ntfy")
//	https://hooks.slack.com/services/...        Slack
//	https://discord.com/api/webhooks/...        Discord
//
// An empty URL returns nil (notifications go to the log only).
func New(url string) (Notifier, error) {
	switch {
	case url == "":
		return nil, nil
	case strings.Contains(url, "hooks.slack.com"):
		return &webhook{url: url, body: slackBody}, nil
	case strings.Contains(url, "discord.com/api/webhooks") || strings.Contains(url, "discordapp.com/api/webhooks"):
		return &webhook{url: url, body: discordBody}, nil
	case strings.Contains(url, "ntfy"):
		return &ntfy{url: url}, nil
	}
	return nil, fmt.Errorf("notify: unrecognized URL %q (want ntfy, Slack or Discord)", url)
}

// Messages can quote text from issues and errors, which anyone may have
// written, so they never ping: Slack's control characters are escaped
// (no <!channel>, <@U123> or <url|label> links), and Discord is told to
// resolve no mentions (no @everyone).

var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func slackBody(t, m string) any {
	return map[string]string{"text": "*" + slackEscape.Replace(t) + "*\n" + slackEscape.Replace(m)}
}

func discordBody(t, m string) any {
	return map[string]any{"content": "**" + t + "**\n" + m, "allowed_mentions": map[string]any{"parse": []string{}}}
}

type webhook struct {
	url  string
	body func(title, msg string) any
}

func (w *webhook) Send(ctx context.Context, title, msg string) error {
	b, _ := json.Marshal(w.body(title, msg))
	req, err := http.NewRequestWithContext(ctx, "POST", w.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return send(req)
}

type ntfy struct{ url string }

func (n *ntfy) Send(ctx context.Context, title, msg string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", n.url, strings.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "factory")
	return send(req)
}

func send(req *http.Request) error {
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("notify: HTTP %d", resp.StatusCode)
	}
	return nil
}

// RateLimited wraps a notifier so that at most n messages go out per window;
// the rest are dropped and counted, and the count is reported with the next
// message that goes through.
type RateLimited struct {
	Next   Notifier
	N      int
	Window time.Duration

	mu      sync.Mutex
	sent    []time.Time
	dropped int
	now     func() time.Time
}

func (r *RateLimited) Send(ctx context.Context, title, msg string) error {
	r.mu.Lock()
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now()
	keep := r.sent[:0]
	for _, s := range r.sent {
		if t.Sub(s) < r.Window {
			keep = append(keep, s)
		}
	}
	r.sent = keep
	if len(r.sent) >= r.N {
		r.dropped++
		r.mu.Unlock()
		return nil
	}
	r.sent = append(r.sent, t)
	if r.dropped > 0 {
		msg += fmt.Sprintf("\n(%d earlier notifications were rate limited)", r.dropped)
		r.dropped = 0
	}
	r.mu.Unlock()
	return r.Next.Send(ctx, title, msg)
}
