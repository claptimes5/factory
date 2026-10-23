package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/web"
)

// uninstaller lists and removes what the factory made: its labels on
// each repo, the job images it built and, if asked, its data. The GitHub
// App, tokens and config files are left for a person, with links. With
// repo set, it's only what the factory made for that repo.
type uninstaller struct {
	cfg              *config.Config
	cfgPath, envPath string
	// repo limits it to one repo, in cfg or already dropped from it; see
	// scoped.
	repo string
	// drop takes a repo out of factory.yaml and its workspace.
	drop func(ctx context.Context, repo string) error
	// forgeFor reaches a repo with the saved credentials.
	forgeFor func(repo string) (forge, error)
	// busy says why removing must wait (jobs running), or "".
	busy func(ctx context.Context) string
	// hold stops new work from starting until its release; nil when
	// nothing in this process starts work.
	hold func() (release func())
	// serving: the factory runs in this process, so its database and
	// sign-ins stay and only the job data goes.
	serving bool
	// active refuses a second Remove while one runs, of any scope.
	active *atomic.Bool
}

func newUninstaller(cfg *config.Config, cfgPath, envPath string, serving bool, busy func(context.Context) string) *uninstaller {
	return &uninstaller{cfg: cfg, cfgPath: cfgPath, envPath: envPath, serving: serving, busy: busy, active: new(atomic.Bool),
		forgeFor: func(repo string) (forge, error) {
			ts, err := tokens(cfg, repo)
			if err != nil {
				return nil, err
			}
			return newForge(cfg, repo, ts), nil
		},
		drop: func(_ context.Context, repo string) error {
			cur, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			return setRepoNames(cfgPath, cur, slices.DeleteFunc(cur.RepoNames(), func(n string) bool { return strings.EqualFold(n, repo) }))
		}}
}

// scoped is u for one repo, or u itself for "". It reads factory.yaml
// again: a repo removed from the dashboard leaves it before the factory
// restarts, and the next must not put it back.
func (u *uninstaller) scoped(repo string) *uninstaller {
	if repo == "" {
		return u
	}
	c := *u
	if cfg, err := config.Load(u.cfgPath); err == nil {
		c.cfg = cfg
	}
	c.repo = strings.Trim(strings.TrimSpace(repo), "/")
	if rc, ok := c.cfg.RepoConf(c.repo); ok {
		c.repo = rc.Name
	}
	return &c
}

// listed says whether u.repo is still in factory.yaml.
func (u *uninstaller) listed() bool {
	_, ok := u.cfg.RepoConf(u.repo)
	return u.repo != "" && ok
}

// repoRefused says why u.repo can't be removed on its own, or "".
func (u *uninstaller) repoRefused() string {
	switch {
	case u.repo == "":
		return ""
	// owner/name at least, so its folder in the data folder is never the
	// data folder itself.
	case !strings.Contains(u.repo, "/") || slices.Contains(strings.Split(u.repo, "/"), ".."):
		return u.repo + " isn't a repo: write it as owner/name, or group/project with its GitLab server."
	case u.listed() && len(u.cfg.Repos) == 1:
		return u.repo + " is the only repo the factory works on: add another first, or remove the factory instead."
	}
	return ""
}

// servingData are the folders in the data folder a running factory can
// do without: no job is running, so nothing reads them.
var servingData = []string{"jobs", "readiness", "compare", "sessions", "cache"}

// dataNames are what the factory puts at the top of its data folder;
// dataPrefixes the starts of the names it makes there.
var (
	dataNames    = []string{"jobs", "readiness", "compare", "sessions", "cache", "sync", "codex", "backups", "web.secret"}
	dataPrefixes = []string{"factory.db", "todos-", "eval-", "capture-", "factory-askpass", "readiness-"}
)

