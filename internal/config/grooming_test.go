package config

import (
	"strings"
	"testing"
	"time"
)

func TestGrooming(t *testing.T) {
	b, _ := exampleYAML()
	y := string(b)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	// Off by default, with the rest ready for `factory groom`.
	g := c.Grooming
	if g.Every != 0 || g.StaleAfter != 90*24*time.Hour || g.MaxIssues != 50 || c.GroomBudget() != c.Budgets.Triage || c.Timeout("groom") != 10*time.Minute {
		t.Fatalf("defaults: %+v", g)
	}
	if r := c.Routing["groom"]; r.Harness != "claude" || r.Model != "sonnet" {
		t.Fatalf("routing.groom = %+v", r)
	}
	if got := c.GroomRepos(); len(got) != 1 || got[0] != "claptimes5/factory" {
		t.Fatalf("repos %v", got)
	}
	c, err = Parse([]byte(y + "\ngrooming: {every: 168h, stale_after: 30d, max_issues: 80, budget: 1.5, repos: [claptimes5/factory]}\n"))
	if err != nil || c.Grooming.Every != 168*time.Hour || c.Grooming.StaleAfter != 30*24*time.Hour || c.Grooming.MaxIssues != 80 || c.GroomBudget() != 1.5 {
		t.Fatalf("set: %+v %v", c.Grooming, err)
	}
	for _, off := range []string{"0", "none"} {
		if c, err := Parse([]byte(y + "\ngrooming: {every: " + off + "}\n")); err != nil || c.Grooming.Every != 0 {
			t.Fatalf("every %s: %v", off, err)
		}
	}
	if c, err := Parse([]byte(y + "\ngrooming: {every: 24h, stale_after: 2160h}\n")); err != nil || c.Grooming.StaleAfter != 90*24*time.Hour {
		t.Fatalf("hours: %v", err)
	}
	// A codex triage keeps its harness and model; a groom route of its own wins.
	c, err = Parse([]byte(strings.Replace(y, "triage:            {harness: claude, model: haiku}", "triage: {harness: codex, model: gpt-6-luna}", 1)))
	if err != nil || c.Routing["groom"].Harness != "codex" || c.Routing["groom"].Model != "gpt-6-luna" {
		t.Fatalf("codex triage: %+v %v", c.Routing["groom"], err)
	}
	c, err = Parse([]byte(strings.Replace(y, "routing:\n", "routing:\n  groom: {harness: claude, model: opus}\n", 1)))
	if err != nil || c.Routing["groom"].Model != "opus" {
		t.Fatalf("own route: %+v %v", c.Routing["groom"], err)
	}
	if _, err := Parse([]byte(y + "\ngrooming: {budget: -1}\n")); err == nil || !strings.Contains(err.Error(), "grooming.budget can't be negative") {
		t.Fatalf("negative budget: %v", err)
	}
	c.Grooming.Off()
	if c.Grooming.Every != 0 {
		t.Fatal("Off left it on")
	}
	for name, bad := range map[string]string{
		"too often":       "grooming: {every: 12h}",
		"not a duration":  "grooming: {every: weekly}",
		"negative":        "grooming: {every: -168h}",
		"stale too soon":  "grooming: {stale_after: 3d}",
		"stale nonsense":  "grooming: {stale_after: 9x}",
		"too many issues": "grooming: {max_issues: 500}",
		"no issues":       "grooming: {max_issues: -1}",
		"negative budget": "grooming: {budget: -1}",
		"unknown repo":    "grooming: {repos: [o/elsewhere]}",
		"unknown key":     "grooming: {interval: 168h}",
	} {
		if _, err := Parse([]byte(y + "\n" + bad + "\n")); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}
