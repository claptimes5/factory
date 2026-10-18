package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNtfyAndSlackBodies(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Header.Get("Title")+"|"+string(b))
	}))
	defer srv.Close()

	n := &ntfy{url: srv.URL}
	if err := n.Send(context.Background(), "PR ready", "#3 is ready"); err != nil {
		t.Fatal(err)
	}
	s := &webhook{url: srv.URL, body: func(t, m string) any { return map[string]string{"text": t + ":" + m} }}
	if err := s.Send(context.Background(), "Parked", "#4"); err != nil {
		t.Fatal(err)
	}
	if got[0] != "PR ready|#3 is ready" || !strings.Contains(got[1], `"text":"Parked:#4"`) {
		t.Fatalf("got %q", got)
	}
}

func TestNewPicksChannel(t *testing.T) {
	for url, want := range map[string]string{
		"https://ntfy.sh/factory":                "*notify.ntfy",
		"https://hooks.slack.com/services/x":     "*notify.webhook",
		"https://discord.com/api/webhooks/1/abc": "*notify.webhook",
	} {
		n, err := New(url)
		if err != nil || fmt.Sprintf("%T", n) != want {
			t.Fatalf("%s: %T %v", url, n, err)
		}
	}
	if n, err := New(""); n != nil || err != nil {
		t.Fatal("empty URL means no notifier")
	}
	if _, err := New("https://example.com/hook"); err == nil {
		t.Fatal("unknown URL should be rejected")
	}
}

type count struct{ n int }

func (c *count) Send(context.Context, string, string) error { c.n++; return nil }

func TestRateLimit(t *testing.T) {
	c := &count{}
	now := time.Unix(0, 0)
	r := &RateLimited{Next: c, N: 2, Window: time.Minute, now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		r.Send(context.Background(), "t", "m")
	}
	if c.n != 2 || r.dropped != 3 {
		t.Fatalf("sent %d dropped %d", c.n, r.dropped)
	}
	now = now.Add(2 * time.Minute)
	r.Send(context.Background(), "t", "m")
	if c.n != 3 || r.dropped != 0 {
		t.Fatalf("after window: sent %d dropped %d", c.n, r.dropped)
	}
}

func TestWebhooksNeverPing(t *testing.T) {
	if s := slackBody("Filed", "<!channel> & <https://evil.example|click>").(map[string]string)["text"]; s != "*Filed*\n&lt;!channel&gt; &amp; &lt;https://evil.example|click&gt;" {
		t.Fatalf("slack %s", s)
	}
	b, _ := json.Marshal(discordBody("Filed", "@everyone"))
	if s := string(b); !strings.Contains(s, `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("discord %s", s)
	}
}
