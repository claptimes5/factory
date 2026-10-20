package web

import (
	"net/http"
	"strings"
)

// APIPlanUsage is how much of each subscription plan's usage windows the
// factory's harnesses have used, for the harnesses that run on one. On a
// plan the windows are what limit the work; dollar figures are what the
// same work would cost at API prices.
type APIPlanUsage struct {
	Plans []APIUsagePlan `json:"plans"`
}

// APIUsagePlan is one harness on a subscription plan.
type APIUsagePlan struct {
	Harness string `json:"harness"` // claude, codex
	Name    string `json:"name"`    // "Claude", "Codex"
	Plan    string `json:"plan"`    // "Claude subscription", "ChatGPT plan"
	// Windows are its latest readings, five-hour first; empty until one
	// arrives, and NoReading then says when one will.
	Windows   []APIUsage `json:"windows"`
	NoReading string     `json:"no_reading"`
	// PauseAt is the percent of a five-hour or weekly window at which its
	// jobs wait for the window to reset.
	PauseAt float64 `json:"pause_at"`
}

// plans describes each harness's plan, and when it reports its usage.
var plans = map[string]struct{ name, plan, noReading string }{
	// Claude Code puts the reading in a rate_limit_event: newer CLIs send
	// every window's on each job, older ones only near a limit.
	"claude": {"Claude", "Claude subscription", "No reading yet: Claude reports usage as its jobs run."},
	// Codex writes its windows to its session file on every run, which
	// the factory copies out of a Docker job.
	"codex": {"Codex", "ChatGPT plan", "No reading yet: Codex reports usage after each job it runs with the Docker runner."},
}

func (s *Server) routesUsage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/usage", s.apiPlanUsage)
}

func (s *Server) apiPlanUsage(w http.ResponseWriter, r *http.Request) {
	out := APIPlanUsage{Plans: []APIUsagePlan{}}
	if len(s.Plans) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	limits, err := s.Store.Limits(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, h := range s.Plans {
		d, ok := plans[h]
		if !ok {
			d.name, d.plan, d.noReading = h, h+" plan", "No reading yet."
		}
		p := APIUsagePlan{Harness: h, Name: d.name, Plan: d.plan, Windows: []APIUsage{}, PauseAt: s.PauseAtUsage}
		// Five-hour, then weekly, then any other the CLI names.
		for _, want := range []string{"5h", "weekly", ""} {
			for _, l := range limits {
				if !strings.EqualFold(l.Harness, h) || want != "" && l.Window != want || want == "" && (l.Window == "5h" || l.Window == "weekly") {
					continue
				}
				p.Windows = append(p.Windows, APIUsage{Harness: l.Harness, Window: l.Window, UsedPercent: l.UsedPercent, ResetsAt: l.ResetsAt, SeenAt: l.SeenAt})
			}
		}
		if len(p.Windows) == 0 {
			p.NoReading = d.noReading
		}
		out.Plans = append(out.Plans, p)
	}
	writeJSON(w, http.StatusOK, out)
}
