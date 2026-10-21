package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/store/storetest"
)

type sent struct {
	mu   sync.Mutex
	msgs []string
}

func (s *sent) Send(_ context.Context, title, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, title+": "+msg)
	return nil
}

// In quiet hours only urgent notifications go out; the rest wait for one
// catch-up when the hours end.
func TestQuietHoursHoldAndCatchUp(t *testing.T) {
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	ch := &sent{}
	now := time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC) // a Tuesday night
	d := &deliverer{st: st, channel: ch, quiet: config.QuietHours{From: "19:00", Until: "08:00", Timezone: "UTC"}, now: func() time.Time { return now }}
	ctx := context.Background()
	d.notify(ctx, "acme/web#41 is ready for review")
	d.notify(ctx, "Split acme/api#9 into 3 issues")
	d.notify(pipeline.Urgent(ctx), "Main health: test went red on acme/web")
	if len(ch.msgs) != 1 || !strings.Contains(ch.msgs[0], "went red") {
		t.Fatalf("in quiet hours: %v", ch.msgs)
	}
	d.catchUp(ctx) // still quiet: nothing yet
	if len(ch.msgs) != 1 {
		t.Fatalf("caught up while quiet: %v", ch.msgs)
	}
	now = time.Date(2026, 10, 21, 8, 1, 0, 0, time.UTC)
	d.catchUp(ctx)
	if len(ch.msgs) != 2 || !strings.HasPrefix(ch.msgs[1], "While it was quiet: 2 updates: • acme/web#41 is ready for review\n• Split") {
		t.Fatalf("catch-up: %q", ch.msgs)
	}
	d.catchUp(ctx)
	d.notify(ctx, "Opened a lessons PR")
	if len(ch.msgs) != 3 || !strings.Contains(ch.msgs[2], "lessons PR") {
		t.Fatalf("after quiet hours: %q", ch.msgs)
	}
}
