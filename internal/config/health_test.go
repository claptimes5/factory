package config

import (
	"os"
	"testing"
	"time"
)

func TestMainHealth(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	y := string(b)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	// Off by default; every 10 minutes and three issues a day once on.
	if h := c.MainHealth; h.Enabled || h.Every.Duration != 10*time.Minute || h.DailyIssueCap != 3 || h.RequireApproval {
		t.Fatalf("defaults: %+v", h)
	}
	c, err = Parse([]byte(y + "\nmain_health: {enabled: true, every: 5m, daily_issue_cap: 1, require_approval: true}\n"))
	if err != nil || !c.MainHealth.Enabled || c.MainHealth.Every.Duration != 5*time.Minute || c.MainHealth.DailyIssueCap != 1 || !c.MainHealth.RequireApproval {
		t.Fatalf("set: %+v %v", c.MainHealth, err)
	}
	for name, bad := range map[string]string{
		"too often":   "main_health: {every: 30s}",
		"negative":    "main_health: {daily_issue_cap: -1}",
		"bad every":   "main_health: {every: often}",
		"unknown key": "main_health: {interval: 1h}",
	} {
		if _, err := Parse([]byte(y + "\n" + bad + "\n")); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}
