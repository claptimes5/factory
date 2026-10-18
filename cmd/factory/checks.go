package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/stack"
)

// ghFiles reads a repo's default branch through its host's API.
type ghFiles struct{ gh forge }

func (f ghFiles) List(ctx context.Context, dir string) ([]string, error) {
	return f.gh.ListDir(ctx, dir)
}
func (f ghFiles) Read(ctx context.Context, path string) (string, error) {
	return f.gh.ReadFile(ctx, path)
}

// setupChecks looks at each repo's files and offers checks and a job
// image that fit its stack. Repos that already have their own are left
// alone.
func setupChecks(ctx context.Context, p *prompter, cfgPath string, cfg *config.Config, client clientFor) error {
	repos := slices.Clone(cfg.Repos)
	changed := false
	for i, r := range repos {
		if r.Gates != nil || r.Image != "" || r.ImageFrom != "" {
			p.say("%s already has its own checks or image in %s; keeping them.", r.Name, cfgPath)
			continue
		}
		gh, _, err := client(r.Name)
		if err != nil {
			p.say("%s: %v", r.Name, err)
			continue
		}
		s, err := stack.Detect(ctx, ghFiles{gh})
		if err != nil && !github.IsNotFound(err) {
			p.say("Couldn't read %s's files: %v. It uses the top-level checks.", r.Name, err)
			continue
		}
		if s == nil {
			p.say("%s: no stack I recognize, so it uses the top-level checks in %s.", r.Name, cfgPath)
			continue
		}
		var gates *config.Gates
		if !s.Go {
			// proves_fix and coverage are Go-only for now.
			gates = &config.Gates{Install: s.Install, Required: s.Required, Commands: s.Commands, ProvesFix: "none", Coverage: "none"}
		}
		if gates == nil && s.ImageFrom == "" {
			p.say("%s is %s; the top-level checks fit it.", r.Name, s.Stack)
			for _, n := range s.Notes {
				p.say("  Note: %s", n)
			}
			continue
		}
		p.say("\n%s looks like %s. Suggested setup:", r.Name, s.Stack)
		if s.ImageFrom != "" {
			p.say("  job image   built on %s", s.ImageFrom)
		}
		if gates != nil {
			if s.Install != "" {
				p.say("  install     %s", s.Install)
			}
			for _, g := range s.Required {
				p.say("  %-11s %s", g, s.Commands[g])
			}
			if len(s.Required) == 0 {
				p.say("  (no test, lint or build commands found; add them under gates.commands)")
			}
		}
		for _, n := range s.Notes {
			p.say("  Note: %s", n)
		}
		if !p.yes(fmt.Sprintf("Use this for %s? You can edit it in %s later", r.Name, cfgPath), true) {
			continue
		}
		repos[i].Gates, repos[i].ImageFrom = gates, s.ImageFrom
		changed = true
	}
	if !changed {
		return nil
	}
	if err := writeRepos(cfgPath, repos); err != nil {
		return err
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	*cfg = *c
	p.say("Updated %s.", cfgPath)
	return nil
}

// jobImage is one image jobs run in, and how to build it.
type jobImage struct {
	tag, from string // from "" builds on the Dockerfile's default base
	repos     []string
}

// jobImages lists the images the configured repos need that the factory
// builds itself. An image a repo names without image_from is the repo's
// own to build, so it is left out.
func jobImages(cfg *config.Config) []jobImage {
	var out []jobImage
	add := func(tag, from, repo string) {
		for i := range out {
			if out[i].tag == tag {
				out[i].repos = append(out[i].repos, repo)
				return
			}
		}
		out = append(out, jobImage{tag: tag, from: from, repos: []string{repo}})
	}
	for _, n := range cfg.RepoNames() {
		r, _ := cfg.RepoConf(n)
		switch {
		case r.ImageFrom != "":
			add(r.Image, r.ImageFrom, n)
		case r.Image == cfg.Runner.Image:
			add(r.Image, "", n)
		}
	}
	return out
}

func (im jobImage) buildArgs(dir string) []string {
	a := []string{"build", "-t", im.tag}
	if im.from != "" {
		a = append(a, "--build-arg", "BASE="+im.from)
	}
	return append(a, dir)
}

// buildImages builds the job images that are missing, or all of them with
// force. It stops at the first failure.
func buildImages(out io.Writer, cfg *config.Config, force bool) error {
	dir := cfg.Path("deploy/job-image")
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return fmt.Errorf("can't find %s next to the config; run this from the factory's checkout", dir)
	}
	for _, im := range jobImages(cfg) {
		if !force && runCmd(io.Discard, "docker", "image", "inspect", im.tag) == nil {
			fmt.Fprintf(out, "%s is built.\n", im.tag)
			continue
		}
		fmt.Fprintf(out, "Building %s for %s...\n", im.tag, joinAnd(im.repos))
		if err := runCmd(out, "docker", im.buildArgs(dir)...); err != nil {
			return fmt.Errorf("building %s: %w", im.tag, err)
		}
	}
	return nil
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	out := s[0]
	for _, x := range s[1 : len(s)-1] {
		out += ", " + x
	}
	return out + " and " + s[len(s)-1]
}
