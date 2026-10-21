package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/push"
	"github.com/claptimes5/factory/internal/store"
)

// deliverer sends the factory's notifications where they go: the channel
// in FACTORY_NOTIFY_URL (ntfy, Slack or Discord) and every browser that
// turned on push from the dashboard. In quiet hours only urgent ones go
// out (see pipeline.Urgent); the rest are held, and sent as one catch-up
// once the quiet hours end.
type deliverer struct {
	st      *store.Store
	channel notify.Notifier // nil without FACTORY_NOTIFY_URL
	push    *push.Sender    // nil if the key can't be read or made
	quiet   config.QuietHours
	now     func() time.Time
}

// deliveries is the serving factory's deliverer: build makes it, serve
// runs its catch-up and hands its push sender to the dashboard.
var deliveries *deliverer

// vapidKey is where the factory keeps its Web Push key (store kv).
const vapidKey = "vapid_key"

// pushSubject is who push services may contact about the factory's pushes.
const pushSubject = "https://github.com/claptimes5/factory"

func newDeliverer(cfg *config.Config, st *store.Store) *deliverer {
	d := &deliverer{st: st, quiet: cfg.Notify.QuietHours, now: time.Now}
	n, err := notify.New(os.Getenv("FACTORY_NOTIFY_URL"))
	if err != nil {
		slog.Warn("notifications to FACTORY_NOTIFY_URL disabled", "err", err)
	} else if n != nil {
		d.channel = &notify.RateLimited{Next: n, N: 20, Window: time.Hour}
	}
	if st != nil {
		if s, err := pushSender(context.Background(), st); err != nil {
			slog.Warn("web push disabled", "err", err)
		} else {
			d.push = s
		}
	}
	return d
}

// pushSender reads the factory's Web Push key, making it the first time.
func pushSender(ctx context.Context, st *store.Store) (*push.Sender, error) {
	saved, err := st.KV(ctx, vapidKey)
	if err != nil {
		return nil, err
	}
	if saved != "" {
		k, err := push.DecodeKey(saved)
		if err != nil {
			return nil, fmt.Errorf("the saved push key: %w", err)
		}
		return &push.Sender{Key: k, Subject: pushSubject}, nil
	}
	k, err := push.NewKey()
	if err != nil {
		return nil, err
	}
	enc, err := push.EncodeKey(k)
	if err != nil {
		return nil, err
	}
	if err := st.SetKV(ctx, vapidKey, enc); err != nil {
		return nil, err
	}
	return &push.Sender{Key: k, Subject: pushSubject}, nil
}

// notify is the pipeline's Notify: now, or held for the catch-up.
func (d *deliverer) notify(ctx context.Context, msg string) {
	urgent := pipeline.IsUrgent(ctx)
	if !urgent && d.quiet.Quiet(d.now()) && d.st != nil {
		if err := d.st.HoldNotification(ctx, msg); err == nil {
			return
		}
	}
	d.send(ctx, "Factory", msg, urgent)
}

// send delivers one message everywhere it goes, now.
func (d *deliverer) send(ctx context.Context, title, msg string, urgent bool) {
	if d.channel != nil {
		if err := d.channel.Send(ctx, title, msg); err != nil {
			slog.Warn("notify", "err", err)
		}
	}
	if d.push == nil || d.st == nil {
		return
	}
	subs, err := d.st.PushSubscriptions(ctx)
	if err != nil {
		slog.Warn("push", "err", err)
		return
	}
	m := push.Message{Title: title, Body: truncateMsg(msg, 1500), URL: "/app/"}
	for _, s := range subs {
		err := d.push.Send(ctx, push.Subscription{Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth}, m, urgent)
		switch {
		case errors.Is(err, push.ErrGone):
			_ = d.st.RemovePushSubscription(ctx, s.Endpoint)
		case err != nil:
			slog.Warn("push", "agent", s.Agent, "err", err)
			_ = d.st.PushFailed(ctx, s.Endpoint, store.Now())
		case s.FailedAt != "":
			_ = d.st.PushFailed(ctx, s.Endpoint, "")
		}
	}
}

// catchUp sends what quiet hours held, as one message, once they're over.
func (d *deliverer) catchUp(ctx context.Context) {
	if d.st == nil || d.quiet.Quiet(d.now()) {
		return
	}
	held, err := d.st.TakeHeldNotifications(ctx)
	if err != nil || len(held) == 0 {
		return
	}
	title := "While it was quiet"
	if len(held) > 1 {
		title = fmt.Sprintf("While it was quiet: %d updates", len(held))
	}
	d.send(ctx, title, truncateMsg("• "+strings.Join(held, "\n• "), 3500), false)
}

// catchUpLoop checks every minute whether quiet hours ended with
// something held.
func (d *deliverer) catchUpLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		d.catchUp(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
