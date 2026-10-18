// Package metrics derives the factory's quality numbers from the store.
// The number that matters is cost per merged PR; the rest explains it.
package metrics

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
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
	// Weekly is the same numbers per ISO week (Monday, UTC), oldest first.
	Weekly []Week `json:"weekly"`
}

// Week is one week of the trend lines. Merged items count in the week
// they merged; first-pass and escalation count in the week the item's
// first implement job was queued.
type Week struct {
	Start         string  `json:"start"` // YYYY-MM-DD, a Monday
	Merged        int     `json:"merged"`
	Cost          float64 `json:"cost_usd"` // spend on the items merged this week
	CostPerMerged float64 `json:"cost_per_merged_usd"`
	Implemented   int     `json:"implemented"`
	FirstPass     float64 `json:"first_pass_rate"`
	Escalation    float64 `json:"escalation_rate"`

	firstPass, escalated int
}

// weekOf returns the Monday that starts ts's week, or "" when ts is not a time.
func weekOf(ts string) string {
	t, err := time.Parse(store.TimeFormat, ts)
	if err != nil {
		return ""
	}
	t = t.UTC()
	t = t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	return t.Format("2006-01-02")
}

func rate(n, d int) float64 {
	if d == 0 {
		return -1
	}
	return float64(n) / float64(d)
}

// Slice is how the builds of one size, or that started on one model, went.
type Slice struct {
	Key           string  `json:"key"`
	Implemented   int     `json:"implemented"`
	FirstPass     float64 `json:"first_pass_rate"`
	Escalation    float64 `json:"escalation_rate"`
	Merged        int     `json:"merged"`
	ReviewEscapes float64 `json:"review_escape_rate"`
	CostPerMerged float64 `json:"cost_per_merged_usd"`

	firstPass, escalated, passed, escapes int
	cost                                  float64
}

// Breakdown is the report's build numbers per route, which the tuning
// loop reads to propose routing changes.
type Breakdown struct {
	Implemented  int     `json:"implemented"`
	ByComplexity []Slice `json:"by_complexity"`
	ByModel      []Slice `json:"by_model"` // the model an item's first build ran on
	ParkReasons  []Count `json:"park_reasons"`
}

// Count is how many items parked for one reason.
type Count struct {
	Reason string `json:"reason"`
	Items  int    `json:"items"`
}

// Compute builds the report over every item the store holds.
func Compute(ctx context.Context, st *store.Store) (*Report, error) {
	r, _, err := Period(ctx, st, time.Time{}, time.Time{})
	return r, err
}

