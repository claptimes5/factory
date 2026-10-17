package config

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T) *Config {
	t.Helper()
	c, err := Load("../../factory.yaml")
	if err != nil {
		t.Fatalf("repo factory.yaml must load: %v", err)
	}
	return c
}

func TestRepoConfigLoads(t *testing.T) {
	c := load(t)
	if c.Repo != "claptimes5/factory" || c.Budgets.ConcurrentJobs != 3 {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestReviewerMustDifferFromBuilder(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	claudeReview := func(model, tiers string) string {
		y := strings.Replace(string(b), "review:            {harness: codex,  model: gpt-5-codex}", "review: {harness: claude, model: "+model+"}", 1)
		y = strings.Replace(y, "review.high_risk:  {harness: codex,  model: gpt-5-codex, effort: high}", "", 1)
		y = strings.Replace(y, "implement.L:       {harness: claude, model: opus}", "implement.L:       {harness: claude, model: sonnet}", 1)
		return strings.Replace(y, "implement: [sonnet, opus]", "implement: "+tiers, 1)
	}
	cases := []struct {
		name, model, tiers string
		ok                 bool
	}{
		{"same model", "sonnet", "[sonnet]", false},
		{"model a retry escalates to", "opus", "[sonnet, opus]", false},
		{"different model at every tier", "opus", "[sonnet]", true},
		{"names compare without case", "Sonnet", "[sonnet]", false},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(claudeReview(tc.model, tc.tiers)))
		if tc.ok && err != nil {
			t.Errorf("%s: want ok, got %v", tc.name, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "differ from the builder")) {
			t.Errorf("%s: want builder/reviewer clash, got %v", tc.name, err)
		}
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	if _, err := Parse(append(b, []byte("\nbogus: 1\n")...)); err == nil {
		t.Fatal("unknown top-level field should be rejected")
	}
}

func TestEscalation(t *testing.T) {
	c := load(t) // implement: [sonnet, opus], 2 per tier
	cases := []struct {
		size    string
		attempt int
		model   string
		ok      bool
	}{
		{"S", 1, "sonnet", true},
		{"S", 2, "sonnet", true},
		{"S", 3, "opus", true},
		{"S", 4, "opus", true},
		{"S", 5, "", false},
		{"L", 1, "opus", true},
		{"L", 2, "opus", true},
		{"L", 3, "", false},
	}
	for _, tc := range cases {
		r, ok := c.Implement(tc.size, tc.attempt)
		if ok != tc.ok || (ok && r.Model != tc.model) {
			t.Errorf("Implement(%s,%d) = %s,%v want %s,%v", tc.size, tc.attempt, r.Model, ok, tc.model, tc.ok)
		}
	}
}

func TestReviewRouteByRisk(t *testing.T) {
	c := load(t)
	if c.Review("high").Effort != "high" || c.Review("low").Effort != "" {
		t.Fatal("high risk should use review.high_risk")
	}
}
