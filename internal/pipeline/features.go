package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/store"
)

// blastRadius is what a change touches according to the feature map on
// the base branch. The map is read from base, not from the change, so a
// builder can't shrink its own blast radius or lower a feature's risk.
type blastRadius struct {
	hits     []features.Hit
	unmapped []string
	checks   map[string]checkResult // by feature name
	note     string                 // why there is no map to use
}

// loadMap reads the feature map at sha; nil when the repo has none.
func (p *Pipeline) loadMap(ctx context.Context, repo, dir, sha string) (*features.Map, error) {
	for _, name := range features.Files {
		body, ok, err := p.git(repo).FileAt(ctx, dir, sha, name)
		if err != nil {
			return nil, err
		}
		if ok {
			return features.Parse([]byte(body))
		}
	}
	return nil, nil
}

// featureChecks finds the features the change touches and runs the check
// of each one that has one, as a gate named feature:<name>.
func (p *Pipeline) featureChecks(ctx context.Context, jobID int64, pt *part, base, head, gateDir string, snap snapshot) ([]gates.Result, *blastRadius) {
	ws := pt.ws
	m, err := p.loadMap(ctx, pt.repo, ws.Repo, base)
	switch {
	case err != nil:
		return nil, &blastRadius{note: "The feature map on the base branch can't be used: " + err.Error()}
	case m == nil:
		return nil, nil
	}
	changed, err := p.git(pt.repo).ChangedFiles(ctx, ws.Repo, base, head)
	if err != nil {
		return nil, &blastRadius{note: "Couldn't list the changed files: " + err.Error()}
	}
	b := &blastRadius{checks: map[string]checkResult{}}
	b.hits, b.unmapped = m.Touched(changed)
	var results []gates.Result
	ran := map[string]checkResult{} // features often share a check
	baseDir := ""
	for _, h := range b.hits {
		cmd := h.Feature.Check
		if cmd == "" {
			continue
		}
		r, done := ran[cmd]
		if !done {
			r = p.featureCheck(ctx, jobID, pt.repo, gateDir, cmd, snap)
			if r.Status == gates.Fail && r.code > 0 {
				// A check that fails the same way on the base branch isn't
				// this change's to fix, and the builder can't change the
				// map, so it would block every change to the feature. The
				// base gets the same install as the change, so missing
				// dependencies don't pass for a red base.
				if baseDir == "" {
					baseDir = p.baseExport(ctx, jobID, pt.repo, ws.Repo, base, pt.sfx())
				}
				if baseDir != "" && baseDir != "-" {
					if code, _, err := p.gateExec(ctx, jobID, baseDir, cmd, gateRun{repo: pt.repo}); err == nil && code == r.code {
						r = checkResult{Result: gates.Result{Status: gates.Pass, Detail: "fails the same way on the base branch, so it isn't counted against this change\n\n" + r.Detail}, baseRed: true}
					}
				}
			}
			ran[cmd] = r
		}
		r.Gate = "feature:" + h.Feature.Name
		b.checks[h.Feature.Name] = r
		results = append(results, r.Result)
	}
	return results, b
}

// baseExport exports the base commit and installs its dependencies, for
// rerunning a failing check there; "-" when that can't be done.
func (p *Pipeline) baseExport(ctx context.Context, jobID int64, repo, repoDir, base, sfx string) string {
	dir := filepath.Join(p.jobDir(jobID), "feature-base"+sfx)
	if err := p.git(repo).Export(ctx, repoDir, base, dir); err != nil {
		return "-"
	}
	if install := p.repoConf(repo).Gates.Install; install != "" {
		if code, _, err := p.gateExec(ctx, jobID, dir, install, gateRun{repo: repo, install: true}); err != nil || code != 0 {
			return "-"
		}
	}
	return dir
}

// featureCheck runs one feature's check on the export under test.
func (p *Pipeline) featureCheck(ctx context.Context, jobID int64, repo, gateDir, cmd string, snap snapshot) checkResult {
	r := checkResult{Result: gates.Result{Status: gates.Pass}}
	code, out, err := p.gateExec(ctx, jobID, gateDir, cmd, gateRun{repo: repo})
	r.code = code
	switch {
	case err != nil:
		r.Status, r.Detail = gates.Fail, "could not run: "+err.Error()
	case code != 0:
		r.Status, r.Detail = gates.Fail, fmt.Sprintf("exit %d\n%s", code, tail(out, 3000))
	default:
		if note := snap.tamperNote(gateDir); note != "" {
			r.Status, r.Detail, r.code = gates.Fail, note, 0
		}
	}
	if err != nil {
		r.code = 0
	}
	return r
}

// checkResult is a feature check's gate result, and whether it fails on
// the base branch as well.
type checkResult struct {
	gates.Result
	code    int // the exit code; 0 if it couldn't run or was tampered with
	baseRed bool
}

// partBlast is one repo's blast radius; folder is "" outside a workspace.
type partBlast struct {
	folder string
	b      *blastRadius
}

