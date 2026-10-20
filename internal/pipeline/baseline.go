package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// A lint baseline makes a lint check that fails on the base branch pass
// there by recording today's offenses as accepted, so only new ones fail
// a change. The factory makes it without a model: it runs the linter's
// own baseline command on the base branch and opens a pull request with
// just the files that command writes, for a person to merge.

// BaselineBranch is the branch a lint baseline is pushed to.
const BaselineBranch = "factory/lint-baseline"

// linter is one linter the factory can baseline.
type linter struct {
	name   string
	runner string   // how the lint command runs it
	cmd    string   // writes the baseline
	files  []string // all it may change
}

// baselineFor is how to baseline the lint command, or nil when its
// linter has no baseline the factory knows.
func baselineFor(lint string) *linter {
	if !strings.Contains(lint, "rubocop") {
		return nil
	}
	runner := "bundle exec rubocop"
	if strings.HasPrefix(strings.TrimSpace(lint), "bin/rubocop") {
		runner = "bin/rubocop"
	}
	// --no-exclude-limit lists every file, so no cop is turned off for
	// the whole repo; no counts or timestamp, so a rerun changes nothing.
	return &linter{name: "RuboCop", runner: runner, cmd: runner + " --auto-gen-config --no-exclude-limit --no-offense-counts --no-auto-gen-timestamp",
		files: []string{".rubocop.yml", ".rubocop_todo.yml"}}
}

// CanBaseline says whether the repo's lint check is one the factory can
// baseline.
func (p *Pipeline) CanBaseline(repo string) bool {
	g := p.repoConf(repo).Gates
	return g != nil && baselineFor(g.Commands["lint"]) != nil
}

