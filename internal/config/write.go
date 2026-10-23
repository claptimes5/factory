package config

import (
	"bytes"
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
			if placed || !strings.HasPrefix(lines[i], "repo:") && !strings.HasPrefix(lines[i], "repos:") {
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
// self_start as s, or none for s nil. A repo's own workspace is written
// out by its name. Its repos set stricter by hand keep their level; the
// others lose theirs, keeping their merge method and breaker. With s, the
// workspace keeps its own self_start, or takes over a repo's, sources and
// limits and all; otherwise s.
func SetWorkspaceTrust(path, ws string, a *AutoMerge, s *SelfStart) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	w, ok := cfg.WorkspaceConf(ws)
	if !ok {
		return fmt.Errorf("%s isn't a workspace in %s", ws, path)
	}
	next := AutoMerge{UpTo: a.UpTo, MaxSize: a.MaxSize}
	if next.UpTo == "none" {
		next.MaxSize = ""
	}
	if w.AutoMerge != nil {
		next.Method, next.Breaker = w.AutoMerge.Method, w.AutoMerge.Breaker
	}
	repos := slices.Clone(cfg.Repos)
	took := -1 // the repo whose self_start the workspace takes over
	for i, r := range repos {
		if cfg.WorkspaceOf(r.Name) != ws {
			continue
		}
		if hasLevel(r.AutoMerge) && !stricter(r.AutoMerge, &next) {
			am := *r.AutoMerge
			am.UpTo, am.MaxSize = "", ""
			repos[i].AutoMerge = &am
			if am.Method == "" && am.Breaker == nil {
				repos[i].AutoMerge = nil
			}
		}
		switch {
		case s == nil:
			repos[i].SelfStart = nil
		case took < 0 && (w.SelfStart == nil || len(w.SelfStart.Sources) == 0) && r.SelfStart != nil && len(r.SelfStart.Sources) > 0:
			took = i
		}
	}
	w.AutoMerge = &next
	switch {
	case s == nil:
		w.SelfStart = nil
	case w.SelfStart != nil && len(w.SelfStart.Sources) > 0:
	case took >= 0:
		w.SelfStart, repos[took].SelfStart = repos[took].SelfStart, nil
	default:
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

// SetRepoWorkspace puts repo in workspace ws in the config file: one
// factory.yaml names, which it then makes (from a repo's own, that repo
// and this one), or "" for a workspace of its own. It leaves the one it
// was in; one left empty goes, and one left with a single repo no longer
// changes together. A repo whose level was its workspace's keeps it as
// its own, so moving it never lets it merge more; the workspaces of
// other repos keep their names.
func SetRepoWorkspace(path, repo, ws string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	rc, ok := cfg.RepoConf(repo)
	if !ok {
		return fmt.Errorf("%s isn't in %s", repo, path)
	}
	if ws != "" && !wsName.MatchString(ws) {
		return fmt.Errorf("workspace names are lowercase letters, digits and -, starting with a letter: %q", ws)
	}
	from := cfg.WorkspaceOf(rc.Name)
	_, named := cfg.Workspaces[from]
	if ws == from || ws == "" && !named {
		return nil // where it is already
	}
	names := cfg.workspaces()
	delete(names, strings.ToLower(rc.Name))
	all := maps.Clone(cfg.Workspaces)
	if all == nil {
		all = map[string]Workspace{}
	}
	if w, ok := all[from]; ok {
		w.Repos = slices.DeleteFunc(slices.Clone(w.Repos), func(r string) bool { return strings.EqualFold(r, rc.Name) })
		switch {
		case len(w.Repos) == 0:
			delete(all, from)
		case len(w.Repos) < 2:
			w.ChangeTogether = false
			all[from] = w
		default:
			all[from] = w
		}
	}
	if ws != "" {
		w, ok := all[ws]
		if !ok {
			w.Repos = slices.DeleteFunc(cfg.WorkspaceRepos(ws), func(r string) bool { return strings.EqualFold(r, rc.Name) })
		}
		w.Repos = append(w.Repos, rc.Name)
		all[ws] = w
	}
	keepNames(cfg, all, names)
	// Its level came from the workspace it leaves: it keeps it as its own.
	if lv := cfg.Workspaces[from].AutoMerge; lv != nil || cfg.lowestLevel(from) != nil {
		for i, r := range cfg.Repos {
			if strings.EqualFold(r.Name, rc.Name) && !hasLevel(r.AutoMerge) {
				am := AutoMerge{UpTo: "none"}
				if rc.AutoMerge != nil {
					am.UpTo, am.MaxSize = rc.AutoMerge.UpTo, rc.AutoMerge.MaxSize
				}
				if r.AutoMerge != nil {
					am.Method, am.Breaker = r.AutoMerge.Method, r.AutoMerge.Breaker
				}
				repos := slices.Clone(cfg.Repos)
				repos[i].AutoMerge = &am
				if err := WriteRepos(path, repos); err != nil {
					return err
				}
			}
		}
	}
	return WriteWorkspaces(path, all)
}

// keepNames writes out, under the name it had, each repo's own workspace
// whose name would otherwise change with all (as web-2 becomes web once
// web is gone): chats and spend go by name. names are each repo's
// workspace before, by lower-cased repo.
func keepNames(cfg *Config, all map[string]Workspace, names map[string]string) {
	after := &Config{Repos: cfg.Repos, Workspaces: all}
	for _, r := range cfg.Repos {
		key := strings.ToLower(r.Name)
		old, ok := names[key]
		if _, named := cfg.Workspaces[old]; !ok || named {
			continue
		}
		if _, taken := all[old]; !taken && after.WorkspaceOf(r.Name) != old {
			all[old] = Workspace{Repos: []string{r.Name}}
		}
	}
}

// RenameWorkspace renames workspace from to to in the config file; a
// repo's own is written out under the new name. The others keep theirs.
func RenameWorkspace(path, from, to string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	w, ok := cfg.WorkspaceConf(from)
	switch {
	case !ok:
		return fmt.Errorf("%s isn't a workspace in %s", from, path)
	case !wsName.MatchString(to):
		return fmt.Errorf("workspace names are lowercase letters, digits and -, starting with a letter: %q", to)
	case to != from && slices.Contains(cfg.WorkspaceNames(), to):
		return fmt.Errorf("there's a workspace named %s already", to)
	}
	names := cfg.workspaces()
	for _, r := range w.Repos {
		delete(names, strings.ToLower(r))
	}
	all := maps.Clone(cfg.Workspaces)
	if all == nil {
		all = map[string]Workspace{}
	}
	delete(all, from)
	all[to] = w
	keepNames(cfg, all, names)
	return WriteWorkspaces(path, all)
}

// KeepWorkspaceNames writes out the workspaces of repos still in the
// config file under the names they had (names, by repo, before a change
// such as removing a repo), where their names would change.
func KeepWorkspaceNames(path string, names map[string]string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	lower := map[string]string{}
	for r, n := range names {
		lower[strings.ToLower(r)] = n
	}
	all := maps.Clone(cfg.Workspaces)
	if all == nil {
		all = map[string]Workspace{}
	}
	n := len(all)
	keepNames(cfg, all, lower)
	if len(all) == n {
		return nil
	}
	return WriteWorkspaces(path, all)
}
