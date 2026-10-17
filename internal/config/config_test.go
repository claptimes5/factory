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

func TestRefusesSameHarnessForBuilderAndReviewer(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	y := strings.Replace(string(b), "review:            {harness: codex,  model: gpt-5-codex}", "review: {harness: claude, model: opus}", 1)
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "different harnesses") {
		t.Fatalf("want harness clash error, got %v", err)
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
