package config

import (
	"os"
	"strings"
	"testing"
)

func TestLearning(t *testing.T) {
	b, _ := os.ReadFile("../../factory.example.yaml")
	y := string(b)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	// On by default, on Sonnet since triage runs on Haiku, with triage's budget.
	if !c.Learning.On() || c.Learning.MinFindings != 2 || c.LearnBudget() != c.Budgets.Triage || c.Timeout("learn").Minutes() != 10 {
		t.Fatalf("defaults: %+v", c.Learning)
	}
	if r := c.Routing["learn"]; r.Harness != "claude" || r.Model != "sonnet" {
		t.Fatalf("routing.learn = %+v", r)
	}
	c, err = Parse([]byte(y + "\nlearning: {enabled: false, min_findings: 3, budget: 1.5}\n"))
	if err != nil || c.Learning.On() || c.Learning.MinFindings != 3 || c.LearnBudget() != 1.5 {
		t.Fatalf("set: %+v %v", c, err)
	}
	// A codex triage keeps its own model; a learn route of its own wins.
	c, err = Parse([]byte(strings.Replace(y, "triage:            {harness: claude, model: haiku}", "triage: {harness: codex, model: gpt-6-luna}", 1)))
	if err != nil || c.Routing["learn"].Harness != "codex" || c.Routing["learn"].Model != "gpt-6-luna" {
		t.Fatalf("codex triage: %+v %v", c.Routing["learn"], err)
	}
	c, err = Parse([]byte(strings.Replace(y, "routing:\n", "routing:\n  learn: {harness: claude, model: opus}\n", 1)))
	if err != nil || c.Routing["learn"].Model != "opus" {
		t.Fatalf("own route: %+v %v", c.Routing["learn"], err)
	}
	for name, bad := range map[string]string{
		"one finding":     "learning: {min_findings: 1}",
		"negative budget": "learning: {budget: -1}",
		"unknown key":     "learning: {every: 1h}",
	} {
		if _, err := Parse([]byte(y + "\n" + bad + "\n")); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}
