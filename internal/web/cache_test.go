package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTTLCache(t *testing.T) {
	now := time.Unix(0, 0)
	c := newTTL[int, int](time.Minute)
	c.now = func() time.Time { return now }
	calls := 0
	load := func(_ context.Context, k int) (int, error) {
		calls++
		if k < 0 {
			return 0, errors.New("no")
		}
		return k * 2, nil
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if v, _ := c.get(ctx, 2, load); v != 4 {
			t.Fatal(v)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	now = now.Add(2 * time.Minute)
	c.get(ctx, 2, load)
	if calls != 2 {
		t.Fatal("an expired entry must be reloaded")
	}
	c.get(ctx, -1, load)
	c.get(ctx, -1, load)
	if calls != 4 {
		t.Fatal("errors must not be cached")
	}
}
