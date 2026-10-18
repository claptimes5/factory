package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/store"
)

// groom queues a grooming run of one repo, or of grooming's repos, for
// the daemon to run on its next pass.
func groom(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store, repo string) error {
	p := &pipeline.Pipeline{Cfg: cfg, Store: st}
	repos := cfg.GroomRepos()
	if repo != "" {
		rc, ok := cfg.RepoConf(repo)
		if !ok {
			return fmt.Errorf("%s isn't one of the factory's repos (%s)", repo, strings.Join(cfg.RepoNames(), ", "))
		}
		repos = []string{rc.Name}
	}
	var errs []error
	for _, r := range repos {
		id, err := p.Groom(ctx, r, "factory groom")
		switch {
		case errors.Is(err, pipeline.ErrGroomQueued):
			fmt.Fprintf(w, "%s: a grooming run is already queued or running\n", r)
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
		default:
			fmt.Fprintf(w, "%s: queued grooming run (job %d); the daemon runs it and comments on each issue it flags\n", r, id)
		}
	}
	return errors.Join(errs...)
}

// groomLines are status's lines on grooming: each repo's last run and its
// open proposals; none while it's off and never ran.
func groomLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	runs, _ := st.GroomRuns(ctx)
	open, _ := st.OpenGroomProposals(ctx, "")
	if cfg.Grooming.Every <= 0 && len(runs) == 0 && len(open) == 0 {
		return nil
	}
	repos := cfg.GroomRepos()
	byRun := map[string]store.GroomRun{}
	for _, r := range runs {
		byRun[strings.ToLower(r.Repo)] = r
		if !containsFold(repos, r.Repo) {
			repos = append(repos, r.Repo)
		}
	}
	var out []string
	for _, repo := range repos {
		line := fmt.Sprintf("grooming %s:\t", repo)
		if r, ok := byRun[strings.ToLower(repo)]; ok {
			line += fmt.Sprintf("last run %s, read %d, flagged %d", r.RanAt[:min(10, len(r.RanAt))], r.Read, r.Flagged)
			if r.Note != "" {
				line += " (" + r.Note + ")"
			}
		} else {
			line += "not run yet"
		}
		kinds := map[string][]string{}
		n := 0
		for _, g := range open {
			if strings.EqualFold(g.Repo, repo) {
				kinds[g.Kind] = append(kinds[g.Kind], issueRef(cfg, repo, g.IssueNumber))
				n++
			}
		}
		if n > 0 {
			var parts []string
			for _, k := range []string{store.GroomDuplicate, store.GroomStale, store.GroomSplit} {
				if len(kinds[k]) > 0 {
					parts = append(parts, k+" "+strings.Join(kinds[k], ","))
				}
			}
			line += fmt.Sprintf("; %d open: %s", n, strings.Join(parts, "; "))
		}
		if cfg.Grooming.Every <= 0 {
			line += " (grooming.every is off)"
		}
		out = append(out, line)
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