// applyBlast raises the item's risk to the riskiest feature the change
// touches, so review scales with it, and saves the blast radius with the
// job for a review resumed after a restart. It returns the markdown: in a
// workspace, a section per repo.
func (p *Pipeline) applyBlast(ctx context.Context, it *store.WorkItem, jobID int64, bs []partBlast) string {
	var md strings.Builder
	found, unread := false, false
	risk, feature := "", ""
	for _, pb := range bs {
		if pb.b == nil {
			continue
		}
		unread = unread || pb.b.note != ""
		found = true
		if r, f := features.MaxRisk(pb.b.hits); features.Higher(r, risk) {
			risk, feature = r, f
			if pb.folder != "" {
				feature = pb.folder + ": " + f
			}
		}
		if pb.folder != "" {
			fmt.Fprintf(&md, "### %s\n\n", pb.folder)
		}
		md.WriteString(pb.b.markdown())
		if pb.folder != "" {
			md.WriteString("\n")
		}
	}
	if !found {
		// No feature map: recorded too, so auto-merge can tell this from a
		// map it couldn't read.
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "blast_radius", Data: `{"markdown":"","unread":false}`}), "AddEvent")
		return ""
	}
	if features.Higher(risk, it.Risk) {
		p.warn(p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Risk: &risk}), "UpdateItem")
		data, _ := json.Marshal(map[string]string{"feature": feature, "from": it.Risk, "to": risk})
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "risk_raised",
			Reason: fmt.Sprintf("%s -> %s: touches %s", it.Risk, risk, feature), Data: string(data)}), "AddEvent")
		it.Risk = risk
	}
	out := md.String()
	data, _ := json.Marshal(map[string]any{"markdown": out, "unread": unread})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &jobID, Type: "blast_radius", Data: string(data)}), "AddEvent")
	return out
}

// savedBlast is the blast radius applyBlast saved for a job.
func (p *Pipeline) savedBlast(ctx context.Context, itemID, jobID int64) string {
	evs, err := p.Store.Events(ctx, itemID)
	if err != nil {
		return ""
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Type == "blast_radius" && e.JobID != nil && *e.JobID == jobID {
			var d struct{ Markdown string }
			_ = json.Unmarshal([]byte(e.Data), &d)
			return d.Markdown
		}
	}
	return ""
}

// markdown is the blast radius for the reviewer and the PR.
func (b *blastRadius) markdown() string {
	if b == nil {
		return ""
	}
	if b.note != "" {
		return b.note + "\n"
	}
	var s strings.Builder
	if len(b.hits) == 0 {
		s.WriteString("The change touches no feature in the feature map.\n")
	} else {
		s.WriteString("| Feature | Risk | Changed files | Proven by |\n| --- | --- | --- | --- |\n")
		for _, h := range b.hits {
			f := h.Feature
			risk := f.Risk
			if risk == "" {
				risk = "-"
			}
			fmt.Fprintf(&s, "| %s | %s | %s | %s |\n", cell(f.Name), risk, cell(list(h.Files, 20)), cell(b.proof(f)))
		}
	}
	if len(b.unmapped) > 0 {
		fmt.Fprintf(&s, "\nNot in the feature map: %s\n", list(b.unmapped, 50))
	}
	return s.String()
}

func (b *blastRadius) proof(f features.Feature) string {
	var parts []string
	if r, ok := b.checks[f.Name]; ok {
		status := r.Status
		if r.baseRed {
			status = "**fails, as on the base branch**"
		}
		parts = append(parts, fmt.Sprintf("`%s`: %s", f.Check, status))
	}
	if len(f.Tests) > 0 {
		parts = append(parts, strings.Join(code(f.Tests), ", "))
	}
	if len(parts) == 0 {
		return "**nothing: no tests are mapped to it**"
	}
	return strings.Join(parts, "; ")
}

// list names up to n files, so a change with thousands can't overflow
// the PR body.
func list(files []string, n int) string {
	if len(files) <= n {
		return strings.Join(code(files), ", ")
	}
	return strings.Join(code(files[:n]), ", ") + fmt.Sprintf(" and %d more", len(files)-n)
}

func code(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = "`" + strings.ReplaceAll(f, "`", "'") + "`"
	}
	return out
}

// cell keeps text from breaking a markdown table row.
func cell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

// blastUnread says whether the feature map couldn't be read for a job, so
// the item's risk may be understated. With no record of it, it can't be
// known to have been read.
func (p *Pipeline) blastUnread(ctx context.Context, itemID, jobID int64) bool {
	evs, err := p.Store.Events(ctx, itemID)
	if err != nil {
		return true
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Type == "blast_radius" && e.JobID != nil && *e.JobID == jobID {
			var d struct{ Unread *bool }
			if json.Unmarshal([]byte(e.Data), &d) != nil || d.Unread == nil {
				return true
			}
			return *d.Unread
		}
	}
	return true
}