// LintBaseline records the lint offenses on repo's base branch as a
// baseline and opens a pull request with it; it returns the pull
// request's address. The lint check must pass with the baseline, or
// nothing is pushed.
func (p *Pipeline) LintBaseline(ctx context.Context, repo string) (string, error) {
	rc := p.repoConf(repo)
	if rc.Gates == nil {
		return "", errors.New("the repo has no checks of its own")
	}
	lint := rc.Gates.Commands["lint"]
	l := baselineFor(lint)
	if l == nil {
		return "", fmt.Errorf("the factory can baseline RuboCop for now, not `%s`", lint)
	}
	data := p.Cfg.Path(p.Cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(data, "readiness-baseline-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g := p.git(repo)
	// A fresh branch off the base, with a clean clone to commit from.
	ws, err := g.Prepare(ctx, dir, rc.BaseBranch, BaselineBranch, true, true)
	if err != nil {
		return "", fmt.Errorf("cloning %s: %w", repo, err)
	}
	// The linter runs in an export, so what install leaves behind never
	// reaches the commit.
	export := filepath.Join(dir, "export")
	if err := g.Export(ctx, ws.Repo, ws.BaseSHA, export); err != nil {
		return "", err
	}
	if err := p.CopyFiles(ctx, repo, export); err != nil {
		return "", err
	}
	if rc.Gates.Install != "" {
		if code, out, err := p.Exec(ctx, repo, export, rc.Gates.Install, true); err != nil || code != 0 {
			return "", runErr("install", code, out, err)
		}
	}
	if code, out, err := p.Exec(ctx, repo, export, l.cmd, false); err != nil || code > 1 {
		return "", runErr(l.name+"'s baseline", code, out, err) // 1 is "offenses found", as it records them
	}
	if err := p.closeGaps(ctx, repo, export, l, lint); err != nil {
		return "", err
	}
	for _, f := range l.files {
		b, err := os.ReadFile(filepath.Join(export, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(ws.Work, f), b, 0o644); err != nil {
			return "", err
		}
	}
	sha, changed, err := g.Ingest(ctx, ws, "Accept today's "+l.name+" offenses as a baseline\n\nMade by the factory with `"+l.cmd+"`.")
	if err != nil {
		return "", err
	}
	if !changed {
		return "", fmt.Errorf("the baseline is already on %s", rc.BaseBranch)
	}
	if err := g.Push(ctx, ws.Repo, sha, BaselineBranch, true); err != nil {
		return "", fmt.Errorf("pushing %s: %w", BaselineBranch, err)
	}
	gh := p.gh(repo)
	if pr, err := gh.FindPR(ctx, BaselineBranch); err == nil && pr != nil {
		return pr.URL, nil // the push updated it
	}
	pr, err := gh.CreatePR(ctx, "Accept today's "+l.name+" offenses as a baseline", baselineBody(l, lint, rc.BaseBranch), BaselineBranch, rc.BaseBranch, false)
	if err != nil {
		return "", err
	}
	return pr.URL, nil
}

func baselineBody(l *linter, lint, base string) string {
	return fmt.Sprintf("`%s` fails on `%s` today, and the factory requires it to pass, so every change would fail it.\n\n"+
		"This records today's offenses as accepted with `%s`, so `%s` passes and only new offenses fail a change. "+
		"It changes only %s; no code changes, and no cop is turned off for the whole repo. "+
		"When the generated lists would replace a cop's own excludes (Style/Documentation skips test/ by default), `.rubocop.yml` gets `inherit_mode: merge: [Exclude]` so they add to them instead, "+
		"and any file still flagged after that (such as a `rubocop:disable` comment the baseline made redundant) is listed too.\n\n"+
		"To pay the old offenses down later, delete a cop's entry from `.rubocop_todo.yml` and fix what it flags, one cop at a time.\n\n"+
		"_Opened by the factory from its Setup page. It ran the baseline on `%s` and checked `%s` passes with it._",
		lint, base, l.cmd, lint, "`"+strings.Join(l.files, "` and `")+"`", base, lint)
}

func runErr(what string, code int, out string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s exited %d:\n%s", what, code, tail(out, 2000))
}

// closeGaps makes lint pass with the baseline its linter wrote. RuboCop's
// generated Exclude lists replace a cop's default ones (Style/Documentation
// skips test/ by default), so files it never flagged start failing:
// .rubocop.yml is told to merge them, unless it already says how. What
// still fails, such as a rubocop:disable comment the baseline made
// redundant, is added to the baseline file by file. Then lint must pass.
func (p *Pipeline) closeGaps(ctx context.Context, repo, dir string, l *linter, lint string) error {
	passes := func() (bool, error) {
		code, out, err := p.Exec(ctx, repo, dir, lint, false)
		if err != nil || code > 1 {
			return false, runErr("lint", code, out, err)
		}
		return code == 0, nil
	}
	if ok, err := passes(); ok || err != nil {
		return err
	}
	cfg := filepath.Join(dir, ".rubocop.yml")
	b, err := os.ReadFile(cfg)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !strings.Contains(string(b), "inherit_mode:") {
		merge := "# The baseline's Exclude lists add to each cop's own, not replace them.\ninherit_mode:\n  merge:\n    - Exclude\n\n"
		if err := os.WriteFile(cfg, append([]byte(merge), b...), 0o644); err != nil {
			return err
		}
		if ok, err := passes(); ok || err != nil {
			return err
		}
	}
	code, out, err := p.Exec(ctx, repo, dir, l.runner+" --format json", false)
	if err != nil || code > 1 {
		return runErr("listing what still fails", code, out, err)
	}
	left, err := rubocopOffenses(out)
	if err != nil {
		return err
	}
	if err := addExcludes(filepath.Join(dir, ".rubocop_todo.yml"), left); err != nil {
		return err
	}
	if code, out, err := p.Exec(ctx, repo, dir, lint, false); err != nil || code != 0 {
		return fmt.Errorf("`%s` still fails with the baseline, so nothing was pushed: %w", lint, runErr("lint", code, out, err))
	}
	return nil
}

// rubocopOffenses reads RuboCop's JSON report: each cop's files.
func rubocopOffenses(out string) (map[string][]string, error) {
	if i := strings.IndexByte(out, '{'); i > 0 {
		out = out[i:] // anything printed before the report
	}
	var r struct {
		Files []struct {
			Path     string `json:"path"`
			Offenses []struct {
				Cop string `json:"cop_name"`
			} `json:"offenses"`
		} `json:"files"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&r); err != nil {
		return nil, fmt.Errorf("reading RuboCop's report: %w", err)
	}
	left := map[string][]string{}
	for _, f := range r.Files {
		for _, o := range f.Offenses {
			if !slices.Contains(left[o.Cop], f.Path) {
				left[o.Cop] = append(left[o.Cop], f.Path)
			}
		}
	}
	return left, nil
}

// addExcludes adds files to each cop's Exclude in a RuboCop config,
// keeping what's there; a cop it turns off stays off.
func addExcludes(path string, add map[string][]string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	get := func(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				return m.Content[i+1]
			}
		}
		v := &yaml.Node{Kind: kind}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
		return v
	}
	cops := slices.Sorted(maps.Keys(add))
	for _, cop := range cops {
		c := get(root, cop, yaml.MappingNode)
		if e := get(c, "Enabled", yaml.ScalarNode); e.Value == "false" {
			continue
		} else if e.Value == "" {
			c.Content = c.Content[:len(c.Content)-2] // not there: leave it so
		}
		ex := get(c, "Exclude", yaml.SequenceNode)
		for _, f := range add[cop] {
			if !slices.ContainsFunc(ex.Content, func(n *yaml.Node) bool { return n.Value == f }) {
				ex.Content = append(ex.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f, Style: yaml.SingleQuotedStyle})
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	// The encoder drops the blank line between cops: put it back before
	// each top-level entry (or the comments heading it), for a readable diff.
	var out strings.Builder
	prev := ""
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line != "" && line[0] != ' ' && line != "\n" && strings.HasPrefix(prev, " ") {
			out.WriteString("\n")
		}
		out.WriteString(line)
		prev = line
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}