// Plan is what removing the factory, or with repo that repo, would
// remove. It changes nothing.
func (u *uninstaller) Plan(ctx context.Context, repo string) web.APIUninstall {
	u = u.scoped(repo)
	if why := u.repoRefused(); why != "" {
		return web.APIUninstall{Labels: []web.APIUninstallLabel{}, Repos: []web.APIUninstallRepo{}, Images: []web.APIUninstallImage{}, Kept: []web.APIUninstallKept{}, Manual: []web.APIUninstallStep{}, Refused: why}
	}
	p := web.APIUninstall{Listed: u.listed(), Labels: []web.APIUninstallLabel{}, Repos: []web.APIUninstallRepo{}, Images: []web.APIUninstallImage{}, Kept: []web.APIUninstallKept{}}
	for _, r := range u.scanLabels(ctx) {
		if r.err != nil {
			p.Repos = append(p.Repos, web.APIUninstallRepo{Repo: r.repo, Error: r.err.Error()})
			continue
		}
		p.Kept = append(p.Kept, r.kept...)
		for _, name := range r.remove {
			l := web.APIUninstallLabel{Repo: r.repo, Name: name, Open: -1}
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			if n, err := r.f.CountOpenLabeled(cctx, name); err == nil {
				l.Open, l.OpenCapped = n, n >= github.LabelCountCap
			}
			cancel()
			p.Labels = append(p.Labels, l)
		}
	}
	var kept []web.APIUninstallKept
	p.Images, kept, p.ImagesNote = u.images()
	p.Kept = append(p.Kept, kept...)
	p.Data = u.data()
	p.Manual = u.manual()
	if u.busy != nil {
		p.Refused = u.busy(ctx)
	}
	return p
}

// repoLabels is one repo's labels: those to remove, and those the factory
// leaves with why.
type repoLabels struct {
	repo   string
	f      forge
	err    error
	remove []string
	kept   []web.APIUninstallKept
}

// scanLabels reads each repo's labels once and sorts out the factory's.
// A label counts as the factory's when it has one of the names the
// factory defines and either its factory: prefix or the description
// setup writes; an intake label you named yourself stays. A GitLab
// group's label can't be deleted from the project, so it stays too.
func (u *uninstaller) scanLabels(ctx context.Context) []repoLabels {
	ours := pipeline.LabelNames(u.cfg.Intake.Label)
	var out []repoLabels
	repos := u.cfg.RepoNames()
	if u.repo != "" {
		repos = []string{u.repo}
	}
	for _, repo := range repos {
		r := repoLabels{repo: repo}
		r.f, r.err = u.forgeFor(repo)
		var labels []github.LabelInfo
		if r.err == nil {
			lctx, cancel := context.WithTimeout(ctx, time.Minute)
			labels, r.err = r.f.LabelsInfo(lctx)
			cancel()
		}
		// GitLab's label names are case-sensitive; GitHub's aren't.
		gitlab := repoFor(u.cfg, repo).GitLab()
		same := func(a, b string) bool { return a == b || !gitlab && strings.EqualFold(a, b) }
		for _, l := range labels {
			if !slices.ContainsFunc(ours, func(o string) bool { return same(o, l.Name) }) {
				continue
			}
			switch {
			case l.Group:
				r.kept = append(r.kept, web.APIUninstallKept{What: "label " + l.Name + " on " + repo,
					Why: "defined in group " + groupOf(u.cfg, repo) + " (or one above it), not removed: delete it there"})
			case !strings.HasPrefix(l.Name, "factory:") && !strings.HasPrefix(l.Description, "Factory:"):
				r.kept = append(r.kept, web.APIUninstallKept{What: "label " + l.Name + " on " + repo,
					Why: "your intake label, which the factory didn't make: yours to delete by hand"})
			default:
				r.remove = append(r.remove, l.Name)
			}
		}
		out = append(out, r)
	}
	return out
}

// groupOf is the group a GitLab project sits in.
func groupOf(cfg *config.Config, repo string) string {
	path := repoFor(cfg, repo).Path()
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return path
}

