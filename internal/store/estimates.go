package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Estimate statuses: whether the item waits for a human before implement.
const (
	EstimateOff      = "off"      // no threshold for its repo
	EstimateUnder    = "under"    // at or under the threshold
	EstimateExempt   = "exempt"   // over it, but a main health fix goes first
	EstimateWaiting  = "waiting"  // over it: waits for a human's OK
	EstimateApproved = "approved" // over it, and someone said go
	EstimateParked   = "parked"   // parked while it waited: still needs an OK
)

// EstimateNeedsOK says whether an estimate in this status keeps the item
// from being built until a human approves it.
func EstimateNeedsOK(status string) bool {
	return status == EstimateWaiting || status == EstimateParked
}

// Estimate is what triage's cost estimate said for an item. The billed
// figures leave out work on a subscription plan; the totals hold all of
// it, at the cost each run reported.
type Estimate struct {
	ItemID         int64
	TriageJob      int64
	IssueHash      string
	Size           string
	MedianUSD      float64
	P75USD         float64
	TotalMedianUSD float64
	TotalP75USD    float64
	Basis          string // repo | all | default
	Samples        int
	Detail         string
	ThresholdUSD   float64
	Status         string
	ApprovedBy     string
	ApprovedUSD    float64
	CreatedAt      string
	DecidedAt      string
	// Announced: the label, comment and notification of a wait went out,
	// with any factory:go from before taken off.
	Announced bool
}

const estimateCols = `item_id, triage_job, issue_hash, size, median_usd, p75_usd, total_median_usd, total_p75_usd, basis, samples,
	detail, threshold_usd, status, approved_by, approved_usd, created_at, decided_at, announced`

func scanEstimate(r scanner) (*Estimate, error) {
	var e Estimate
	err := r.Scan(&e.ItemID, &e.TriageJob, &e.IssueHash, &e.Size, &e.MedianUSD, &e.P75USD, &e.TotalMedianUSD, &e.TotalP75USD, &e.Basis, &e.Samples,
		&e.Detail, &e.ThresholdUSD, &e.Status, &e.ApprovedBy, &e.ApprovedUSD, &e.CreatedAt, &e.DecidedAt, &e.Announced)
	return &e, err
}

