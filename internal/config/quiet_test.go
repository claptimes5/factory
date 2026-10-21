package config

import (
	"testing"
	"time"
)

func TestQuietHours(t *testing.T) {
	night := QuietHours{From: "19:00", Until: "08:00", Weekends: true, Timezone: "UTC"}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for when, want := range map[string]bool{
		"2026-10-20T18:59:00Z": false, // a Tuesday evening
		"2026-10-20T19:00:00Z": true,
		"2026-10-21T02:00:00Z": true, // across midnight
		"2026-10-21T08:00:00Z": false,
		"2026-10-24T12:00:00Z": true, // a Saturday
	} {
		if got := night.Quiet(at(when)); got != want {
			t.Errorf("%s: quiet = %v, want %v", when, got, want)
		}
	}
	day := QuietHours{From: "12:00", Until: "13:00", Timezone: "UTC"}
	if !day.Quiet(at("2026-10-20T12:30:00Z")) || day.Quiet(at("2026-10-20T13:30:00Z")) {
		t.Error("hours within a day")
	}
	for _, bad := range []QuietHours{{From: "19:00"}, {From: "7pm", Until: "08:00"}, {From: "19:00", Until: "08:00", Timezone: "Mars/Olympus"}} {
		if bad.check() == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if (QuietHours{}).On() || (QuietHours{}).Quiet(time.Now()) {
		t.Error("no quiet hours is never quiet")
	}
}