// Period builds the report and its breakdown over the items whose first
// build was queued in [from, to), or that were filed then when never
// built. A zero time leaves that end open.
func Period(ctx context.Context, st *store.Store, from, to time.Time) (*Report, *Breakdown, error) {
	all, err := st.ListItems(ctx)
	if err != nil {
		return nil, nil, err
	}
	in := func(ts string) bool {
		t, err := time.Parse(store.TimeFormat, ts)
		if err != nil {
			return from.IsZero() && to.IsZero()
		}
		return (from.IsZero() || !t.Before(from)) && (to.IsZero() || t.Before(to))
	}
	r := &Report{}
	b := &Breakdown{}
	byCxSlice, byModelSlice := map[string]*Slice{}, map[string]*Slice{}
	parks := map[string]int{}
	var mergedCost float64
	byCx, byModel := map[string]*Group{}, map[string]*Group{}
	var implemented, firstPass, escalated, passedPRs, escapes, awaitingStays int
	weeks := map[string]*Week{}
	week := func(ts string) *Week {
		k := weekOf(ts)
		if k == "" {
			return nil
		}
		if weeks[k] == nil {
			weeks[k] = &Week{Start: k}
		}
		return weeks[k]
	}
	for _, it := range all {
		jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		if err != nil {
			return nil, nil, err
		}
		sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
		started := it.CreatedAt
		for _, j := range jobs {
			if j.Kind == "implement" {
				started = j.QueuedAt
				break
			}
		}
		if !in(started) {
			continue
		}
		r.Items++
		evs, err := st.Events(ctx, it.ID)
		if err != nil {
			return nil, nil, err
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
		cx := it.Complexity
		if cx == "" {
			cx = "?"
		}
		// The item's slices: its size, and the model its first build ran on.
		var slices []*Slice
		if len(impls) > 0 {
			implemented++
			slices = []*Slice{slice(byCxSlice, cx), slice(byModelSlice, impls[0].Harness+"/"+impls[0].Model)}
			models := map[string]bool{}
			for _, j := range impls {
				models[j.Model] = true
			}
			w := week(impls[0].QueuedAt)
			if w != nil {
				w.Implemented++
			}
			for _, sl := range slices {
				sl.Implemented++
			}
			if len(models) > 1 {
				escalated++
				if w != nil {
					w.escalated++
				}
				for _, sl := range slices {
					sl.escalated++
				}
			}
			var v verdict.Review
			if impls[0].State == state.Succeeded && firstReview != nil &&
				verdict.Parse([]byte(firstReview.Verdict), &v) == nil && !v.Blocks() {
				firstPass++
				if w != nil {
					w.firstPass++
				}
				for _, sl := range slices {
					sl.firstPass++
				}
			}
		}
		merged, closed, changed := false, false, false
		mergedAt := it.UpdatedAt
		var awaitSince time.Time
		for _, e := range evs {
			switch e.Type {
			case "pr_merged":
				merged = true
				mergedAt = e.At
				var d struct{ Changed bool }
				_ = json.Unmarshal([]byte(e.Data), &d)
				changed = d.Changed
			case "pr_closed":
				closed = true
			case "stage":
				var d struct{ From, To string }
				_ = json.Unmarshal([]byte(e.Data), &d)
				t, _ := time.Parse(store.TimeFormat, e.At)
				if d.To == string(state.Parked) {
					parks[parkReason(e.Reason)]++
				}
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
			for _, sl := range slices {
				sl.passed++
				if closed || changed {
					sl.escapes++
				}
			}
		}
		switch {
		case merged:
			r.Merged++
			mergedCost += cost
			if w := week(mergedAt); w != nil {
				w.Merged++
				w.Cost += cost
			}
			for _, sl := range slices {
				sl.Merged++
				sl.cost += cost
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
	r.Weekly = []Week{}
	for _, w := range weeks {
		if w.Merged > 0 {
			w.CostPerMerged = w.Cost / float64(w.Merged)
		}
		w.FirstPass = rate(w.firstPass, w.Implemented)
		w.Escalation = rate(w.escalated, w.Implemented)
		r.Weekly = append(r.Weekly, *w)
	}
	sort.Slice(r.Weekly, func(a, b int) bool { return r.Weekly[a].Start < r.Weekly[b].Start })
	b.Implemented = implemented
	b.ByComplexity, b.ByModel = sliced(byCxSlice), sliced(byModelSlice)
	b.ParkReasons = []Count{}
	for k, n := range parks {
		b.ParkReasons = append(b.ParkReasons, Count{k, n})
	}
	sort.Slice(b.ParkReasons, func(i, j int) bool {
		x, y := b.ParkReasons[i], b.ParkReasons[j]
		return x.Items > y.Items || x.Items == y.Items && x.Reason < y.Reason
	})
	if len(b.ParkReasons) > 8 {
		b.ParkReasons = b.ParkReasons[:8]
	}
	return r, b, nil
}

func slice(m map[string]*Slice, k string) *Slice {
	if m[k] == nil {
		m[k] = &Slice{Key: k}
	}
	return m[k]
}

func sliced(m map[string]*Slice) []Slice {
	out := []Slice{}
	for _, s := range m {
		s.FirstPass = rate(s.firstPass, s.Implemented)
		s.Escalation = rate(s.escalated, s.Implemented)
		s.ReviewEscapes = rate(s.escapes, s.passed)
		if s.Merged > 0 {
			s.CostPerMerged = s.cost / float64(s.Merged)
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out
}

var digits = regexp.MustCompile(`[0-9]+`)

// parkReason is a park's reason as a group: its first line, with numbers
// (issue, PR and job numbers, dollars) made alike.
func parkReason(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = digits.ReplaceAllString(s, "N")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
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
