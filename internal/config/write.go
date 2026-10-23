package config

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// WriteRepos replaces the config's top-level repo: or repos: entry, and
// whatever belongs to it, with repos. Comments inside that entry are
// dropped; the rest of the file, comments included, stays as it is.
func WriteRepos(path string, repos []Repo) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var block string
	if bare, _ := repos[0].MarshalYAML(); len(repos) == 1 && bare == repos[0].Name {
		// Through YAML, so a name that needs quoting gets it.
		one, err := yaml.Marshal(struct {
			Repo string `yaml:"repo"`
		}{repos[0].Name})
		if err != nil {
			return err
		}
		block = string(one)
	} else {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(repos); err != nil {
			return err
		}
		block = "repos:\n"
		for _, l := range strings.SplitAfter(strings.TrimRight(buf.String(), "\n")+"\n", "\n") {
			if l != "" {
				block += "  " + l
			}
		}
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out strings.Builder
	found := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		isRepo := strings.HasPrefix(l, "repo:") || strings.HasPrefix(l, "repos:")
		if found || !isRepo {
			out.WriteString(l)
			continue
		}
		found = true
		out.WriteString(block)
		// Skip the entry's own lines, and blank or comment lines between
		// them, up to the next top-level key.
		for i+1 < len(lines) && continues(lines[i+1:]) {
			i++
		}
	}
	if !found {
		// A new factory.yaml names none: the block takes the place of the
		// example's commented-out repo: line, or goes before the first key.
		out.Reset()
		at := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "# repo:") || strings.HasPrefix(l, "#repo:") })
		if at < 0 {
			at = slices.IndexFunc(lines, func(l string) bool {
				t := strings.TrimSpace(l)
				return t != "" && !strings.HasPrefix(t, "#")
			})
		}
		if at < 0 {
			at = len(lines)
		}
		for i, l := range lines {
			if i == at {
				out.WriteString(block)
				if strings.HasPrefix(l, "#") && strings.Contains(l, "repo:") {
					continue
				}
			}
			out.WriteString(l)
		}
		if at == len(lines) {
			out.WriteString(block)
		}
	}
	return WriteFile(path, []byte(out.String()))
}

// continues reports whether lines[0] still belongs to the YAML entry
// above it: it is indented or a "- item", or it is blank or a comment
// with such a line after it.
func continues(lines []string) bool {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "-")
	}
	return false
}

// WriteFile replaces the config file in one step, after checking that
// the new text is a valid config.
func WriteFile(path string, out []byte) error {
	dir, _ := filepath.Abs(filepath.Dir(path))
	if _, err := ParseIn(out, dir); err != nil {
		return fmt.Errorf("updating %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".setup-tmp"
	if err := os.WriteFile(tmp, out, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteWorkspaces replaces the config's top-level workspaces: entry with
// ws, or removes it for none, as WriteRepos does repos:. A new one goes
// after the repos.
func WriteWorkspaces(path string, ws map[string]Workspace) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	block := ""
	if len(ws) > 0 {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(map[string]map[string]Workspace{"workspaces": ws}); err != nil {
			return err
		}
		block = buf.String()
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out strings.Builder
	found := false
	for i := 0; i < len(lines); i++ {
		if found || !strings.HasPrefix(lines[i], "workspaces:") {
			out.WriteString(lines[i])
			continue
		}
		found = true
		out.WriteString(block)
		for i+1 < len(lines) && continues(lines[i+1:]) {
			i++
		}
	}
	if !found && block != "" {
		// After the repo: or repos: entry and what belongs to it.
		out.Reset()
		placed := false
		for i := 0; i < len(lines); i++ {
			out.WriteString(lines[i])
			if placed || !(strings.HasPrefix(lines[i], "repo:") || strings.HasPrefix(lines[i], "repos:")) {
				continue
			}
			for i+1 < len(lines) && continues(lines[i+1:]) {
				i++
				out.WriteString(lines[i])
			}
			out.WriteString(block)
			placed = true
		}
		if !placed {
			out.WriteString(block)
		}
	}
	return WriteFile(path, []byte(out.String()))
}

// SetWorkspaceTrust gives workspace ws a trust level in the config file,
// as the dashboard's trust screen sets one: up_to and max_size from a, and
// self_start as s (keeping the workspace's own sources and limits if it
// has them), or none for s nil. A repo's own workspace is written out by
// its name. The levels set on its repos go, so the workspace's is theirs;
// a repo's merge method and breaker move to the workspace when it has none.
func SetWorkspaceTrust(path, ws string, a *AutoMerge, s *SelfStart) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	w, ok := cfg.WorkspaceConf(ws)
	if !ok {
		return fmt.Errorf("%s isn't a workspace in %s", ws, path)
	}
	next := *a
	if next.UpTo == "none" {
		next.MaxSize = ""
	}
	if w.AutoMerge != nil {
		next.Method, next.Breaker = w.AutoMerge.Method, w.AutoMerge.Breaker
	}
	repos := slices.Clone(cfg.Repos)
	for i, r := range repos {
		if cfg.WorkspaceOf(r.Name) != ws {
			continue
		}
		if r.AutoMerge != nil {
			next.Method, next.Breaker = cmp.Or(next.Method, r.AutoMerge.Method), cmp.Or(next.Breaker, r.AutoMerge.Breaker)
		}
		repos[i].AutoMerge, repos[i].SelfStart = nil, nil
	}
	w.AutoMerge = &next
	switch {
	case s == nil:
		w.SelfStart = nil
	case w.SelfStart == nil || len(w.SelfStart.Sources) == 0:
		w.SelfStart = s
	}
	all := maps.Clone(cfg.Workspaces)
	if all == nil {
		all = map[string]Workspace{}
	}
	all[ws] = w
	// The workspace's level first, so the file is valid between the writes.
	if err := WriteWorkspaces(path, all); err != nil {
		return err
	}
	return WriteRepos(path, repos)
}