// images are the job images the factory built: the tags it builds, and
// old untagged copies marked with imageLabel. A repo's own image is never
// one, and the runner image is only when it carries the mark, since
// another image may have that name.
func (u *uninstaller) images() ([]web.APIUninstallImage, []web.APIUninstallKept, string) {
	out, kept := []web.APIUninstallImage{}, []web.APIUninstallKept{}
	if u.cfg.Runner.Kind != "docker" {
		return out, kept, "Jobs don't run in Docker here (runner.kind is " + u.cfg.Runner.Kind + "), so there are no job images."
	}
	if !dockerUp() {
		return out, kept, "Docker isn't answering, so the job images can't be listed. Start it and look again."
	}
	if u.repo != "" {
		return u.repoImages()
	}
	runner := withTag(u.cfg.Runner.Image)
	var own []string
	for _, n := range u.cfg.RepoNames() {
		if r, _ := u.cfg.RepoConf(n); r.ImageFrom == "" && r.Image != "" && withTag(r.Image) != runner {
			own = append(own, withTag(r.Image))
		}
	}
	var tags []string
	for _, j := range jobImages(u.cfg) {
		tags = append(tags, j.tag)
		if j.build != nil {
			tags = append(tags, j.from) // after the job image built on it
		}
	}
	for _, tag := range tags {
		tag := withTag(tag)
		if slices.Contains(own, tag) || slices.ContainsFunc(out, func(im web.APIUninstallImage) bool { return im.Ref == tag }) {
			continue
		}
		var b bytes.Buffer
		if runCmd(&b, "docker", "image", "inspect", "--format", "{{.Id}}\t{{.Size}}\t{{index .Config.Labels \""+imageLabel+"\"}}", tag) != nil {
			continue // not built
		}
		f := strings.SplitN(strings.TrimSpace(b.String()), "\t", 3)
		for len(f) < 3 {
			f = append(f, "")
		}
		if tag == runner && f[2] != "1" {
			kept = append(kept, web.APIUninstallKept{What: "image " + tag, Why: "not marked as built by the factory, so it's left alone; remove it yourself if the factory built it"})
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		out = append(out, web.APIUninstallImage{ID: shortID(f[0]), Ref: tag, Size: humanBytes(n)})
	}
	var b bytes.Buffer
	if err := runCmd(&b, "docker", "image", "ls", "--filter", "label="+imageLabel+"=1", "--filter", "dangling=true", "--format", "{{.ID}}\t{{.Size}}"); err != nil {
		return out, kept, "Couldn't list the old copies of the job images: " + cmdError(b.String(), err)
	}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		id, size, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		// Tags first: an old copy may be removable only once its tag is.
		out = append(out, web.APIUninstallImage{ID: shortID(id), Ref: shortID(id), Size: size, Dangling: true})
	}
	return out, kept, ""
}

