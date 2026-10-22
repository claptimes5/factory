package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
		if r.Gates != nil || r.Image != "" || r.ImageFrom != "" || r.ImageBuild != nil {
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
		if gates == nil && s.ImageFrom == "" && s.ImageBuild == nil && len(s.Services) == 0 {
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
		if b := s.ImageBuild; b != nil {
			stage := ""
			if b.Target != "" {
				stage = ", stage " + b.Target
			}
			p.say("  job image   built from the repo's %s%s", cmp.Or(b.Dockerfile, "Dockerfile"), stage)
		}
		for _, n := range slices.Sorted(maps.Keys(s.Services)) {
			p.say("  service     %s (%s)", n, s.Services[n].Image)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			p.say("  env         %s=%s", k, s.Env[k])
		}
		if gates != nil {
			if s.Install != "" {
				p.say("  install     %s", s.Install)
			}
			for _, g := range s.Required {
				from := ""
				if src := s.Sources[g]; src != "" {
					from = "   (from " + src + ")"
				}
				p.say("  %-11s %s%s", g, s.Commands[g], from)
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
		repos[i].Gates, repos[i].ImageFrom, repos[i].ImageBuild = gates, s.ImageFrom, s.ImageBuild
		repos[i].Services, repos[i].Env = s.Services, s.Env
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
	tag, from  string // from "" builds on the Dockerfile's default base
	repos      []string
	playwright bool // a repo on it takes screenshots, so it needs Chromium
	// build, when set, makes from out of repos[0]'s own Dockerfile first.
	build *config.ImageBuild
}

// jobImages lists the images the configured repos need that the factory
// builds itself. An image a repo names without image_from is the repo's
// own to build, so it is left out.
func jobImages(cfg *config.Config) []jobImage {
	var out []jobImage
	add := func(tag, from, repo string) {
		_, pw := cfg.PreviewFor(repo)
		for i := range out {
			if out[i].tag == tag {
				out[i].repos = append(out[i].repos, repo)
				out[i].playwright = out[i].playwright || pw
				return
			}
		}
		out = append(out, jobImage{tag: tag, from: from, repos: []string{repo}, playwright: pw})
	}
	for _, n := range cfg.RepoNames() {
		r, _ := cfg.RepoConf(n)
		switch {
		case r.ImageBuild != nil:
			// Its own tag and base: never shared with another repo.
			_, pw := cfg.PreviewFor(n)
			out = append(out, jobImage{tag: r.Image, from: r.BaseImage(), repos: []string{n}, playwright: pw, build: r.ImageBuild})
		case r.ImageFrom != "":
			add(r.Image, r.ImageFrom, n)
		case r.Image == cfg.Runner.Image:
			add(r.Image, "", n)
		}
	}
	// The orchestrator, tuning and grooming belong to no repo, so they run
	// in runner.image even when every repo has an image of its own.
	if usesRunnerImage(cfg) && !slices.ContainsFunc(out, func(im jobImage) bool { return im.tag == cfg.Runner.Image }) {
		out = append(out, jobImage{tag: cfg.Runner.Image, repos: []string{runnerImageUsers}})
	}
	return out
}

const runnerImageUsers = "the orchestrator, tuning and grooming"

// usesRunnerImage says whether a job outside any repo can run.
func usesRunnerImage(cfg *config.Config) bool {
	return cfg.Orchestrator.On || cfg.Tuning.Every > 0 || cfg.Grooming.Every > 0
}

// sourceLabel records what a job image was built from, so one built from
// something factory.yaml no longer says (another image_from, say) counts
// as not built, and serve builds it again.
const sourceLabel = "io.factory.source"

// source is what im is built from, for sourceLabel.
func (im jobImage) source() string {
	s := "from=" + im.from
	if im.playwright {
		s += " playwright"
	}
	if im.build != nil {
		b, _ := json.Marshal(im.build) // map keys sorted, so it's stable
		s += " build=" + string(b)
	}
	return s
}

// inspectArgs read an image's sourceLabel; current says whether what they
// gave (or err, when there's no such image) is a build of im as it is now.
// Every image jobImages lists is the factory's to build, so one with no
// label, built before there was one, is built again once.
func (im jobImage) inspectArgs() []string {
	return []string{"image", "inspect", "--format", `{{index .Config.Labels "` + sourceLabel + `"}}`, im.tag}
}

func (im jobImage) current(out string, err error) bool {
	return err == nil && strings.TrimSpace(out) == im.source()
}

// isBuilt says whether im is built as factory.yaml says now.
func (im jobImage) isBuilt() bool {
	var b strings.Builder
	err := runCmd(&b, "docker", im.inspectArgs()...)
	return im.current(b.String(), err)
}

func (im jobImage) buildArgs(dir string) []string {
	// The label lets pruneImages clean up old copies of only our images.
	a := []string{"build", "-t", im.tag, "--label", imageLabel + "=1", "--label", sourceLabel + "=" + im.source()}
	if im.from != "" {
		a = append(a, "--build-arg", "BASE="+im.from)
	}
	if im.playwright {
		a = append(a, "--build-arg", "PLAYWRIGHT=1")
	}
	return append(a, dir)
}

// cmdRun runs a command, as runCmdContext does.
type cmdRun func(ctx context.Context, out io.Writer, name string, args ...string) error

// make builds the image in dir's Dockerfile on its base, first building
// the base from the repo's own Dockerfile when it says to. tok reaches
// the repo to clone it.
func (im jobImage) make(ctx context.Context, out io.Writer, cfg *config.Config, dir string, tok func(*config.Config, string) (github.TokenSource, error), run cmdRun) error {
	if im.build != nil {
		if err := im.makeBase(ctx, out, cfg, tok, run); err != nil {
			return err
		}
	}
	return run(ctx, out, "docker", im.buildArgs(dir)...)
}

// makeBase builds im.from from the repo's Dockerfile, on a clone of its
// base branch. It runs on this machine's Docker, outside the jobs'
// sandbox, as building the app's image yourself would.
func (im jobImage) makeBase(ctx context.Context, out io.Writer, cfg *config.Config, tok func(*config.Config, string) (github.TokenSource, error), run cmdRun) error {
	repo := im.repos[0]
	rc, _ := cfg.RepoConf(repo)
	ts, err := tok(cfg, repo)
	if err != nil {
		return fmt.Errorf("cloning %s for its Dockerfile: %w", repo, err)
	}
	data := cfg.Path(cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(data, "image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	g := repoGit(cfg, repo, ts)
	ws, err := g.Prepare(ctx, tmp, rc.BaseBranch, "", false, false)
	if err != nil {
		return fmt.Errorf("cloning %s for its Dockerfile: %w", repo, err)
	}
	src := filepath.Join(tmp, "src")
	if err := g.Export(ctx, ws.Repo, ws.BaseSHA, src); err != nil {
		return err
	}
	df := cmp.Or(im.build.Dockerfile, "Dockerfile")
	fmt.Fprintf(out, "Building %s from %s's %s at %s...\n", im.from, repo, df, rc.BaseBranch)
	args := []string{"build", "-t", im.from, "--label", imageLabel + "=1", "-f", filepath.Join(src, filepath.FromSlash(df))}
	if im.build.Target != "" {
		args = append(args, "--target", im.build.Target)
	}
	for _, k := range slices.Sorted(maps.Keys(im.build.Args)) {
		args = append(args, "--build-arg", k+"="+im.build.Args[k])
	}
	for _, id := range slices.Sorted(maps.Keys(im.build.Secrets)) {
		f, err := cfg.SecretFile(rc, id)
		if err != nil {
			return err
		}
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("the build secret %s for %s's Dockerfile: %w", id, repo, err)
		}
		args = append(args, "--secret", "id="+id+",src="+f)
	}
	if err := run(ctx, out, "docker", append(args, filepath.Join(src, filepath.FromSlash(cmp.Or(im.build.Context, "."))))...); err != nil {
		err = fmt.Errorf("building %s from its %s: %w", repo, df, err)
		// A secret it isn't given is just absent in the build, so the
		// step that reads it fails without naming it.
		b, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(df)))
		var missing []string
		for _, id := range stack.SecretIDs(string(b)) {
			if im.build.Secrets[id] == "" {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			err = fmt.Errorf("%w; it uses the build secrets %s, which image_build.secrets in factory.yaml doesn't give: set each to the file that holds it", err, strings.Join(missing, ", "))
		}
		return err
	}
	// The factory's tools go on with apt; say so now, not as a failed
	// step of the next build.
	if run(ctx, io.Discard, "docker", "run", "--rm", "--entrypoint", "sh", im.from, "-c", "command -v apt-get") != nil {
		return fmt.Errorf("%s's %s builds an image without apt-get (Alpine, distroless?); job images need Debian or Ubuntu, so set image_from in factory.yaml instead of image_build", repo, df)
	}
	return pointAtCheckout(ctx, run, im.from, tmp)
}

// pointAtCheckout re-tags base with its PATH's entries into its own copy
// of the app (bin/ under its WORKDIR, where binstubs are) moved to the
// job's checkout at /work: that copy is the base branch as built, not
// the change being checked, and a binstub there runs its Gemfile.
func pointAtCheckout(ctx context.Context, run cmdRun, base, tmp string) error {
	var b strings.Builder
	if err := run(ctx, &b, "docker", "image", "inspect", "--format", "{{.Config.WorkingDir}}\n{{range .Config.Env}}{{println .}}{{end}}", base); err != nil {
		return err
	}
	wd, envs, _ := strings.Cut(b.String(), "\n")
	path, fixed := "", false
	for _, kv := range strings.Split(envs, "\n") {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			path, fixed = movePath(p, strings.TrimSpace(wd))
		}
	}
	if !fixed {
		return nil
	}
	dir := filepath.Join(tmp, "path")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\nENV PATH="+path+"\n"), 0o644); err != nil {
		return err
	}
	return run(ctx, io.Discard, "docker", "build", "-t", base, "--label", imageLabel+"=1", dir)
}

// movePath is PATH with each entry under the image's WORKDIR wd moved to
// /work, and whether any was.
func movePath(path, wd string) (string, bool) {
	wd = strings.TrimSuffix(wd, "/")
	if wd == "" || wd == "/work" {
		return path, false
	}
	parts, moved := strings.Split(path, ":"), false
	for i, p := range parts {
		if p == wd || strings.HasPrefix(p, wd+"/") {
			parts[i], moved = "/work"+strings.TrimPrefix(p, wd), true
		}
	}
	return strings.Join(parts, ":"), moved
}

// buildImages builds the job images that are missing, or all of them with
// force. It stops at the first failure.
func buildImages(out io.Writer, cfg *config.Config, force bool) error {
	dir := cfg.Path("deploy/job-image")
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return fmt.Errorf("can't find %s next to the config; run this from the factory's checkout", dir)
	}
	built := false
	for _, im := range jobImages(cfg) {
		if !force && im.isBuilt() {
			fmt.Fprintf(out, "%s is built.\n", im.tag)
			continue
		}
		fmt.Fprintf(out, "Building %s for %s...\n", im.tag, joinAnd(im.repos))
		run := func(_ context.Context, out io.Writer, name string, args ...string) error {
			return runCmd(out, name, args...)
		}
		if err := im.make(context.Background(), out, cfg, dir, tokens, run); err != nil {
			return fmt.Errorf("building %s: %w", im.tag, err)
		}
		built = true
	}
	if built {
		pruneImages(cfg)
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
