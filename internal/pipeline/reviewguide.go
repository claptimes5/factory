package pipeline

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/reviewmd"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// A repo's REVIEW.md is guidance its people write for the factory's
// reviewers. It is read from the base branch, never from the change, so a
// builder can't change what reviews it (and the diff guard protects it).
// Its text goes to the reviewer, the critic and the verifier, fenced and
// marked as the repo's; its front matter's skip and max_minor the daemon
// applies itself, after the reviewer has spoken (see applyGuides).

// maxReviewMD is the largest REVIEW.md read at all; the guidance in it
// is cut to reviewmd.MaxBytes.
const maxReviewMD = 1 << 20

// loadGuide reads the REVIEW.md at sha in the checkout dir: nil when the
// repo has none. One the factory can't read (a symlink out of the repo,
// say, or one too big) comes back with only a note saying so.
func (p *Pipeline) loadGuide(ctx context.Context, repo, dir, sha string) *reviewmd.Guide {
	g := p.git(repo)
	for _, name := range reviewmd.Files {
		mode, err := g.Mode(ctx, dir, sha, name)
		if err != nil {
			p.warn(err, "REVIEW.md")
			return nil
		}
		switch mode {
		case "":
			continue
		case "100644", "100755":
		default:
			// A symlink's target isn't protected, so a builder could
			// change what it says: it isn't read at all.
			return &reviewmd.Guide{File: name, Notes: []string{fmt.Sprintf("%s isn't a regular file (mode %s), so the factory doesn't read it", name, mode)}}
		}
		file := name
		if size, err := g.run(ctx, dir, nil, "cat-file", "-s", sha+":"+file); err == nil {
			if n, _ := strconv.Atoi(size); n > maxReviewMD {
				return &reviewmd.Guide{File: name, Notes: []string{fmt.Sprintf("%s is over %d MB, so the factory doesn't read it", name, maxReviewMD>>20)}}
			}
		}
		text, ok, err := g.FileAt(ctx, dir, sha, file)
		if err != nil || !ok {
			p.warn(err, "REVIEW.md")
			return nil
		}
		gd := reviewmd.Parse(name, text)
		for _, n := range gd.Notes {
			p.log().Warn("REVIEW.md", "repo", repo, "note", n)
		}
		return gd
	}
	return nil
}

// repoGuide is one repo's REVIEW.md in a review: folder is the repo's
// folder in a workspace checkout ("" on its own), and the rest is what a
// skip must never cover there.
type repoGuide struct {
	repo, folder string
	guide        *reviewmd.Guide
	protected    []string
	risky        *features.Map // the base branch's feature map, or nil
	// noSkip says skip can't apply: the feature map couldn't be read, so
	// a high-risk path can't be told apart.
	noSkip bool
	// modified are the files the change modifies, which were there on
	// the base: the only ones a skip may cover. A file it adds, renames
	// or copies could be anything the builder put under a skipped path.
	modified map[string]bool
}

// guides are a review's REVIEW.md files, one per repo that has one.
type guides []repoGuide

// reviewGuides reads REVIEW.md for each repo a review spans, from the
// base branch each was checked out on.
func (p *Pipeline) reviewGuides(ctx context.Context, parts []*part) guides {
	var gs guides
	for _, pt := range parts {
		gd := p.loadGuide(ctx, pt.repo, pt.ws.Repo, pt.ws.BaseSHA)
		if gd == nil {
			continue
		}
		rg := repoGuide{repo: pt.repo, folder: pt.folder, guide: gd, protected: p.repoConf(pt.repo).Gates.DiffGuard.ProtectedPaths}
		if len(gd.Skip) > 0 {
			m, err := p.loadMap(ctx, pt.repo, pt.ws.Repo, pt.ws.BaseSHA)
			p.warn(err, "loadMap")
			rg.risky, rg.noSkip = m, err != nil
			if pt.base != "" && pt.head != "" {
				mod, err := p.git(pt.repo).modifiedFiles(ctx, pt.ws.Repo, pt.base, pt.head)
				p.warn(err, "modifiedFiles")
				rg.modified = mod // nil on an error: nothing is skipped
			}
		}
		gs = append(gs, rg)
	}
	return gs
}

// modifiedFiles lists the files modified between two commits: there at
// from, and still there at to. Added, deleted, renamed and copied files
// aren't in it.
func (g *Git) modifiedFiles(ctx context.Context, repo, from, to string) (map[string]bool, error) {
	var out strings.Builder
	if _, err := g.run(ctx, repo, &out, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-status", "-z", from, to); err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	files := map[string]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "M" {
			files[fields[i+1]] = true
		}
	}
	return files, nil
}

