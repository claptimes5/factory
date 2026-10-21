package config

import (
	"os"
	"strings"
	"testing"
)

// Every preset makes a config that loads, for one harness or two either
// way round, and is the preset the config then says it has.
func TestPresets(t *testing.T) {
	src, err := os.ReadFile("../../factory.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Fast mode on Opus, which only Opus may have: a preset that moves the
	// route off Opus must drop it.
	src = []byte(strings.Replace(string(src), "implement.L:       {harness: claude, model: opus}", "implement.L:       {harness: claude, model: opus, fast: true}", 1))
	c, err := ParseIn(src, "../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range [][2]string{{"claude", "codex"}, {"codex", "claude"}, {"claude", "claude"}, {"codex", "codex"}} {
		for _, p := range Presets {
			changes, err := c.PresetChanges(p.Key, h[0], h[1])
			if err != nil {
				t.Fatal(err)
			}
			out, err := Edit(src, changes)
			if err != nil {
				t.Fatalf("%s %v: %v", p.Key, h, err)
			}
			got, err := ParseIn(out, "../..")
			if err != nil {
				t.Fatalf("%s %v doesn't load: %v\n%s", p.Key, h, err, out)
			}
			if b, r := got.Harnesses(); b != h[0] || r != h[1] {
				t.Fatalf("%s %v: builds on %s, reviews on %s", p.Key, h, b, r)
			}
			if now, err := PresetNow(out); err != nil || now != p.Key {
				t.Fatalf("%s %v reads as %q, %v", p.Key, h, now, err)
			}
			if sec := got.Routing["security"]; sec.Model == got.Routing["review"].Model {
				t.Fatalf("%s %v: the security pass on the reviewer's model %s", p.Key, h, sec.Model)
			}
		}
	}
	// The example as shipped is Balanced? It has its own choices: it reads
	// as one preset or as set by hand, never an error.
	if _, err := PresetNow(src); err != nil {
		t.Fatal(err)
	}
}

// Codex's models come from the config, cheapest first; Claude's are its
// aliases.
func TestPresetModels(t *testing.T) {
	c := &Config{Pricing: map[string]Price{"gpt-big": {OutputPerMTok: 10}, "gpt-small": {OutputPerMTok: 1}}}
	if m := c.models("codex"); m != (models{"gpt-small", "gpt-small", "gpt-big"}) && m != (models{"gpt-small", "gpt-big", "gpt-big"}) {
		t.Fatalf("codex models %v", m)
	}
	if r := c.pick("codex", mid, ""); r.Model != "gpt-big" || r.Effort != "medium" {
		t.Fatalf("mid = %+v", r)
	}
	if m := c.models("claude"); m != (models{"haiku", "sonnet", "opus"}) {
		t.Fatalf("claude models %v", m)
	}
}
