package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/store"
)

// Trust level 6: the issues the factory files from its own findings start
// without a label when the repo allows it (config.SelfStart), within a
// daily cap and budget, and merge themselves under its auto_merge. Below
// it they wait for a person, to label or to merge
// (docs/plans/trust-level-6.md).

// OrchestratorApprover is who approves the issues the orchestrator opens
// on its own schedule.
const OrchestratorApprover = "the factory orchestrator"

// SelfApprover is who approves an item the factory started from source.
func SelfApprover(source string) string {
	switch source {
	case "main_health":
		return HealthApprover
	case "orchestrator":
		return OrchestratorApprover
	}
	return "the factory (" + source + ")"
}

// selfApprovers are every self-started item's approver.
func selfApprovers() []string {
	out := []string{HealthApprover, OrchestratorApprover}
	for _, src := range []string{SourceDependabot, SourceTODOs, SourceSentry, SourceHunt} {
		out = append(out, SelfApprover(src))
	}
	return out
}

// SelfSource is the source of an item the factory started on its own,
// from who approved it; "" for one a person approved (the chat's issues
// are a person's: they asked for them).
func SelfSource(approvedBy string) string {
	switch {
	case approvedBy == HealthApprover:
		return "main_health"
	case approvedBy == OrchestratorApprover:
		return "orchestrator"
	case strings.HasPrefix(approvedBy, "the factory (") && strings.HasSuffix(approvedBy, ")"):
		return strings.TrimSuffix(strings.TrimPrefix(approvedBy, "the factory ("), ")")
	}
	return ""
}

// selfStarted is the source of an item the factory started on its own,
// or "". With no approval readable it's treated as self-started: then it
// waits for a person, the safe side.
func (p *Pipeline) selfStarted(ctx context.Context, it *store.WorkItem) string {
	a, err := p.Store.ItemApproval(ctx, it.ID)
	if err != nil {
		return "unknown"
	}
	if a == nil {
		return ""
	}
	return SelfSource(a.ApprovedBy)
}

// mergesItself says whether a self-started item may merge itself: only
// when its repo is at level 6 for its source.
func (p *Pipeline) mergesItself(ctx context.Context, it *store.WorkItem) bool {
	src := p.selfStarted(ctx, it)
	return src == "" || p.repoConf(it.Repo).SelfStart.Allows(src)
}

func startOfDay() string { return time.Now().UTC().Format("2006-01-02") + "T00:00:00" }

// selfStartRoom says why no more work may start on its own today in repo,
// or "": a tripped breaker, or the daily cap and budget for self-started
// work, which its workspace's repos share.
func (p *Pipeline) selfStartRoom(ctx context.Context, repo string) (string, error) {
	if p.breakerTripped(ctx, repo) {
		return "auto-merge is off after a revert or a broken main, which stops it starting its own work until it's reset", nil
	}
	ss := p.repoConf(repo).SelfStart
	since := startOfDay()
	n, spent := 0, 0.0
	for _, r := range p.sameWorkspace(repo) {
		c, err := p.Store.CountEventsFor(ctx, "self_started", r, since)
		if err != nil {
			return "", err
		}
		v, err := p.Store.SpendByApprovers(ctx, r, since, selfApprovers())
		if err != nil {
			return "", err
		}
		n, spent = n+c, spent+v
	}
	if n >= ss.DailyCap() {
		return fmt.Sprintf("%d started on their own today (self_start.daily)", n), nil
	}
	if b := ss.BudgetUSD(p.Cfg.Budgets.DailyTotal); spent >= b {
		return fmt.Sprintf("$%.2f of its $%.2f a day for work it starts itself is spent (self_start.budget)", spent, b), nil
	}
	return "", nil
}

// selfStart starts issue n, which the factory filed from source, when the
// repo is at level 6 for that source and today's cap and budget allow:
// approved by the factory and queued for triage. It says whether it did,
// and why not when the repo allows the source but today doesn't.
func (p *Pipeline) selfStart(ctx context.Context, repo string, n int, title, source string) (bool, string, error) {
	if !p.repoConf(repo).SelfStart.Allows(source) {
		return false, "", nil
	}
	if why, err := p.selfStartRoom(ctx, repo); err != nil || why != "" {
		return false, why, err
	}
	it, created, err := p.Store.UpsertItem(ctx, repo, n, title)
	if err != nil || !created {
		return false, "", err
	}
	is, err := p.gh(repo).GetIssue(ctx, n)
	if err == nil {
		err = p.approve(ctx, it, *is, SelfApprover(source))
	}
	p.warn(err, "approve") // should this fail, it parks at triage
	if err := p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "self_started", Reason: repo, Data: fmt.Sprintf(`{"source":%q}`, source)}); err != nil {
		return false, "", err
	}
	return true, "", p.enqueueTriage(ctx, it, 1, "")
}
