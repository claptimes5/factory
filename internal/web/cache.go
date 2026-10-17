package web

import (
	"context"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/github"
)

// ttlCache remembers successful lookups for a while, so dashboard refetches
// (every change, from every open tab) do not spend the daemon's GitHub rate
// limit.
type ttlCache[K comparable, V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[K]ttlEntry[V]
	now func() time.Time
}

type ttlEntry[V any] struct {
	v  V
	at time.Time
}

func (c *ttlCache[K, V]) get(ctx context.Context, k K, load func(context.Context, K) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.m[k]; ok && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()
	v, err := load(ctx, k)
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 1000 {
		c.m = map[K]ttlEntry[V]{}
	}
	c.m[k] = ttlEntry[V]{v, c.now()}
	return v, nil
}

func newTTL[K comparable, V any](ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, m: map[K]ttlEntry[V]{}, now: time.Now}
}

// CachedIssue caches issue reads for ttl.
func CachedIssue(f IssueFunc, ttl time.Duration) IssueFunc {
	c := newTTL[int, *github.Issue](ttl)
	return func(ctx context.Context, n int) (*github.Issue, error) { return c.get(ctx, n, f) }
}

// CachedChecks caches check runs per commit for ttl.
func CachedChecks(f CheckFunc, ttl time.Duration) CheckFunc {
	c := newTTL[string, []github.CheckRun](ttl)
	return func(ctx context.Context, sha string) ([]github.CheckRun, error) { return c.get(ctx, sha, f) }
}
