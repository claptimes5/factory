package config

import (
	"fmt"
	"time"
)

// QuietHours are when only something wrong is sent: the rest waits for
// one catch-up when they end. From and Until are 24-hour times such as
// 19:00 and 08:00 (Until may be earlier: the hours span midnight); both
// empty is off. Weekends keeps Saturday and Sunday quiet all day.
type QuietHours struct {
	From     string `yaml:"from,omitempty"`
	Until    string `yaml:"until,omitempty"`
	Weekends bool   `yaml:"weekends,omitempty"`
	// Timezone is an IANA name such as Europe/Madrid; empty is the
	// factory machine's own.
	Timezone string `yaml:"timezone,omitempty"`
}

// On says whether there are quiet hours at all.
func (q QuietHours) On() bool { return q.From != "" || q.Weekends }

func clock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a time like 19:00", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (q QuietHours) check() error {
	if (q.From == "") != (q.Until == "") {
		return fmt.Errorf("notify.quiet_hours needs both from and until, or neither")
	}
	if q.From != "" {
		if _, err := clock(q.From); err != nil {
			return fmt.Errorf("notify.quiet_hours.from: %w", err)
		}
		if _, err := clock(q.Until); err != nil {
			return fmt.Errorf("notify.quiet_hours.until: %w", err)
		}
	}
	if q.Timezone != "" {
		if _, err := time.LoadLocation(q.Timezone); err != nil {
			return fmt.Errorf("notify.quiet_hours.timezone: %w", err)
		}
	}
	return nil
}

// Quiet says whether t falls in the quiet hours.
func (q QuietHours) Quiet(t time.Time) bool {
	if q.Timezone != "" {
		if loc, err := time.LoadLocation(q.Timezone); err == nil {
			t = t.In(loc)
		}
	} else {
		t = t.Local()
	}
	if q.Weekends && (t.Weekday() == time.Saturday || t.Weekday() == time.Sunday) {
		return true
	}
	if q.From == "" {
		return false
	}
	from, _ := clock(q.From)
	until, _ := clock(q.Until)
	now := t.Hour()*60 + t.Minute()
	if from <= until {
		return now >= from && now < until
	}
	return now >= from || now < until // across midnight
}
