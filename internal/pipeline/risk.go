package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/claptimes5/factory/internal/store"
)

// Review scales with an item's risk: triage rates it low or high, and the
// feature map can raise it to the riskiest feature the change touches.
//
//   - low: one independent reviewer; the PR says a skim is enough.
//   - medium: the review.medium_risk reviewer when set; the PR points you
//     at the features it touches.
//   - high: the review.high_risk reviewer, then a second, independent
//     Claude critic; the PR asks you to read all of it.
//
// Merge stays human at every level.

// riskLabel is the PR label for a risk level.
func riskLabel(risk string) string { return "factory:risk-" + risk }

// riskLevels are the levels, lowest first.
var riskLevels = []string{"low", "medium", "high"}

// riskOrLow is the item's risk, low when triage didn't set one.
func riskOrLow(risk string) string {
	for _, r := range riskLevels {
		if r == risk {
			return r
		}
	}
	return "low"
}

// labelRisk puts the item's risk label on its PR and takes off any other.
func (p *Pipeline) labelRisk(ctx context.Context, it *store.WorkItem, pr int) {
	risk := riskOrLow(it.Risk)
	for _, r := range riskLevels {
		if r != risk {
			p.warn(p.gh(it.Repo).UnlabelPR(ctx, pr, riskLabel(r)), "UnlabelPR")
		}
	}
	p.warn(p.gh(it.Repo).LabelPR(ctx, pr, riskLabel(risk)), "LabelPR")
}

// riskWhy says where the item's risk came from: the feature that raised it
// when the feature map did, else triage.
func (p *Pipeline) riskWhy(ctx context.Context, it *store.WorkItem) string {
	risk := riskOrLow(it.Risk)
	if evs, err := p.Store.Events(ctx, it.ID); err == nil {
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Type != "risk_raised" {
				continue
			}
			var d struct{ Feature, To string }
			if json.Unmarshal([]byte(evs[i].Data), &d) == nil && d.To == risk && d.Feature != "" {
				return fmt.Sprintf("it touches %s, a %s-risk feature in the feature map", d.Feature, risk)
			}
			break
		}
	}
	if it.Risk == "" {
		return "triage didn't rate it"
	}
	return "triage rated it " + risk
}

// reviewGuide is the PR's "How to review" section: how much of your
// attention the change needs, and why.
func (p *Pipeline) reviewGuide(ctx context.Context, it *store.WorkItem, features bool) string {
	why := p.riskWhy(ctx, it)
	switch riskOrLow(it.Risk) {
	case "high":
		s := fmt.Sprintf("**High risk**: %s. Two independent reviewers passed it, the reviewer and a Claude critic, but read the whole diff before you merge", why)
		if features {
			s += ", and check the features under Blast radius by hand"
		}
		return s + ".\n"
	case "medium":
		s := fmt.Sprintf("**Medium risk**: %s. An independent reviewer passed it. Read the changes", why)
		if features {
			s += " to the features under Blast radius"
		}
		return s + " closely; the rest can be skimmed.\n"
	}
	return fmt.Sprintf("**Low risk**: %s. The gates and an independent reviewer passed it; a skim of the diff is enough.\n", why)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