// SetEstimate records an item's estimate, replacing the one before.
func (s *Store) SetEstimate(ctx context.Context, e Estimate) error {
	if e.CreatedAt == "" {
		e.CreatedAt = Now()
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO estimates(`+estimateCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ItemID, e.TriageJob, e.IssueHash, e.Size, e.MedianUSD, e.P75USD, e.TotalMedianUSD, e.TotalP75USD, e.Basis, e.Samples,
			e.Detail, e.ThresholdUSD, e.Status, e.ApprovedBy, e.ApprovedUSD, e.CreatedAt, e.DecidedAt, e.Announced)
		return err
	})
}

// ItemEstimate is an item's estimate, or nil when it has none.
func (s *Store) ItemEstimate(ctx context.Context, itemID int64) (*Estimate, error) {
	e, err := scanEstimate(s.read.QueryRowContext(ctx, `SELECT `+estimateCols+` FROM estimates WHERE item_id = ?`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// Estimates lists every item's estimate, by item.
func (s *Store) Estimates(ctx context.Context) ([]*Estimate, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+estimateCols+` FROM estimates ORDER BY item_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Estimate
	for rows.Next() {
		e, err := scanEstimate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ApproveEstimate records who said go on a waiting estimate. It reports
// false when the estimate wasn't waiting.
func (s *Store) ApproveEstimate(ctx context.Context, itemID int64, by string) (bool, error) {
	var ok bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE estimates SET status = ?, approved_by = ?, approved_usd = p75_usd, decided_at = ? WHERE item_id = ? AND status = ?`,
			EstimateApproved, by, Now(), itemID, EstimateWaiting)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	return ok, err
}

// SetEstimateStatus moves an estimate from one status to another, and
// reports whether it was in from.
func (s *Store) SetEstimateStatus(ctx context.Context, itemID int64, from, to string) (bool, error) {
	var ok bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE estimates SET status = ? WHERE item_id = ? AND status = ?`, to, itemID, from)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	return ok, err
}

// SetEstimateAnnounced records that a wait was announced.
func (s *Store) SetEstimateAnnounced(ctx context.Context, itemID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE estimates SET announced = 1 WHERE item_id = ?`, itemID)
		return err
	})
}

// CostPart is what one kind of job on one harness cost an item.
type CostPart struct {
	Kind    string // implement | review | critic
	Harness string
	USD     float64
}

// ItemCost is what building one item cost in its latest cycle (since its
// last triage), by stage: implement (every attempt and escalation, gates
// included), review and critic.
type ItemCost struct {
	ItemID     int64
	Repo       string
	Size       string
	Stage      string
	Implement  float64
	Review     float64
	Critic     float64
	Parts      []CostPart
	FinishedAt string // its last job's
}

// Total is the item's whole build cost.
func (c ItemCost) Total() float64 { return c.Implement + c.Review + c.Critic }

// ItemCosts lists the build cost of every item whose latest cycle got at
// least one implement job and that has nothing queued or running: done,
// waiting for a review, or parked along the way. Triage isn't counted, nor
// is anything before the item's last triage: a factory:retry starts over,
// and the size it has now is the one that cycle was rated.
func (s *Store) ItemCosts(ctx context.Context) ([]ItemCost, error) { return s.itemCosts(ctx, 0) }

// ItemCostOf is ItemCosts for one item: nil when it doesn't qualify.
func (s *Store) ItemCostOf(ctx context.Context, itemID int64) (*ItemCost, error) {
	cs, err := s.itemCosts(ctx, itemID)
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	return &cs[0], nil
}

func (s *Store) itemCosts(ctx context.Context, itemID int64) ([]ItemCost, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT w.id, w.repo, COALESCE(w.complexity,''), w.stage, j.kind, j.harness,
		COALESCE(SUM(j.cost_usd), 0), COALESCE(MAX(j.finished_at), '')
		FROM work_items w JOIN jobs j ON j.item_id = w.id
		WHERE (? = 0 OR w.id = ?) AND w.stage IN ('done', 'awaiting_human', 'parked') AND COALESCE(w.complexity,'') != ''
		  AND j.kind IN ('implement', 'review', 'critic')
		  AND j.id > COALESCE((SELECT MAX(t.id) FROM jobs t WHERE t.item_id = w.id AND t.kind = 'triage'), 0)
		  AND NOT EXISTS (SELECT 1 FROM jobs a WHERE a.item_id = w.id AND a.state IN ('queued', 'running'))
		GROUP BY w.id, j.kind, j.harness
		ORDER BY w.id, j.kind, j.harness`, itemID, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemCost
	for rows.Next() {
		var c ItemCost
		var p CostPart
		var fin string
		if err := rows.Scan(&c.ItemID, &c.Repo, &c.Size, &c.Stage, &p.Kind, &p.Harness, &p.USD, &fin); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ItemID != c.ItemID {
			out = append(out, c)
		}
		cur := &out[len(out)-1]
		cur.Parts = append(cur.Parts, p)
		switch p.Kind {
		case "implement":
			cur.Implement += p.USD
		case "review", KindVerify, KindSecurity: // the verifier and the security pass are part of reviewing
			cur.Review += p.USD
		case "critic", KindParallelCritic:
			cur.Critic += p.USD
		}
		cur.FinishedAt = max(cur.FinishedAt, fin)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Only items whose cycle built something.
	keep := out[:0]
	for _, c := range out {
		for _, p := range c.Parts {
			if p.Kind == "implement" {
				keep = append(keep, c)
				break
			}
		}
	}
	return keep, nil
}

// Range is how the estimate reads in a line: "$1.80–$3.10" (median to
// p75), with "+ plan usage" when part of the work draws on a subscription,
// or "on your plan" when all of it does.
func (e *Estimate) Range() string {
	billed := money(e.MedianUSD)
	if e.P75USD-e.MedianUSD >= 0.005 {
		billed += "–" + money(e.P75USD)
	}
	plan := e.TotalP75USD-e.P75USD >= 0.005
	switch {
	case e.P75USD < 0.005 && plan:
		return "on your plan"
	case plan:
		return billed + " + plan usage"
	}
	return billed
}

func money(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 2, 64) }
