package pipeline

import (
	"context"
	"fmt"
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
	checks   map[string]gates.Result // by feature name
	note     string                  // why there is no map to use
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
func (p *Pipeline) featureChecks(ctx context.Context, jobID int64, it *store.WorkItem, ws *Workspace, base, head, gateDir string, snap snapshot) ([]gates.Result, *blastRadius) {
	m, err := p.loadMap(ctx, it.Repo, ws.Repo, base)
	switch {
	case err != nil:
		return nil, &blastRadius{note: "The feature map on the base branch can't be used: " + err.Error()}
	case m == nil:
		return nil, nil
	}
	changed, err := p.git(it.Repo).ChangedFiles(ctx, ws.Repo, base, head)
	if err != nil {
		return nil, &blastRadius{note: "Couldn't list the changed files: " + err.Error()}
	}
	b := &blastRadius{checks: map[string]gates.Result{}}
	b.hits, b.unmapped = m.Touched(changed)
	var results []gates.Result
	for _, h := range b.hits {
		if h.Feature.Check == "" {
			continue
		}
		r := gates.Result{Gate: "feature:" + h.Feature.Name, Status: gates.Pass}
		code, out, err := p.gateExec(ctx, jobID, gateDir, h.Feature.Check, gateRun{repo: it.Repo})
		switch {
		case err != nil:
			r.Status, r.Detail = gates.Fail, "could not run: "+err.Error()
		case code != 0:
			r.Status, r.Detail = gates.Fail, fmt.Sprintf("exit %d\n%s", code, tail(out, 3000))
		default:
			if note := snap.tamperNote(gateDir); note != "" {
				r.Status, r.Detail = gates.Fail, note
			}
		}
		b.checks[h.Feature.Name] = r
		results = append(results, r)
	}
	return results, b
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
			fmt.Fprintf(&s, "| %s | %s | %s | %s |\n", cell(f.Name), risk, cell(strings.Join(code(h.Files), ", ")), cell(b.proof(f)))
		}
	}
	if len(b.unmapped) > 0 {
		fmt.Fprintf(&s, "\nNot in the feature map: %s\n", strings.Join(code(b.unmapped), ", "))
	}
	return s.String()
}

func (b *blastRadius) proof(f features.Feature) string {
	var parts []string
	if r, ok := b.checks[f.Name]; ok {
		parts = append(parts, fmt.Sprintf("`%s`: %s", f.Check, r.Status))
	}
	if len(f.Tests) > 0 {
		parts = append(parts, strings.Join(code(f.Tests), ", "))
	}
	if len(parts) == 0 {
		return "**nothing: no tests are mapped to it**"
	}
	return strings.Join(parts, "; ")
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
