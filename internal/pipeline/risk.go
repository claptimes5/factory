package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/diffrisk"
	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/store"
)

// Review scales with an item's risk: triage rates it low or high from the
// issue, and once the change is built the feature map can raise it to the
// riskiest feature it touches, and its diff can raise it further (see
// rateDiff). Nothing after triage lowers it.
//
//   - low: one independent reviewer; the PR says a skim is enough.
//   - medium: the review.medium_risk reviewer when set; the PR points you
//     at the features it touches.
//   - high: the review.high_risk reviewer, then a second critic (the
//     critic route, else implement.L); the PR asks you to read all of it.
//
// Merge stays human at every level, unless auto_merge lets a low (or
// medium) one merge itself. A change of a kind risk.needs_person lists
// never does, whatever its risk.

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

// labelRisk puts the item's risk label on its PR in repo and takes off
// any other.
func (p *Pipeline) labelRisk(ctx context.Context, it *store.WorkItem, repo string, pr int) {
	risk := riskOrLow(it.Risk)
	for _, r := range riskLevels {
		if r != risk {
			p.warn(p.gh(repo).UnlabelPR(ctx, pr, riskLabel(r)), "UnlabelPR")
		}
	}
	p.warn(p.gh(repo).LabelPR(ctx, pr, riskLabel(risk)), "LabelPR")
}

// riskWhy says where the item's risk came from: the feature that raised it
// when the feature map did in this run (since the latest triage), else
// triage.
func (p *Pipeline) riskWhy(ctx context.Context, it *store.WorkItem) string {
	risk := riskOrLow(it.Risk)
	jobs, _ := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	run := map[int64]bool{}
	for _, j := range jobs { // newest first
		if j.Kind == "triage" {
			break
		}
		run[j.ID] = true
	}
	if evs, err := p.Store.Events(ctx, it.ID); err == nil {
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			if e.Type != "risk_raised" || e.JobID == nil || !run[*e.JobID] {
				continue
			}
			var d struct {
				Feature, To string
				Reasons     []string
			}
			if json.Unmarshal([]byte(e.Data), &d) == nil && len(d.Reasons) > 0 {
				if d.To == risk {
					return "its diff " + andList(d.Reasons)
				}
				break
			}
			if json.Unmarshal([]byte(e.Data), &d) != nil || d.Feature == "" {
				// Saved before the event carried data: "low -> high: touches X".
				arrow, feature, ok := strings.Cut(e.Reason, ": touches ")
				_, to, _ := strings.Cut(arrow, " -> ")
				if !ok {
					break
				}
				d.Feature, d.To = feature, to
			}
			if d.To == risk {
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
		s := fmt.Sprintf("**High risk**: %s. The reviewer and then a second critic passed it, but read the whole diff before you merge", why)
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

// riskyFeatures names the high-risk features in the base branch's feature
// map with where they live, so the builder knows before it changes them.
func (p *Pipeline) riskyFeatures(ctx context.Context, repo, dir, base string) []string {
	m, err := p.loadMap(ctx, repo, dir, base)
	if err != nil || m == nil {
		p.warn(err, "loadMap") // the gates report a bad map to the reviewer
		return nil
	}
	var out []string
	more := 0
	for _, f := range m.Features {
		switch {
		case f.Risk != "high":
		case len(out) == 10:
			more++
		default:
			out = append(out, fmt.Sprintf("%s (%s)", f.Name, list(f.Paths, 5)))
		}
	}
	if more > 0 {
		out = append(out, fmt.Sprintf("and %d more in features.yaml", more))
	}
	return out
}

// textOnly says the builder may skip the test-first rule: triage rated the
// change XS (text, not behavior) and not high risk.
func textOnly(it *store.WorkItem) bool {
	return it.Complexity == "XS" && riskOrLow(it.Risk) != "high"
}

// andList joins phrases as prose: "a", "a and b", "a, b and c".
func andList(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// prCode is how a PR, or anything else that renders markdown, quotes
// what a diff names: in a code span it can't close, clipped, with no
// issue reference or mention, in case anything renders it bare.
func prCode(s string) string { return "`" + inert(clip(s, 120)) + "`" }

// rateDiff rates the built change from its diff in each repo it touches
// (see diffrisk), with the feature map's say from the blast radius, and
// raises the item's risk to that rating when it's higher. It records why,
// and saves the rating with the build job for the PR, auto-merge and a
// review resumed after a restart. A diff it can't read is saved as
// unread, which keeps the PR from merging itself. A raised risk it can't
// save is an error: review would run at the lower risk.
func (p *Pipeline) rateDiff(ctx context.Context, it *store.WorkItem, jobID int64, touched []*part, bs []partBlast) (diffrisk.Assessment, error) {
	rules := p.Cfg.RiskRules()
	a := diffrisk.Assessment{Risk: "low"}
	for _, pt := range touched {
		a.Merge(p.rateOne(ctx, pt, rules), pt.folder)
	}
	for _, pb := range bs {
		if pb.b == nil {
			continue
		}
		if r, f := features.MaxRisk(pb.b.hits); r == "medium" || r == "high" {
			a.Add(diffrisk.Reason{Kind: diffrisk.KindFeature, Risk: r, Label: pb.folder, What: "touches a " + r + "-risk feature in the feature map",
				Items: []diffrisk.Item{{Name: f}}}, rules)
		}
	}
	if from := riskOrLow(it.Risk); diffrisk.Higher(a.Risk, from) {
		a.Raised = true
		var why []string
		for _, r := range a.Reasons {
			if r.Risk == a.Risk {
				why = append(why, r.Text(prCode))
			}
		}
		to := a.Risk
		if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Risk: &to}); err != nil {
			return a, fmt.Errorf("saving the risk its diff raised: %w", err)
		}
		data, _ := json.Marshal(map[string]any{"from": from, "to": to, "reasons": why})
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "risk_raised",
			Reason: fmt.Sprintf("%s -> %s from the diff: %s", from, to, truncate(strings.Join(why, "; "), 500)), Data: string(data)}), "AddEvent")
		it.Risk = to
	}
	// Without it, auto-merge finds no rating and leaves the PR for you.
	data, _ := json.Marshal(a)
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "diff_risk", Data: string(data)}), "AddEvent")
	return a, nil
}

