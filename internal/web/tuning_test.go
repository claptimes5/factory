package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/store"
)

func addProposal(t *testing.T, s *Server, changes ...store.ProposedChange) int64 {
	t.Helper()
	id, err := s.Store.AddProposal(context.Background(), store.Proposal{
		Summary: "The daily cap binds.", Changes: changes,
		Dropped: []store.ProposedChange{{Path: []string{"runner", "kind"}, Value: "local", Why: "faster", Reason: "runner.kind isn't a setting tuning may change"}},
		Ideas:   []string{"Run go vet first."}, Diff: "--- a/factory.yaml\n+++ b/factory.yaml\n", PRURL: "https://github.com/o/r/pull/9",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTuningGetApplyDismiss(t *testing.T) {
	base, path, s, _ := settingsSetup(t, "127.0.0.1:8080")
	var tu APITuning
	getJSON(t, base+"/api/v1/settings/tuning", &tu)
	if tu.Proposal != nil {
		t.Fatalf("no proposal yet, got %+v", tu.Proposal)
	}
	id := addProposal(t, s, store.ProposedChange{Path: []string{"budgets", "daily_total"}, Value: 75.0, Why: "the cap bound on 3 days"})
	getJSON(t, base+"/api/v1/settings/tuning", &tu)
	p := tu.Proposal
	if p == nil || p.ID != id || p.Status != "pending" || len(p.Changes) != 1 || p.Changes[0].Why == "" || len(p.Dropped) != 1 ||
		p.Dropped[0].Reason == "" || len(p.Ideas) != 1 || p.PRURL == "" || p.Diff == "" {
		t.Fatalf("proposal = %+v", p)
	}

	// A save since the proposal is kept: the changes are applied to the
	// file as it is now, not as the stored diff had it.
	var st APISettings
	getJSON(t, base+"/api/v1/settings", &st)
	if code, body := postJSON(t, base+"/api/v1/settings", "", `{"version":"`+st.Version+`","changes":[{"path":["budgets","concurrent_jobs"],"value":4}]}`); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	apply := fmt.Sprintf("%s/api/v1/settings/tuning/%d/apply", base, id)
	if code, body := postJSON(t, apply, "", `{"version":"`+st.Version+`"}`); code != http.StatusConflict {
		t.Fatalf("apply over a stale version = %d %s", code, body)
	}
	if code, _ := postJSON(t, apply, "https://evil.example", `{}`); code != http.StatusForbidden {
		t.Fatalf("a cross-origin apply must be refused, got %d", code)
	}
	getJSON(t, base+"/api/v1/settings", &st)
	code, body := postJSON(t, apply, "", `{"version":"`+st.Version+`"}`)
	if code != 200 {
		t.Fatalf("apply = %d %s", code, body)
	}
	var saved APISettings
	if err := json.Unmarshal([]byte(body), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Changed || field(saved, "budgets", "daily_total").Value != 75.0 || field(saved, "budgets", "concurrent_jobs").Value != 4.0 {
		t.Fatalf("saved = %+v", saved)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "  daily_total: 75\n") {
		t.Fatalf("file:\n%s", b)
	}
	getJSON(t, base+"/api/v1/settings/tuning", &tu)
	if tu.Proposal != nil {
		t.Fatalf("an applied proposal is still pending: %+v", tu.Proposal)
	}
	if code, body := postJSON(t, apply, "", `{"version":"`+saved.Version+`"}`); code != http.StatusConflict || !strings.Contains(body, "already applied") {
		t.Fatalf("second apply = %d %s", code, body)
	}

	// Dismissing changes nothing in the file.
	before, _ := os.ReadFile(path)
	id = addProposal(t, s, store.ProposedChange{Path: []string{"budgets", "daily_total"}, Value: 90.0, Why: "x"})
	code, body = postJSON(t, fmt.Sprintf("%s/api/v1/settings/tuning/%d/dismiss", base, id), "", `{}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("dismiss = %d %s", code, body)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("dismiss changed the file")
	}
	if p, _ := s.Store.GetProposal(context.Background(), id); p.Status != store.ProposalDismissed || p.DecidedBy != "human" {
		t.Fatalf("dismissed = %+v", p)
	}
	if code, _ := postJSON(t, base+"/api/v1/settings/tuning/999/dismiss", "", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown proposal = %d", code)
	}
}

func TestTuningApplyRefusedWithoutAPassword(t *testing.T) {
	base, path, s, _ := settingsSetup(t, "100.64.0.7:8080")
	orig, _ := os.ReadFile(path)
	id := addProposal(t, s, store.ProposedChange{Path: []string{"budgets", "daily_total"}, Value: 75.0, Why: "x"})
	for _, act := range []string{"apply", "dismiss"} {
		if code, body := postJSON(t, fmt.Sprintf("%s/api/v1/settings/tuning/%d/%s", base, id, act), "", `{}`); code != http.StatusForbidden || !strings.Contains(body, "FACTORY_WEB_PASSWORD") {
			t.Fatalf("%s = %d %s", act, code, body)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != string(orig) {
		t.Fatal("the file changed")
	}
	if p, _ := s.Store.PendingProposal(context.Background()); p == nil || p.ID != id {
		t.Fatalf("pending = %+v", p)
	}
}

// Apply takes the changes you picked, and the card reads each against the
// file as it is now: the diff is fresh, and a change already made says so.
func TestTuningApplyPicked(t *testing.T) {
	base, path, s, _ := settingsSetup(t, "127.0.0.1:8080")
	id := addProposal(t, s,
		store.ProposedChange{Path: []string{"budgets", "daily_total"}, Value: 75.0, Why: "a"},
		store.ProposedChange{Path: []string{"budgets", "concurrent_jobs"}, Value: 5.0, Why: "b"})
	var tu APITuning
	getJSON(t, base+"/api/v1/settings/tuning", &tu)
	if p := tu.Proposal; p.Changes[0].Now != "" || p.Changes[1].Now != "" || !strings.Contains(p.Diff, "+  daily_total: 75") || !strings.Contains(p.Diff, "+  concurrent_jobs: 5") {
		t.Fatalf("proposal = %+v", p)
	}
	var st APISettings
	getJSON(t, base+"/api/v1/settings", &st)
	apply := fmt.Sprintf("%s/api/v1/settings/tuning/%d/apply", base, id)
	for body, want := range map[string]string{`[]`: "pick at least one", `[2]`: "no change 2", `[0,0]`: "no change 0"} {
		if code, out := postJSON(t, apply, "", `{"version":"`+st.Version+`","changes":`+body+`}`); code != 400 || !strings.Contains(out, want) {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
	if code, out := postJSON(t, apply, "", `{"version":"`+st.Version+`","changes":[1]}`); code != 200 {
		t.Fatalf("apply = %d %s", code, out)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "concurrent_jobs: 5") || strings.Contains(string(b), "daily_total: 75") {
		t.Fatalf("file:\n%s", b)
	}

	// A new proposal for what the file already says: no diff, and it says so.
	addProposal(t, s, store.ProposedChange{Path: []string{"budgets", "concurrent_jobs"}, Value: 5.0, Why: "b"})
	getJSON(t, base+"/api/v1/settings/tuning", &tu)
	if p := tu.Proposal; p.Changes[0].Now != "factory.yaml already says that" || p.Diff != "" {
		t.Fatalf("proposal = %+v", p)
	}
	getJSON(t, base+"/api/v1/settings", &st)
	if code, out := postJSON(t, fmt.Sprintf("%s/api/v1/settings/tuning/%d/apply", base, tu.Proposal.ID), "", `{"version":"`+st.Version+`"}`); code != 400 || !strings.Contains(out, "already makes") {
		t.Fatalf("no-op apply = %d %s", code, out)
	}
}