// prompt is the guidance for a reviewer's prompt, fenced: it is the
// repo's text, which the reviewer reads as guidance, not instructions.
func (gs guides) prompt() string {
	var b strings.Builder
	for _, g := range gs {
		s := g.guide.Summary()
		if s == "" {
			continue
		}
		if g.folder != "" {
			fmt.Fprintf(&b, "=== %s (%s/) ===\n\n", g.repo, g.folder)
		}
		b.WriteString(s + "\n\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return fenced(b.String())
}

// file names the REVIEW.md files a review reads, for its prompt.
func (gs guides) file() string {
	var names []string
	for _, g := range gs {
		if g.guide.Summary() == "" {
			continue
		}
		n := g.guide.File
		if g.folder != "" {
			n = g.folder + "/" + n
		}
		names = append(names, "`"+n+"`")
	}
	return strings.Join(names, " and ")
}

// of finds the guide for a finding's path, and the path within its repo.
func (gs guides) of(file string) (*repoGuide, string) {
	for i := range gs {
		g := &gs[i]
		if g.folder == "" {
			return g, file
		}
		if rest, ok := strings.CutPrefix(file, g.folder+"/"); ok {
			return g, rest
		}
	}
	return nil, ""
}

// skips says whether a finding on file is only a note: REVIEW.md skips
// its path, and the path isn't one a skip may never cover (protected, a
// high-risk feature's or security-sensitive by its name).
func (g *repoGuide) skips(file string) bool {
	// The path is the reviewer's text: only a plain path to a file the
	// change modifies counts.
	if g == nil || g.noSkip || file == "" || path.Clean(file) != file || strings.HasPrefix(file, "/") || strings.HasPrefix(file, "./") ||
		strings.Contains(file, "..") || !g.modified[file] {
		return false
	}
	if !g.guide.Skips(file) || reviewmd.Sensitive(file) || gates.Protected(file, g.protected) {
		return false
	}
	if g.risky != nil {
		hits, _ := g.risky.Touched([]string{file})
		if r, _ := features.MaxRisk(hits); r == "high" {
			return false
		}
	}
	return true
}

// applyGuides applies the review's REVIEW.md files to a verdict, before
// its earlier rounds are carried in and its findings go to the verifier:
//
//   - A major or minor finding that would block, on a path a skip covers,
//     is kept as a note. Never a critical one, never on a high-risk item,
//     and never on a protected, high-risk or security-sensitive path. If
//     that leaves the reviewer's block with nothing to block on, the
//     verdict passes (findings carried from earlier rounds still block).
//   - Past max_minor, minor notes are left out, and counted. A minor
//     finding that blocks is never left out: the cap can't pass a change.
//
// Neither ever touches a finding of the security pass. The text of
// REVIEW.md can only steer the reviewer; these are the only two things it
// does to a verdict.
func applyGuides(gs guides, v *verdict.Review, risk string) {
	if len(gs) == 0 {
		return
	}
	applied := &verdict.GuideApplied{}
	var files []string
	for _, g := range gs {
		n := g.guide.File
		if g.folder != "" {
			n = g.folder + "/" + n
		}
		files = append(files, n)
		for _, note := range g.guide.Notes {
			applied.Notes = append(applied.Notes, n+": "+note)
		}
	}
	applied.File = strings.Join(files, ", ")
	v.Guide = applied
	demoted, still := false, false
	if risk != "high" {
		for _, i := range v.Blocking() {
			f := &v.Findings[i]
			g, rel := gs.of(f.File)
			if f.Severity == verdict.Critical || f.Carried || f.By == store.KindSecurity || !g.skips(rel) {
				still = true
				continue
			}
			no := false
			f.Blocks, f.Skipped, demoted = &no, true, true
		}
	}
	if demoted && !still && v.Decision == "block" && !v.BlockedWithoutFinding {
		v.Decision = "pass"
	}
	limit := -1
	for _, g := range gs {
		if m := g.guide.MaxMinor; m != nil && (limit < 0 || *m < limit) {
			limit = *m
		}
	}
	if limit < 0 {
		return
	}
	blocking := map[int]bool{}
	for _, i := range v.Blocking() {
		blocking[i] = true
	}
	kept, minors := v.Findings[:0:0], 0
	for i, f := range v.Findings {
		if f.Severity == verdict.Minor && !blocking[i] && !f.Carried && f.By != store.KindSecurity {
			if minors++; minors > limit {
				applied.DroppedMinor++
				continue
			}
		}
		kept = append(kept, f)
	}
	v.Findings = kept
}
