package store

import (
	"context"
	"strings"
	"testing"
)

func TestDashboardValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	if v, err := s.KV(ctx, "home_seen"); v != "" || err != nil {
		t.Fatalf("unset: %q %v", v, err)
	}
	s.SetKV(ctx, "home_seen", "a")
	s.SetKV(ctx, "home_seen", "b")
	if v, _ := s.KV(ctx, "home_seen"); v != "b" {
		t.Fatalf("kv = %q", v)
	}

	s.AddPushSubscription(ctx, PushSubscription{Endpoint: "https://push/1", P256dh: "k", Auth: "a", Agent: "phone"})
	s.PushFailed(ctx, "https://push/1", "then")
	s.AddPushSubscription(ctx, PushSubscription{Endpoint: "https://push/1", P256dh: "k2", Auth: "a2"})
	subs, _ := s.PushSubscriptions(ctx)
	if len(subs) != 1 || subs[0].P256dh != "k2" || subs[0].FailedAt != "" {
		t.Fatalf("subscribing again replaces the keys and clears the failure: %+v", subs)
	}
	s.RemovePushSubscription(ctx, "https://push/1")
	if subs, _ := s.PushSubscriptions(ctx); len(subs) != 0 {
		t.Fatalf("after removing: %+v", subs)
	}

	s.HoldNotification(ctx, "one")
	s.HoldNotification(ctx, "two")
	held, err := s.TakeHeldNotifications(ctx)
	if err != nil || strings.Join(held, ",") != "one,two" {
		t.Fatalf("held = %v %v", held, err)
	}
	if held, _ := s.TakeHeldNotifications(ctx); len(held) != 0 {
		t.Fatalf("taken twice: %v", held)
	}
}
