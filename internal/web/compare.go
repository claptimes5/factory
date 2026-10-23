package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/claptimes5/factory/internal/compare"
)

// APIComparisons are the `factory compare` reports, newest first.
type APIComparisons struct {
	Runs []APIComparison `json:"runs"`
}

// APIComparison is one issue built once per skill set.
type APIComparison struct {
	ID         string    `json:"id"`
	Ref        string    `json:"ref"`
	Title      string    `json:"title"`
	BaseSHA    string    `json:"base_sha"`
	Complexity string    `json:"complexity"`
	Started    string    `json:"started"`
	Arms       []APIArm  `json:"arms"`
	started    time.Time // for sorting
}

// APIArm is one skill set's build of the issue.
type APIArm struct {
	Skillset   string    `json:"skillset"`
	Branch     string    `json:"branch"`
	Stage      string    `json:"stage"`
	Gates      []APIGate `json:"gates"`
	Review     string    `json:"review"`
	Findings   []string  `json:"findings"`
	Implements int       `json:"implement_jobs"`
	CostUSD    float64   `json:"cost_usd"`
	Seconds    float64   `json:"seconds"`
	DiffStat   string    `json:"diff_stat"`
	Error      string    `json:"error"`
}

func (s *Server) apiCompare(w http.ResponseWriter, r *http.Request) {
	out := APIComparisons{Runs: []APIComparison{}}
	ents, _ := os.ReadDir(s.CompareDir) // none when CompareDir is ""
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(s.CompareDir, e.Name(), "report.json"))
		if err != nil {
			continue
		}
		var rep compare.Report
		if json.Unmarshal(b, &rep) != nil {
			continue
		}
		c := APIComparison{ID: e.Name(), Ref: s.ref(rep.Repo, rep.Issue), Title: rep.Title, BaseSHA: rep.BaseSHA,
			Complexity: rep.Complexity, Started: rep.Started.UTC().Format(time.RFC3339), Arms: []APIArm{}, started: rep.Started}
		for _, a := range rep.Arms {
			arm := APIArm{Skillset: a.Skillset, Branch: a.Branch, Stage: a.Stage, Gates: []APIGate{}, Review: a.Review,
				Findings: append([]string{}, a.Findings...), Implements: a.Implements, CostUSD: a.CostUSD,
				Seconds: a.Seconds, DiffStat: a.DiffStat, Error: a.Error}
			for _, g := range a.Gates {
				arm.Gates = append(arm.Gates, APIGate{Gate: g.Gate, Status: g.Status, Detail: g.Detail})
			}
			c.Arms = append(c.Arms, arm)
		}
		out.Runs = append(out.Runs, c)
	}
	sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].started.After(out.Runs[j].started) })
	writeJSON(w, http.StatusOK, out)
}