// repoImages are the images built for u.repo alone: its job image and
// the base built from its Dockerfile. One the other repos or the
// orchestrator still use stays, and so does one without the factory's
// mark. Old untagged copies are left to the hourly cleanup.
func (u *uninstaller) repoImages() ([]web.APIUninstallImage, []web.APIUninstallKept, string) {
	out, kept := []web.APIUninstallImage{}, []web.APIUninstallKept{}
	used := []string{withTag(u.cfg.Runner.Image)}
	for _, j := range jobImages(u.rest()) {
		used = append(used, withTag(j.tag))
		if j.build != nil {
			used = append(used, withTag(j.from))
		}
	}
	rc := repoFor(u.cfg, u.repo)
	var tags []string
	if r, ok := u.cfg.RepoConf(u.repo); ok {
		tags = append(tags, withTag(r.Image))
	}
	for _, tag := range append(tags, rc.JobImage(), rc.BaseImage()) {
		if slices.Contains(used, tag) || slices.ContainsFunc(out, func(im web.APIUninstallImage) bool { return im.Ref == tag }) {
			continue
		}
		var b bytes.Buffer
		if runCmd(&b, "docker", "image", "inspect", "--format", "{{.Id}}\t{{.Size}}\t{{index .Config.Labels \""+imageLabel+"\"}}", tag) != nil {
			continue // not built
		}
		f := strings.SplitN(strings.TrimSpace(b.String()), "\t", 3)
		for len(f) < 3 {
			f = append(f, "")
		}
		if f[2] != "1" {
			kept = append(kept, web.APIUninstallKept{What: "image " + tag, Why: "not marked as built by the factory, so it's left alone; remove it yourself if the factory built it"})
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		out = append(out, web.APIUninstallImage{ID: shortID(f[0]), Ref: tag, Size: humanBytes(n)})
	}
	return out, kept, ""
}

// rest is the config without u.repo.
func (u *uninstaller) rest() *config.Config {
	c := *u.cfg
	c.Repos = slices.DeleteFunc(slices.Clone(c.Repos), func(r config.Repo) bool { return strings.EqualFold(r.Name, u.repo) })
	return &c
}

// repoFiles are u.repo's files in the data folder that exist: its
// dependency cache and readiness report.
func (u *uninstaller) repoFiles() []string {
	var out []string
	for _, p := range []string{
		filepath.Join(u.dataDir(), "cache", strings.ReplaceAll(u.repo, "/", "__")),
		readiness.Path(pipeline.ReadinessDir(u.cfg), u.repo),
	} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// withTag adds :latest to an image name without a tag, as docker shows it.
func withTag(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref
	}
	return ref + ":latest"
}

func cmdError(out string, err error) string {
	if l := lastLine(out); l != "" {
		return l
	}
	return err.Error()
}

// data is the data folder and what removing it here removes.
func (u *uninstaller) data() web.APIUninstallData {
	dir := u.dataDir()
	d := web.APIUninstallData{Path: dir}
	if u.repo != "" {
		files := u.repoFiles()
		if len(files) == 0 {
			d.Note = "There's nothing of " + u.repo + "'s in the data folder."
			return d
		}
		var n int64
		for _, f := range files {
			n += dirSize(f)
		}
		d.Exists, d.Size = true, humanBytes(n)
		d.What = u.repo + "'s dependency cache and readiness report: " + joinAnd(files)
		return d
	}
	if _, err := os.Stat(dir); err != nil {
		d.Note = "There's no data folder."
		return d
	}
	d.Exists = true
	if err := u.dataRemovable(dir); err != nil {
		d.Blocked = err.Error()
	}
	if u.serving {
		var n int64
		for _, s := range servingData {
			n += dirSize(filepath.Join(dir, s))
		}
		d.Size = humanBytes(n)
		d.What = "Job logs, kept checkouts of stopped builds, readiness reports and repo caches, in " + dir + "."
		d.Note = "The database (items, jobs, spend), the Codex sign-in and the dashboard's key stay while the factory runs. Once it's stopped, delete " + dir + ", or run ./factory uninstall --data."
		return d
	}
	d.Size = humanBytes(dirSize(dir))
	d.What = "Everything in " + dir + ": the database (items, jobs, spend), job logs and kept checkouts, readiness reports and the Codex sign-in."
	return d
}

func (u *uninstaller) dataDir() string {
	dir := u.cfg.Path(u.cfg.DataDir)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// dataRemovable says why the data folder isn't the factory's alone to
// delete, or nil: it must hold the factory's database and nothing at
// its top the factory didn't make, and be neither home nor hold the
// settings (as data_dir: . would).
func (u *uninstaller) dataRemovable(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	not := func(why string) error {
		return fmt.Errorf("%s isn't removed: %s. Delete what you don't want by hand", dir, why)
	}
	if home, err := os.UserHomeDir(); err == nil {
		realHome, _ := filepath.EvalSymlinks(home)
		for _, d := range []string{dir, real} {
			if d == home || d == realHome {
				return not("it's your home folder")
			}
		}
	}
	if dir == "/" || real == "/" {
		return not("it's the root folder")
	}
	for _, f := range []string{u.cfgPath, u.envPath} {
		if inside(dir, f) || inside(real, f) {
			return not("it holds " + filepath.Base(f))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "factory.db")); err != nil {
		return not("there's no factory.db in it, so it may not be the factory's")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return not(err.Error())
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue // .DS_Store and the like
		}
		if !slices.Contains(dataNames, n) && !slices.ContainsFunc(dataPrefixes, func(p string) bool { return strings.HasPrefix(n, p) }) {
			return not("it holds " + n + ", which the factory didn't make")
		}
	}
	return nil
}

// inside says whether path is in dir, following links in path.
func inside(dir, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	for _, p := range []string{abs, evalOr(abs)} {
		if rel, err := filepath.Rel(dir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

func evalOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// manual are the steps left for a person, with links.
func (u *uninstaller) manual() []web.APIUninstallStep {
	var out []web.APIUninstallStep
	if u.repo != "" {
		return u.repoManual()
	}
	if len(githubRepos(u.cfg)) > 0 {
		owner, _, _ := strings.Cut(githubRepos(u.cfg)[0], "/")
		slug, app, inst := os.Getenv("FACTORY_GITHUB_APP_SLUG"), os.Getenv("FACTORY_GITHUB_APP_ID"), os.Getenv("FACTORY_GITHUB_INSTALLATION_ID")
		switch {
		case slug != "":
			out = append(out,
				web.APIUninstallStep{Text: "Delete the GitHub App " + slug + " (only its owner can): its settings page, Advanced, Delete GitHub App.", URL: githubWeb + "/settings/apps/" + url.PathEscape(slug)},
				web.APIUninstallStep{Text: "If " + owner + " owns the app, it's under the organization's settings instead.", URL: githubWeb + "/organizations/" + url.PathEscape(owner) + "/settings/apps/" + url.PathEscape(slug)})
		case app != "":
			out = append(out, web.APIUninstallStep{Text: "Delete the GitHub App (ID " + app + ") from your apps, or from " + owner + "'s if the organization owns it (only its owner can).", URL: githubWeb + "/settings/apps"})
		}
		if inst != "" {
			out = append(out, web.APIUninstallStep{Text: "Or keep the app and uninstall it from " + owner + ": its installation, Uninstall.", URL: githubWeb + "/organizations/" + url.PathEscape(owner) + "/settings/installations/" + url.PathEscape(inst)},
				web.APIUninstallStep{Text: "For an install on your own account, it's here.", URL: githubWeb + "/settings/installations/" + url.PathEscape(inst)})
		}
		if os.Getenv("GITHUB_TOKEN") != "" && app == "" {
			out = append(out, web.APIUninstallStep{Text: "Revoke the GitHub token the factory used (GITHUB_TOKEN), if you made it for the factory.", URL: githubWeb + "/settings/personal-access-tokens"})
		}
	}
	for _, h := range gitlabHosts(u.cfg) {
		out = append(out, web.APIUninstallStep{Text: "Revoke the GitLab token the factory used (GITLAB_TOKEN) on " + h + ": a personal token under your access tokens, or a project token under the project's Settings, Access tokens.",
			URL: "https://" + h + "/-/user_settings/personal_access_tokens"})
	}
	files := []string{u.cfgPath}
	if _, err := os.Stat(u.envPath); err == nil {
		files = append(files, u.envPath)
	}
	out = append(out, web.APIUninstallStep{Text: "The factory doesn't delete its settings: delete " + joinAnd(files) + " yourself once you're done (factory.env holds its keys)."})
	return out
}

// repoManual are the steps left for a person once u.repo is gone: taking
// away the access the factory had to it.
func (u *uninstaller) repoManual() []web.APIUninstallStep {
	rc := repoFor(u.cfg, u.repo)
	if rc.GitLab() {
		return []web.APIUninstallStep{{Text: "If you made a project access token for " + u.repo + " alone, revoke it: the project's Settings, Access tokens.",
			URL: rc.WebURL() + "/-/settings/access_tokens"}}
	}
	var out []web.APIUninstallStep
	owner, _, _ := strings.Cut(u.repo, "/")
	if inst := os.Getenv("FACTORY_GITHUB_INSTALLATION_ID"); inst != "" {
		out = append(out, web.APIUninstallStep{Text: "To stop the GitHub App reaching " + u.repo + ", take it off the app's repository access: its installation, Repository access.",
			URL: githubWeb + "/organizations/" + url.PathEscape(owner) + "/settings/installations/" + url.PathEscape(inst)},
			web.APIUninstallStep{Text: "For an install on your own account, it's here.", URL: githubWeb + "/settings/installations/" + url.PathEscape(inst)})
	}
	if os.Getenv("GITHUB_TOKEN") != "" && os.Getenv("FACTORY_GITHUB_APP_ID") == "" {
		out = append(out, web.APIUninstallStep{Text: "If you made the GitHub token (GITHUB_TOKEN) for " + u.repo + " alone, revoke it.", URL: githubWeb + "/settings/personal-access-tokens"})
	}
	return out
}

// Remove removes the labels and job images, and with data the data.
// With repo, it's that repo's, and the repo leaves factory.yaml first: if
// that fails, nothing goes, and once it's done, removing it again finishes
// what's left. New work is held off throughout, and running work is
// looked for again before each step that a job could be using.
func (u *uninstaller) Remove(ctx context.Context, data bool, repo string) (web.APIUninstallResult, error) {
	u = u.scoped(repo)
	if why := u.repoRefused(); why != "" {
		return web.APIUninstallResult{}, errors.New(why)
	}
	if !u.active.CompareAndSwap(false, true) {
		return web.APIUninstallResult{}, errors.New("already removing: wait for it to finish")
	}
	defer u.active.Store(false)
	if u.hold != nil {
		release := u.hold()
		defer release()
	}
	busy := func() string {
		if u.busy == nil {
			return ""
		}
		return u.busy(ctx)
	}
	if why := busy(); why != "" {
		return web.APIUninstallResult{}, errors.New(why)
	}
	// Data asked for that can't go refuses the lot, before anything is.
	dir := u.dataDir()
	_, statErr := os.Stat(dir)
	data = data && statErr == nil && u.repo == ""
	if data {
		if err := u.dataRemovable(dir); err != nil {
			return web.APIUninstallResult{}, fmt.Errorf("nothing was removed: %w", err)
		}
	}
	res := web.APIUninstallResult{Done: []web.APIUninstallDone{}, Manual: u.manual()}
	add := func(what string, err error) {
		d := web.APIUninstallDone{What: what, OK: err == nil}
		if err != nil {
			d.Error = err.Error()
		}
		res.Done = append(res.Done, d)
	}
	if u.listed() {
		if err := u.drop(ctx, u.repo); err != nil {
			return web.APIUninstallResult{}, fmt.Errorf("nothing was removed: couldn't take %s out of %s: %w", u.repo, filepath.Base(u.cfgPath), err)
		}
		add(u.repo+" from "+filepath.Base(u.cfgPath), nil)
	}
	if u.serving && u.repo == "" {
		res.Manual = append([]web.APIUninstallStep{{Text: "Stop the factory now. While it runs it puts labels back as it works, and it picks up no new issues without its " + u.cfg.Intake.Label + " label."}}, res.Manual...)
	}
	// Labels: one read of each repo, no counting.
	for _, r := range u.scanLabels(ctx) {
		if r.err != nil {
			add("labels on "+r.repo, fmt.Errorf("couldn't read them: %w", r.err))
			continue
		}
		for _, name := range r.remove {
			lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			add("label "+name+" on "+r.repo, r.f.DeleteLabel(lctx, name))
			cancel()
		}
	}
	if why := busy(); why != "" {
		add("job images and data", errors.New("not removed: "+why))
		return res, nil
	}
	images, _, note := u.images()
	if note != "" && u.cfg.Runner.Kind == "docker" {
		add("job images", errors.New(note))
	}
	for _, im := range images {
		var b bytes.Buffer
		err := runCmd(&b, "docker", "image", "rm", im.Ref)
		if err != nil {
			err = errors.New(cmdError(b.String(), err))
		}
		add("image "+im.Ref, err)
	}
	if u.repo != "" {
		for _, f := range u.repoFiles() {
			add("data "+f, os.RemoveAll(f))
		}
		return res, nil
	}
	if !data {
		return res, nil
	}
	if why := busy(); why != "" {
		add("data "+dir, errors.New("not removed: "+why))
		return res, nil
	}
	if err := u.dataRemovable(dir); err != nil {
		add("data "+dir, err)
		return res, nil
	}
	if !u.serving {
		add("data "+dir, os.RemoveAll(dir))
		return res, nil
	}
	for _, s := range servingData {
		if _, err := os.Stat(filepath.Join(dir, s)); err == nil {
			add("data "+filepath.Join(dir, s), os.RemoveAll(filepath.Join(dir, s)))
		}
	}
	return res, nil
}

// dirSize is the bytes in the files under dir.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func humanBytes(n int64) string {
	const unit = 1000 // as docker shows sizes
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// runningJobs says why removing must wait for the jobs in st, or "".
func runningJobs(ctx context.Context, st *store.Store) string {
	jobs, err := st.ListJobs(ctx, store.JobFilter{State: state.Running, Limit: 20})
	if err != nil {
		return "Couldn't check for running jobs: " + err.Error()
	}
	if len(jobs) == 0 {
		return ""
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, fmt.Sprintf("#%d", j.ID))
	}
	return fmt.Sprintf("Not now: %d job(s) are running (%s). Wait for them to finish, or cancel them from the dashboard, then try again.", len(jobs), strings.Join(ids, ", "))
}

// factoryAnswers says whether something serves the dashboard's address:
// a factory running from this config. Tests replace it.
var factoryAnswers = func(listen string) bool {
	c, err := net.DialTimeout("tcp", listen, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// uninstall is `factory uninstall`: it lists what it would remove, and
// removes it once you type remove (or the repo's name). With --repo, it's
// one repo's share, and the repo leaves factory.yaml.
func uninstall(ctx context.Context, out io.Writer, in io.Reader, cfgPath string, cfg *config.Config, envPath string, args []string) error {
	data, dry, repo := false, false, ""
	usage := errors.New("usage: factory uninstall [--data] [--dry-run], or factory uninstall --repo owner/name [--dry-run]")
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--data":
			data = true
		case a == "--dry-run":
			dry = true
		case a == "--repo" && i+1 < len(args):
			i++
			repo = args[i]
		case strings.HasPrefix(a, "--repo="):
			repo = strings.TrimPrefix(a, "--repo=")
		default:
			return usage
		}
	}
	if data && repo != "" {
		return usage
	}
	// A running factory would put labels back, and holds its database.
	if factoryAnswers(cfg.Web.Listen) {
		return fmt.Errorf("something is listening on %s, the dashboard's address: stop the factory first if it's running, or remove it from the dashboard (Settings, Remove the factory)", cfg.Web.Listen)
	}
	busy := func(ctx context.Context) string {
		if runs := liveRuns(cfg.Path(cfg.DataDir)); len(runs) > 0 {
			return "Not now: " + joinAnd(runs) + " is running. Wait for it to finish, then try again."
		}
		db := filepath.Join(cfg.Path(cfg.DataDir), "factory.db")
		if _, err := os.Stat(db); err != nil {
			return ""
		}
		st, err := store.Open(db)
		if err != nil {
			return "Couldn't open the database to check for running jobs: " + err.Error()
		}
		defer func() { _ = st.Close() }()
		return runningJobs(ctx, st)
	}
	u := newUninstaller(cfg, cfgPath, envPath, false, busy)
	if repo != "" {
		repo = u.scoped(repo).repo
	}
	fmt.Fprintln(out, "Looking for what the factory made...")
	p := u.Plan(ctx, repo)
	printPlan(out, p, data || repo != "")
	if repo != "" && p.Refused == "" {
		fmt.Fprintln(out, "\nfactory.yaml:")
		if p.Listed {
			fmt.Fprintf(out, "  %s leaves repos: and its workspace\n", repo)
		} else {
			fmt.Fprintf(out, "  %s isn't in it\n", repo)
		}
	}
	if p.Refused != "" {
		return errors.New(p.Refused)
	}
	if data && p.Data.Blocked != "" {
		return fmt.Errorf("nothing was removed: %s", p.Data.Blocked)
	}
	if dry {
		fmt.Fprintln(out, "\nNothing was removed (--dry-run).")
		return nil
	}
	if len(p.Labels) == 0 && len(p.Images) == 0 && (!data && repo == "" || !p.Data.Exists) && !p.Listed {
		fmt.Fprintln(out, "\nThere's nothing to remove.")
		return nil
	}
	name := repo
	if repos := cfg.RepoNames(); name == "" && len(repos) == 1 {
		name = repos[0]
	}
	want := "remove"
	if name != "" {
		want = "remove (or " + name + ")"
	}
	fmt.Fprintf(out, "\nType %s to remove all of the above: ", want)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "remove" && (name == "" || line != name) {
		fmt.Fprintln(out, "\nNothing was removed.")
		return nil
	}
	res, err := u.Remove(ctx, data, repo)
	if err != nil {
		return err
	}
	failed := 0
	fmt.Fprintln(out)
	for _, d := range res.Done {
		if d.OK {
			fmt.Fprintf(out, "  removed  %s\n", d.What)
		} else {
			failed++
			fmt.Fprintf(out, "  FAILED   %s: %s\n", d.What, d.Error)
		}
	}
	printManual(out, res.Manual)
	if failed > 0 {
		return fmt.Errorf("%d of %d couldn't be removed; see above", failed, len(res.Done))
	}
	return nil
}

func printPlan(out io.Writer, p web.APIUninstall, data bool) {
	fmt.Fprintln(out, "\nLabels (deleting a label takes it off every issue and PR that has it):")
	if len(p.Labels) == 0 && len(p.Repos) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, l := range p.Labels {
		open := fmt.Sprintf("%d open", l.Open)
		switch {
		case l.Open < 0:
			open = "open count unknown"
		case l.OpenCapped:
			open = fmt.Sprintf("%d+ open", l.Open)
		}
		fmt.Fprintf(out, "  %s  %s  (%s)\n", l.Repo, l.Name, open)
	}
	for _, r := range p.Repos {
		fmt.Fprintf(out, "  %s: couldn't read its labels: %s\n", r.Repo, r.Error)
	}
	fmt.Fprintln(out, "\nJob images:")
	switch {
	case p.ImagesNote != "":
		fmt.Fprintln(out, "  "+p.ImagesNote)
	case len(p.Images) == 0:
		fmt.Fprintln(out, "  none")
	}
	for _, im := range p.Images {
		what := im.Ref
		if im.Dangling {
			what += " (an old copy)"
		}
		fmt.Fprintf(out, "  %s  %s\n", what, im.Size)
	}
	if len(p.Kept) > 0 {
		fmt.Fprintln(out, "\nLeft alone:")
		for _, k := range p.Kept {
			fmt.Fprintf(out, "  %s: %s\n", k.What, k.Why)
		}
	}
	fmt.Fprintln(out, "\nData:")
	switch {
	case !p.Data.Exists:
		fmt.Fprintln(out, "  "+p.Data.Note)
	case p.Data.Blocked != "":
		fmt.Fprintf(out, "  kept: %s\n", p.Data.Blocked)
	case data:
		fmt.Fprintf(out, "  removed too: %s (%s)\n", p.Data.What, p.Data.Size)
	default:
		fmt.Fprintf(out, "  kept: %s (%s). Add --data to remove it too.\n", p.Data.Path, p.Data.Size)
	}
	if p.Refused == "" {
		printManual(out, p.Manual)
	}
}

func printManual(out io.Writer, steps []web.APIUninstallStep) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintln(out, "\nLeft for you to do:")
	for _, s := range steps {
		if s.URL != "" {
			fmt.Fprintf(out, "  - %s\n    %s\n", s.Text, s.URL)
		} else {
			fmt.Fprintf(out, "  - %s\n", s.Text)
		}
	}
}
