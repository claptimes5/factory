// Package metrics derives the factory's quality numbers from the store.
// The number that matters is cost per merged PR; the rest explains it.
package metrics

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// Group is cost per merged PR for one slice of the work.
type Group struct {
	Key    string  `json:"key"`
	Merged int     `json:"merged"`
	Cost   float64 `json:"cost_usd"`
	PerPR  float64 `json:"cost_per_pr_usd"`
}

// Report is every derived view. Rates are 0..1; -1 means no data yet.
type Report struct {
	Items         int     `json:"items"`
	Merged        int     `json:"merged"`
	Closed        int     `json:"closed"`
	Parked        int     `json:"parked"`
	CostPerMerged float64 `json:"cost_per_merged_usd"`
	ByComplexity  []Group `json:"by_complexity"`
	ByModel       []Group `json:"by_model"` // the model whose implement job landed
	// FirstPass: of items that were implemented, the share whose first
	// implement job passed the gates and whose first review passed.
	FirstPass float64 `json:"first_pass_rate"`
	// Escalation: of implemented items, the share that needed a higher tier.
	Escalation float64 `json:"escalation_rate"`
	// ReviewBlocks counts reviewer blocks; ReviewEscapes is the share of
	// reviewer-passed PRs a human changed before merging, or closed.
	ReviewBlocks  int     `json:"review_blocks"`
	ReviewEscapes float64 `json:"review_escape_rate"`
	// AwaitingHours is the total and mean time items sat in awaiting_human.
	AwaitingHours     float64 `json:"awaiting_human_hours"`
	AwaitingMeanHours float64 `json:"awaiting_human_mean_hours"`
}

func rate(n, d int) float64 {
	if d == 0 {
		return -1
	}
	return float64(n) / float64(d)
}

// Compute builds the report over every item the store holds.
func Compute(ctx context.Context, st *store.Store) (*Report, error) {
	items, err := st.ListItems(ctx)
	if err != nil {
		return nil, err
	}
	r := &Report{Items: len(items)}
	var mergedCost float64
	byCx, byModel := map[string]*Group{}, map[string]*Group{}
	var implemented, firstPass, escalated, passedPRs, escapes, awaitingStays int
	for _, it := range items {
		jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		if err != nil {
			return nil, err
		}
		sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
		evs, err := st.Events(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		var cost float64
		var impls []*store.Job
		var firstReview *store.Job
		for _, j := range jobs {
			cost += j.CostUSD
			switch j.Kind {
			case "implement":
				impls = append(impls, j)
			case "review":
				if firstReview == nil && j.State == state.Succeeded {
					firstReview = j
				}
				var v verdict.Review
				if j.State == state.Succeeded && verdict.Parse([]byte(j.Verdict), &v) == nil && v.Blocks() {
					r.ReviewBlocks++
				}
			}
		}
		if len(impls) > 0 {
			implemented++
			models := map[string]bool{}
			for _, j := range impls {
				models[j.Model] = true
			}
			if len(models) > 1 {
				escalated++
			}
			var v verdict.Review
			if impls[0].State == state.Succeeded && firstReview != nil &&
				verdict.Parse([]byte(firstReview.Verdict), &v) == nil && !v.Blocks() {
				firstPass++
			}
		}
		merged, closed, changed := false, false, false
		var awaitSince time.Time
		for _, e := range evs {
			switch e.Type {
			case "pr_merged":
				merged = true
				var d struct{ Changed bool }
				_ = json.Unmarshal([]byte(e.Data), &d)
				changed = d.Changed
			case "pr_closed":
				closed = true
			case "stage":
				var d struct{ From, To string }
				_ = json.Unmarshal([]byte(e.Data), &d)
				t, _ := time.Parse(store.TimeFormat, e.At)
				if d.To == string(state.AwaitingHuman) {
					awaitSince = t
				} else if d.From == string(state.AwaitingHuman) && !awaitSince.IsZero() {
					r.AwaitingHours += t.Sub(awaitSince).Hours()
					awaitingStays++
					awaitSince = time.Time{}
				}
			}
		}
		if !awaitSince.IsZero() {
			r.AwaitingHours += time.Since(awaitSince).Hours()
			awaitingStays++
		}
		if merged || it.Stage == state.Done {
			merged = true
		}
		if merged || closed {
			passedPRs++
			if closed || changed {
				escapes++
			}
		}
		switch {
		case merged:
			r.Merged++
			mergedCost += cost
			cx := it.Complexity
			if cx == "" {
				cx = "?"
			}
			add(byCx, cx, cost)
			model := "?"
			for i := len(impls) - 1; i >= 0; i-- {
				if impls[i].State == state.Succeeded {
					model = impls[i].Harness + "/" + impls[i].Model
					break
				}
			}
			add(byModel, model, cost)
		case closed:
			r.Closed++
		case it.Stage == state.Parked:
			r.Parked++
		}
	}
	if r.Merged > 0 {
		r.CostPerMerged = mergedCost / float64(r.Merged)
	}
	r.ByComplexity, r.ByModel = groups(byCx), groups(byModel)
	r.FirstPass = rate(firstPass, implemented)
	r.Escalation = rate(escalated, implemented)
	r.ReviewEscapes = rate(escapes, passedPRs)
	if awaitingStays > 0 {
		r.AwaitingMeanHours = r.AwaitingHours / float64(awaitingStays)
	}
	return r, nil
}

func add(m map[string]*Group, k string, cost float64) {
	g := m[k]
	if g == nil {
		g = &Group{Key: k}
		m[k] = g
	}
	g.Merged++
	g.Cost += cost
}

func groups(m map[string]*Group) []Group {
	out := []Group{}
	for _, g := range m {
		g.PerPR = g.Cost / float64(g.Merged)
		out = append(out, *g)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out
}
