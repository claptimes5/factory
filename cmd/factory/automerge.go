package main

import (
	"context"
	"fmt"
	"io"
	"os/user"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/gitlab"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/store"
)

// Both forges' clients have what the guard rails read.
var (
	_ pipeline.RevertForge = (*gitlab.Client)(nil)
	_ pipeline.MergeQueuer = (*gitlab.Client)(nil)
	_ pipeline.HealthForge = (*gitlab.Client)(nil)
)

// automerge shows each repo's auto-merge breaker, or with reset <repo>
// turns a tripped one off so the repo's PRs may merge themselves again.
// The daemon reads the breaker from the database, so it needn't restart.
func automerge(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store, args []string) error {
	p := &pipeline.Pipeline{Cfg: cfg, Store: st}
	switch {
	case len(args) == 0:
		for _, l := range autoMergeLines(ctx, st, cfg) {
			fmt.Fprintln(w, strings.ReplaceAll(l, "\t", " "))
		}
		return nil
	case len(args) == 2 && args[0] == "reset":
		by := "factory automerge reset"
		if u, err := user.Current(); err == nil && u.Username != "" {
			by += " (" + u.Username + ")"
		}
		if err := p.ResetAutoMerge(ctx, args[1], by); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: auto-merge's breaker is reset; its PRs may merge themselves again. PRs whose auto-merge it cancelled still wait for you.\n", args[1])
		return nil
	}
	return fmt.Errorf("usage: factory automerge [reset <repo>]")
}

// autoMergeLines are status's lines on auto-merge's guard rails: each
// repo's breaker where auto-merge is on or the breaker tripped, and the
// review escape and revert rates.
func autoMergeLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	p := &pipeline.Pipeline{Cfg: cfg, Store: st}
	var out []string
	for _, b := range p.Breakers(ctx) {
		if !b.On && !b.Tripped {
			continue
		}
		line := fmt.Sprintf("auto-merge %s:\t", b.Repo)
		switch {
		case b.Tripped:
			// The reason names PRs and checks: CI's text goes through SafeText.
			line += fmt.Sprintf("off since %s, its breaker tripped: %s. `factory automerge reset %s` turns it back on", b.TrippedAt, pipeline.SafeText(b.Reason, 300), b.Repo)
			if b.CoolOff > 0 {
				line += fmt.Sprintf(" (or it does %s after it tripped)", fmtDays(b.CoolOff))
			}
		case !b.On:
			line += "off (auto_merge)"
		case b.Threshold == 0:
			line += "on; its breaker never trips"
		default:
			line += fmt.Sprintf("on; %d of %d strikes in %s trip its breaker", b.Strikes, b.Threshold, fmtDays(b.Window))
		}
		out = append(out, line)
	}
	if rep, err := metrics.Compute(ctx, st); err == nil && (rep.RevertMerged > 0 || rep.ReviewEscapes >= 0) {
		line := "quality:\treview escapes " + pct(rep.ReviewEscapes)
		line += fmt.Sprintf(", reverts %s (%d of %d factory PRs merged in %d days)", pct(rep.RevertRate), rep.Reverted, rep.RevertMerged, rep.RevertWindowDays)
		if rep.BrokeMain > 0 {
			line += fmt.Sprintf(", %d turned main red", rep.BrokeMain)
		}
		out = append(out, line)
	}
	return out
}

// fmtDays writes a duration in days when it's whole days.
func fmtDays(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

// pct writes a rate, or - when there's no data.
func pct(r float64) string {
	if r < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", r*100)
}
