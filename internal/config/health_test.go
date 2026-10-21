package config

import (
	"strings"
	"testing"
	"time"
)

func TestMainHealth(t *testing.T) {
	b, _ := exampleYAML()
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
	// 0 is the default, as the error for a negative one says.
	c, err = Parse([]byte(y + "\nmain_health: {daily_issue_cap: 0}\n"))
	if err != nil || c.MainHealth.DailyIssueCap != 3 {
		t.Fatalf("0: %d %v", c.MainHealth.DailyIssueCap, err)
	}
	if _, err := Parse([]byte(y + "\nmain_health: {daily_issue_cap: -1}\n")); err == nil || !strings.Contains(err.Error(), "0 or left out is 3") {
		t.Fatalf("negative: %v", err)
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
