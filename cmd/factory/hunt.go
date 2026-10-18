package main

import (
	"context"
	"fmt"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/store"
)

// huntLines are status's lines on old-bug hunting, when it's on: per
// repo, the hunts run and the issues they filed.
func huntLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	if !cfg.Hunt.Enabled {
		return nil
	}
	p := &pipeline.Pipeline{Cfg: cfg, Store: st}
	var out []string
	for _, repo := range cfg.HuntRepos() {
		runs, filed, err := p.HuntCounts(ctx, repo)
		if err != nil {
			continue
		}
		out = append(out, fmt.Sprintf("hunt %s:\t%d run, %d filed", repo, runs, filed))
	}
	return out
}