// rateOne rates one repo's change, read in the daemon's own repository.
func (p *Pipeline) rateOne(ctx context.Context, pt *part, rules diffrisk.Rules) diffrisk.Assessment {
	g := p.git(pt.repo)
	a := diffrisk.Assessment{Risk: "low"}
	files, err := g.Changes(ctx, pt.ws.Repo, pt.base, pt.head)
	if err != nil {
		p.warn(err, "Changes")
		a.Unreadable("couldn't list the changed files", "")
		return a
	}
	patch, err := g.Patch(ctx, pt.ws.Repo, pt.base, pt.head)
	if err != nil {
		p.warn(err, "Patch")
		a.Unreadable("couldn't read the diff", "")
		return a
	}
	at := func(sha string) func(string) (string, bool, error) {
		return func(path string) (string, bool, error) { return g.FileAt(ctx, pt.ws.Repo, sha, path) }
	}
	return diffrisk.Rate(diffrisk.Change{Files: files, Patch: patch, Before: at(pt.base), After: at(pt.head)}, rules)
}

// savedDiffRisk is the rating rateDiff saved for a build job; nil when
// none was.
func (p *Pipeline) savedDiffRisk(ctx context.Context, itemID, jobID int64) *diffrisk.Assessment {
	evs, err := p.Store.Events(ctx, itemID)
	if err != nil {
		return nil
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Type == "diff_risk" && e.JobID != nil && *e.JobID == jobID {
			var a diffrisk.Assessment
			if json.Unmarshal([]byte(e.Data), &a) != nil {
				return nil
			}
			return &a
		}
	}
	return nil
}

// noRating is why a build with no rating on record waits for a person:
// it can't be known not to need one.
var noRating = diffrisk.Reason{Kind: diffrisk.KindUnread, What: "has no rating from its diff on record", NeedsPerson: true}

// needsPerson are the reasons the build job's change waits for a person
// whatever its risk (see risk.needs_person); noRating with no rating on
// record.
func (p *Pipeline) needsPerson(ctx context.Context, itemID, jobID int64) []diffrisk.Reason {
	a := p.savedDiffRisk(ctx, itemID, jobID)
	if a == nil {
		return []diffrisk.Reason{noRating}
	}
	return a.NeedsPerson()
}

// needsPersonNote ends a notification about the build job's PR with why
// it waits for you whatever its risk: " It needs you whatever its risk:
// it adds a dependency, or changes where one comes from." It names kinds,
// never what the diff names; "" when nothing keeps it waiting.
func (p *Pipeline) needsPersonNote(ctx context.Context, itemID, jobID int64) string {
	var what []string
	for _, r := range p.needsPerson(ctx, itemID, jobID) {
		if !slices.Contains(what, r.What) {
			what = append(what, r.What)
		}
	}
	if len(what) == 0 {
		return ""
	}
	return " " + SafeText("It needs you whatever its risk: it "+andList(what)+".", 300)
}

// diffRiskSection is the PR's account of the diff's rating, under "How
// to review": each reason it found, and the ones that keep it waiting for
// a person.
func (p *Pipeline) diffRiskSection(ctx context.Context, it *store.WorkItem, jobID int64) string {
	a := p.savedDiffRisk(ctx, it.ID, jobID)
	if a == nil {
		return "**Needs a person.** The factory has no rating of this change from its diff on record, so it can't tell whether the change is of a kind that always waits for you (`risk.needs_person`). This PR won't merge itself.\n"
	}
	var b strings.Builder
	if len(a.Reasons) > 0 {
		b.WriteString("**From the diff.** Triage rated the issue before any code existed; the factory rated the change again from what was built, and the PR keeps the higher rating:\n\n")
		for _, r := range a.Reasons {
			fmt.Fprintf(&b, "- %s: it %s\n", r.Risk, r.Text(prCode))
		}
		b.WriteString("\n")
	}
	if np := a.NeedsPerson(); len(np) > 0 {
		var what []string
		for _, r := range np {
			what = append(what, r.Text(prCode))
		}
		fmt.Fprintf(&b, "**Needs a person.** It %s. Changes like that always wait for you, whatever their risk (`risk.needs_person`), so this PR won't merge itself.\n", andList(what))
	}
	return b.String()
}
