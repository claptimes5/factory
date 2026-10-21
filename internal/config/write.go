package config

import (
	"bytes"
	"fmt"
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

// SetRepoTrust gives repo its own auto_merge and self_start in the config
// file, as the dashboard's trust screen sets a level: up_to and max_size
// from a, the merge method and breaker as the repo has them now; self_start
// as s, keeping the repo's own sources and limits if it has them, or none
// for s nil.
func SetRepoTrust(path, repo string, a *AutoMerge, s *SelfStart) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	repos := slices.Clone(cfg.Repos)
	for i, r := range repos {
		if !strings.EqualFold(r.Name, repo) {
			continue
		}
		next := *a
		if cur, _ := cfg.RepoConf(r.Name); cur.AutoMerge != nil {
			next.Method, next.Breaker = cur.AutoMerge.Method, cur.AutoMerge.Breaker
		}
		if next.UpTo == "none" {
			next.MaxSize = ""
		}
		repos[i].AutoMerge = &next
		switch {
		case s == nil:
			repos[i].SelfStart = nil
		case r.SelfStart == nil || len(r.SelfStart.Sources) == 0:
			repos[i].SelfStart = s
		}
		return WriteRepos(path, repos)
	}
	return fmt.Errorf("%s isn't in %s", repo, path)
}
