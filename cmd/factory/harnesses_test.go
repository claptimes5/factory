package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

// Whichever harness comes first does everything; the second to arrive
// reviews what the first builds; dropping one leaves the other doing it
// all. How hard it works stays.
func TestRouteHarnesses(t *testing.T) {
	src, err := os.ReadFile("../../factory.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "factory.yaml")
	os.WriteFile(path, src, 0o644)
	roles := func() (string, string) {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		for k, r := range c.Routing {
			if r.Harness != "claude" && r.Harness != "codex" {
				t.Fatalf("%s routes to %q", k, r.Harness)
			}
		}
		return c.Harnesses()
	}
	steps := []struct {
		before, after []string
		builder, rev  string
		said          string
	}{
		{nil, []string{"codex"}, "codex", "codex", "Codex now builds and reviews."},
		{[]string{"codex"}, []string{"claude", "codex"}, "codex", "claude", "Codex now builds, and Claude reviews what it builds."},
		{[]string{"claude", "codex"}, []string{"claude", "codex"}, "codex", "claude", ""}, // a key updated: nothing moves
		{[]string{"claude", "codex"}, []string{"claude"}, "claude", "claude", "Claude now builds and reviews."},
	}
	for i, s := range steps {
		said, err := routeHarnesses(path, s.before, s.after)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if b, r := roles(); b != s.builder || r != s.rev || said != s.said {
			t.Fatalf("step %d: %s builds, %s reviews; said %q", i, b, r, said)
		}
	}
	// Only Codex: nothing routes to Claude.
	routeHarnesses(path, nil, []string{"codex"})
	c, _ := config.Load(path)
	for k, r := range c.Routing {
		if r.Harness == "claude" {
			t.Fatalf("%s still routes to Claude", k)
		}
	}
}
