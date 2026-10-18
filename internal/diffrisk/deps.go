package diffrisk

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// A new dependency is a name a manifest or lockfile lists after the
// change and didn't before, or one whose source changed: an npm alias to
// another package, a git URL, a local path, a replace or patch directive,
// another registry or index. A version bump from the usual registry
// doesn't count. Each parser reads only names and sources, line by line
// where the format allows, so a file it half understands still yields
// what it can. A manifest of an ecosystem it doesn't read counts, when
// the change touches it, as a new dependency.

// dep is one dependency: its name, and where it comes from when that
// isn't the ecosystem's usual registry ("" when it is).
type dep struct{ name, from string }

func (d dep) String() string {
	if d.from == "" {
		return d.name
	}
	return d.name + " from " + d.from
}

// manifests read the dependencies of each file they know, by path;
// manifests before lockfiles, so a dependency both add is credited to
// the manifest.
var manifests = []struct {
	match func(p string) bool
	deps  func(body string) ([]dep, error)
}{
	{base("go.mod"), goMod},
	{base("package.json"), packageJSON},
	{isRequirements, requirements},
	{base("pyproject.toml"), pyproject},
	{base("Pipfile"), pipfile},
	{base("Cargo.toml"), cargoToml},
	{base("Gemfile"), gemfile},
	{func(p string) bool { return strings.HasSuffix(p, ".gemspec") }, names(regexp.MustCompile(`\.add_(?:runtime_|development_)?dependency\s*\(?\s*["']([^"']+)["']`))},
	{base("package-lock.json", "npm-shrinkwrap.json"), npmLock},
	{base("yarn.lock"), yarnLock},
	{base("pnpm-lock.yaml"), pnpmLock},
	{base("Pipfile.lock"), pipfileLock},
	{base("poetry.lock"), lockPackages(func(string) bool { return true }, "")},
	{base("uv.lock"), lockPackages(func(key string) bool { return key != "editable" && key != "virtual" }, "https://pypi.org/simple")},
	// A workspace crate has no source.
	{base("Cargo.lock"), lockPackages(func(key string) bool { return key != "" }, "registry+https://github.com/rust-lang/crates.io-index")},
	{base("Gemfile.lock"), gemLock},
}

// unread are dependency manifests of ecosystems the factory doesn't read:
// any change to one counts as a new dependency.
var unread = []string{"composer.json", "setup.py", "setup.cfg", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle",
	"settings.gradle.kts", "packages.config", "Directory.Packages.props", "mix.exs", "Package.swift", "Podfile", "deno.json", "deno.jsonc", "go.work"}

func isUnread(p string) bool {
	b := path.Base(p)
	return slices.Contains(unread, b) || strings.HasSuffix(b, ".csproj") || strings.HasSuffix(b, ".fsproj") || strings.HasSuffix(b, ".vbproj")
}

func base(names ...string) func(string) bool {
	return func(p string) bool { return slices.Contains(names, path.Base(p)) }
}

var reqName = regexp.MustCompile(`^(requirements|constraints)[\w.-]*\.(txt|in)$`)

// isRequirements is a pip requirements file: requirements*.txt or .in,
// constraints*.txt, or a .txt or .in under a requirements/ directory.
func isRequirements(p string) bool {
	b := path.Base(p)
	if reqName.MatchString(b) {
		return true
	}
	ext := path.Ext(b)
	return (ext == ".txt" || ext == ".in") && slices.Contains(strings.Split(path.Dir(p), "/"), "requirements")
}

// lockfiles are generated: their lines don't count toward a change's size.
var lockfiles = []string{"go.sum", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "bun.lock",
	"Pipfile.lock", "poetry.lock", "uv.lock", "Cargo.lock", "Gemfile.lock", "composer.lock"}

func isLockfile(p string) bool { return slices.Contains(lockfiles, path.Base(p)) }

// newDependencies are the dependencies the change adds, each named once,
// and the manifests it touches that aren't read. What it can't read it
// records on a.
func newDependencies(ch Change, a *Assessment) ([]Item, []string) {
	var out []Item
	var unknown []string
	seen := map[string]bool{}
	for _, f := range ch.Files {
		if f.Status != "D" && isUnread(f.Path) {
			unknown = append(unknown, f.Path)
		}
	}
	for _, m := range manifests {
		for _, f := range ch.Files {
			if f.Status == "D" || !m.match(f.Path) {
				continue
			}
			after, ok, err := ch.After(f.Path)
			if err != nil || !ok {
				a.Unreadable("couldn't read a dependency manifest", f.Path)
				continue
			}
			now, err := m.deps(after)
			if err != nil {
				a.Unreadable("couldn't parse a dependency manifest", f.Path)
				continue
			}
			had := map[dep]bool{}
			before, ok, err := ch.Before(f.Path)
			if err != nil {
				a.Unreadable("couldn't read a dependency manifest on the base branch", f.Path)
				continue
			}
			if ok {
				old, _ := m.deps(before) // what it can read; one it can't counts as new
				for _, d := range old {
					had[d] = true
				}
			}
			for _, d := range now {
				if d.name != "" && !had[d] && !seen[d.String()] {
					seen[d.String()] = true
					out = append(out, Item{Name: d.String(), File: f.Path})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, unknown
}

// goMod reads go.mod's requirements, direct and indirect, and its
// replace directives (a replace is where the module comes from).
func goMod(body string) ([]dep, error) {
	var out []dep
	block := ""
	add := func(verb string, f []string) {
		switch {
		case verb == "require" && len(f) >= 1:
			out = append(out, dep{name: f[0]})
		case verb == "replace":
			// old [v] => new [v]: the target, version and all, is the source.
			if i := slices.Index(f, "=>"); i > 0 && i+1 < len(f) {
				out = append(out, dep{name: f[0], from: "replace " + strings.Join(f[i+1:], " ")})
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line, _, _ = strings.Cut(line, "//")
		line = strings.Replace(strings.Replace(line, "(", " ( ", 1), ")", " ) ", 1)
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case block != "" && f[0] == ")":
			block = ""
		case block != "":
			add(block, f)
		case (f[0] == "require" || f[0] == "replace") && len(f) >= 2 && f[1] == "(":
			block = f[0]
		case f[0] == "require" || f[0] == "replace":
			add(f[0], f[1:])
		}
	}
	return out, nil
}

// aliasRE is an npm alias without a version: npm:other.
var aliasRE = regexp.MustCompile(`^(@[\w.-]+/)?[a-z][\w.-]*$`)

// npmFrom is where an npm spec takes the package from: "" for a version
// range on the registry; the package for an alias (npm:other@1); the
// spec itself for a git, file, link, URL or GitHub one.
func npmFrom(spec string) string {
	spec = strings.TrimSpace(spec)
	switch {
	case strings.HasPrefix(spec, "npm:"):
		// yarn berry writes every registry range as npm:^1.2; an alias
		// names another package: npm:other@1.
		rest := strings.TrimPrefix(spec, "npm:")
		if rest == "" || rest[0] != '@' && !unicode.IsLetter(rune(rest[0])) || !strings.Contains(rest[1:], "@") && !aliasRE.MatchString(rest) {
			return ""
		}
		return "npm:" + beforeVersion(rest)
	case strings.HasPrefix(spec, "workspace:"):
		return ""
	case strings.Contains(spec, ":") || strings.Contains(spec, "/"):
		// git+, github:, file:, link:, http(s)://, or user/repo.
		return spec
	}
	return ""
}

func packageJSON(body string) ([]dep, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, err
	}
	var out []dep
	for _, k := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]any
		if json.Unmarshal(m[k], &deps) == nil {
			for n, v := range deps {
				s, _ := v.(string)
				out = append(out, dep{n, npmFrom(s)})
			}
		}
	}
	// Overrides (npm), resolutions (yarn) and pnpm.overrides force a
	// package's version or source wherever it's used.
	overrides := func(raw json.RawMessage, kind string) {
		var walk func(prefix string, v any)
		walk = func(prefix string, v any) {
			switch v := v.(type) {
			case string:
				out = append(out, dep{kind + " " + prefix, "spec " + v})
			case map[string]any:
				for k, x := range v {
					p := k
					if prefix != "" && k != "." {
						p = prefix + " > " + k
					} else if k == "." {
						p = prefix
					}
					walk(p, x)
				}
			}
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			walk("", v)
		}
	}
	overrides(m["overrides"], "override")
	overrides(m["resolutions"], "resolution")
	var pnpm struct {
		Overrides json.RawMessage `json:"overrides"`
	}
	if json.Unmarshal(m["pnpm"], &pnpm) == nil && pnpm.Overrides != nil {
		overrides(pnpm.Overrides, "override")
	}
	for _, k := range []string{"bundleDependencies", "bundledDependencies"} {
		var names []string
		if json.Unmarshal(m[k], &names) == nil {
			for _, n := range names {
				out = append(out, dep{n, "bundled"})
			}
		}
		var all bool
		if json.Unmarshal(m[k], &all) == nil && all {
			out = append(out, dep{"every dependency", "bundled"})
		}
	}
	return out, nil
}

// npmLock reads package-lock.json: v2 and v3's packages, with where each
// was resolved from when that isn't the registry, and v1's dependencies,
// nested ones included.
func npmLock(body string) ([]dep, error) {
	type v1 struct {
		Resolved     string        `json:"resolved"`
		Dependencies map[string]v1 `json:"dependencies"`
	}
	var lock struct {
		Packages map[string]struct {
			Resolved string `json:"resolved"`
			Link     bool   `json:"link"`
		} `json:"packages"`
		Dependencies map[string]v1 `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(body), &lock); err != nil {
		return nil, err
	}
	from := func(resolved string) string {
		if resolved == "" || strings.HasPrefix(resolved, "https://registry.npmjs.org/") {
			return ""
		}
		u, _, _ := strings.Cut(resolved, "#")
		if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			// Another registry: its host, not each tarball's URL.
			host, _, _ := strings.Cut(strings.SplitN(u, "://", 2)[1], "/")
			return "registry " + host
		}
		return resolved
	}
	var out []dep
	for k, p := range lock.Packages {
		if i := strings.LastIndex(k, "node_modules/"); i >= 0 {
			out = append(out, dep{k[i+len("node_modules/"):], from(p.Resolved)})
		}
	}
	var walk func(map[string]v1)
	walk = func(ds map[string]v1) {
		for n, d := range ds {
			out = append(out, dep{n, from(d.Resolved)})
			walk(d.Dependencies)
		}
	}
	walk(lock.Dependencies)
	return out, nil
}

// beforeVersion is a package spec's name: "@scope/x@^1" is @scope/x.
func beforeVersion(spec string) string {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i]
	}
	return spec
}

func yarnLock(body string) ([]dep, error) {
	var out []dep
	for _, line := range strings.Split(body, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '#' || !strings.HasSuffix(line, ":") {
			continue
		}
		for _, spec := range strings.Split(strings.TrimSuffix(line, ":"), ",") {
			spec = strings.Trim(strings.TrimSpace(spec), `"`)
			if spec == "__metadata" || strings.Contains(spec, "@workspace:") {
				continue
			}
			name := beforeVersion(spec)
			out = append(out, dep{name, npmFrom(strings.TrimPrefix(spec, name+"@"))})
		}
	}
	return out, nil
}

func pnpmLock(body string) ([]dep, error) {
	var lock struct {
		Packages map[string]any `yaml:"packages"`
	}
	if err := yaml.Unmarshal([]byte(body), &lock); err != nil {
		return nil, err
	}
	var out []dep
	for k := range lock.Packages {
		k = strings.TrimPrefix(k, "/")
		k, _, _ = strings.Cut(k, "(") // peer dependencies
		if i := strings.LastIndex(k, "@"); i > 0 {
			out = append(out, dep{k[:i], npmFrom(k[i+1:])})
		} else if i := strings.LastIndex(k, "/"); i > 0 { // v5: /name/1.0.0
			out = append(out, dep{name: k[:i]})
		}
	}
	return out, nil
}

func pipfileLock(body string) ([]dep, error) {
	var lock map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &lock); err != nil {
		return nil, err
	}
	var out []dep
	for _, k := range []string{"default", "develop"} {
		var deps map[string]map[string]any
		if json.Unmarshal(lock[k], &deps) == nil {
			for n, d := range deps {
				out = append(out, dep{pyName(n), pySource(d)})
			}
		}
	}
	var meta struct {
		Meta struct {
			Sources []struct {
				URL string `json:"url"`
			} `json:"sources"`
		} `json:"_meta"`
	}
	if json.Unmarshal([]byte(body), &meta) == nil {
		for _, s := range meta.Meta.Sources {
			out = append(out, dep{"package index", s.URL})
		}
	}
	return out, nil
}

// pySource is where a Pipfile or Poetry table takes a package from: its
// git, path, url, file or index; "" for the index.
func pySource(d map[string]any) string {
	for _, k := range []string{"git", "path", "url", "file", "index", "source"} {
		if v, ok := d[k].(string); ok {
			return k + " " + v
		}
	}
	return ""
}

var (
	pyNameRE = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)
	pySepRE  = regexp.MustCompile(`[-_.]+`)
	inlineRE = regexp.MustCompile(`\{[^}]*\}`)
)

// pyName is a Python requirement's normalized name (PEP 503).
func pyName(req string) string {
	m := pyNameRE.FindStringSubmatch(req)
	if m == nil {
		return ""
	}
	return strings.ToLower(pySepRE.ReplaceAllString(m[1], "-"))
}

// pyReq is a PEP 508 requirement: its name, and a direct reference
// ("name @ https://...") as its source.
func pyReq(req string) dep {
	req = strings.TrimSpace(req)
	if i := strings.Index(req, "@"); i > 0 {
		if _, ref, ok := strings.Cut(req, "@"); ok && !strings.ContainsAny(req[:i], "<>=!~") {
			ref, _, _ = strings.Cut(strings.TrimSpace(ref), ";")
			return dep{pyName(req), strings.TrimSpace(ref)}
		}
	}
	return dep{name: pyName(req)}
}

// requirements reads a pip requirements file. A line that isn't a plain
// requirement (an index, a find-links, an included file, an editable or
// a direct URL) is its own entry, so adding or changing one counts.
func requirements(body string) ([]dep, error) {
	var out []dep
	for _, line := range strings.Split(body, "\n") {
		line, _, _ = strings.Cut(line, " #")
		line = strings.Join(strings.Fields(line), " ")
		switch {
		case line == "" || line[0] == '#':
		case line[0] != '-' && strings.Contains(line, " @ "):
			out = append(out, pyReq(line)) // name @ https://...
		case line[0] == '-' || strings.Contains(line, "://"):
			// --index-url, -i, --extra-index-url, --find-links, -f, -r, -c,
			// -e, git+https://...: where packages come from.
			out = append(out, dep{"pip option", line})
		default:
			out = append(out, pyReq(line))
		}
	}
	return out, nil
}

var (
	gemRE       = regexp.MustCompile(`^\s*gem\s*\(?\s*["']([^"']+)["'](.*)$`)
	gemSourceRE = regexp.MustCompile(`\b(git|github|path|source)\s*(?::|=>)\s*["']([^"']+)["']`)
	sourceRE    = regexp.MustCompile(`^\s*source\s*\(?\s*["']([^"']+)["']`)
)

func gemfile(body string) ([]dep, error) {
	var out []dep
	for _, line := range strings.Split(body, "\n") {
		if m := gemRE.FindStringSubmatch(line); m != nil {
			d := dep{name: m[1]}
			if s := gemSourceRE.FindStringSubmatch(m[2]); s != nil {
				d.from = s[1] + " " + s[2]
			}
			out = append(out, d)
		} else if m := sourceRE.FindStringSubmatch(line); m != nil {
			out = append(out, dep{"gem source", m[1]})
		}
	}
	return out, nil
}

// gemLock reads Gemfile.lock's specs, under the remote each comes from.
func gemLock(body string) ([]dep, error) {
	var out []dep
	remote := ""
	spec := regexp.MustCompile(`^    ([A-Za-z0-9._-]+) \(`)
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line != "" && line[0] != ' ':
			remote = "" // GEM, GIT, PATH ...
		case strings.HasPrefix(line, "  remote: "):
			if r := strings.TrimSpace(strings.TrimPrefix(line, "  remote: ")); r != "https://rubygems.org/" {
				remote = r
			}
		default:
			if m := spec.FindStringSubmatch(line); m != nil {
				out = append(out, dep{m[1], remote})
			}
		}
	}
	return out, nil
}

func names(re *regexp.Regexp) func(string) ([]dep, error) {
	return func(body string) ([]dep, error) {
		var out []dep
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			out = append(out, dep{name: m[1]})
		}
		return out, nil
	}
}

// tomlEntry is one key of a TOML file, as tomlScan reads it: the table
// it's in, the key, and its value's text (an array's whole text).
type tomlEntry struct{ table, key, value string }

// tomlScan reads a TOML file's tables and keys well enough to find
// dependencies: a key's value runs to the end of its line, or to the
// bracket that closes its array or inline table.
func tomlScan(body string) []tomlEntry {
	var out []tomlEntry
	table := ""
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "" || line[0] == '#':
			continue
		case strings.HasPrefix(line, "["):
			table = strings.Trim(strings.TrimSpace(strings.SplitN(line, "#", 2)[0]), "[] ")
			out = append(out, tomlEntry{table: table})
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		for depth := arrayDepth(value); depth > 0 && i+1 < len(lines); depth = arrayDepth(value) {
			i++
			value += "\n" + lines[i]
		}
		out = append(out, tomlEntry{table, strings.TrimSpace(key), value})
	}
	return out
}

// arrayDepth is how many brackets s leaves open, outside quotes and
// comments.
func arrayDepth(s string) int {
	depth := 0
	var quote rune
	comment := false
	for _, c := range s {
		switch {
		case c == '\n':
			comment = false
		case comment:
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			comment = true
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
	}
	return depth
}

// tomlKey is a key's name: `"a.b"` is a.b, and dotted a.workspace is a.
func tomlKey(k string) string {
	if strings.HasPrefix(k, `"`) || strings.HasPrefix(k, "'") {
		return strings.Trim(k, `"'`)
	}
	k, _, _ = strings.Cut(k, ".")
	return k
}

var tomlStringRE = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

// tomlStrings are the strings an array's text holds, inline tables left
// out ({include-group = "x"} isn't a requirement).
func tomlStrings(v string) []string {
	var out []string
	for _, m := range tomlStringRE.FindAllStringSubmatch(inlineRE.ReplaceAllString(v, ""), -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

// tomlSource is where an inline table ({ git = "...", rev = "..." })
// takes a dependency from: its git, path, url, registry, index or the
// package it renames; "" for none.
func tomlSource(v string) string {
	if !strings.HasPrefix(strings.TrimSpace(v), "{") {
		return ""
	}
	var parts []string
	for _, k := range sourceKeys {
		if m := sourceKeyRE[k].FindStringSubmatch(v); m != nil {
			parts = append(parts, k+" "+strings.Trim(m[1], `"'`))
		}
	}
	return strings.Join(parts, ", ")
}

// sourceKeys are the keys of a TOML dependency that say where it comes
// from, rather than which version.
var sourceKeys = []string{"git", "path", "url", "registry", "index", "package", "branch", "rev", "tag", "source", "workspace"}

var sourceKeyRE = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, k := range sourceKeys {
		m[k] = regexp.MustCompile(`\b` + k + `\s*=\s*("[^"]*"|'[^']*'|true)`)
	}
	return m
}()

// sourced adds a dependency's source keys, from its own table, to the
// dependency at out[i].
func sourced(out []dep, i int, e tomlEntry) {
	if i >= 0 && slices.Contains(sourceKeys, e.key) {
		if out[i].from != "" {
			out[i].from += ", "
		}
		out[i].from += e.key + " " + strings.Trim(e.value, `"'`)
	}
}

// subtable is the last part of a table name after prefix: "serde" in
// dependencies.serde.
func subtable(table, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(table, prefix+".")
	if !ok || rest == "" || strings.Contains(strings.Trim(rest, `"'`), ".") && !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	return strings.Trim(rest, `"'`), true
}

func pipfile(body string) ([]dep, error) {
	var out []dep
	for _, e := range tomlScan(body) {
		switch {
		case e.key == "" && e.table == "source":
		case e.table == "source" && e.key == "url":
			out = append(out, dep{"package index", strings.Trim(e.value, `"'`)})
		case e.key != "" && (e.table == "packages" || e.table == "dev-packages"):
			out = append(out, dep{pyName(tomlKey(e.key)), tomlSource(e.value)})
		}
	}
	return out, nil
}

func pyproject(body string) ([]dep, error) {
	var out []dep
	reqs := func(v string) {
		for _, r := range tomlStrings(v) {
			if d := pyReq(r); d.name != "" {
				out = append(out, d)
			}
		}
	}
	poetry := func(t string) bool {
		return t == "tool.poetry.dependencies" || t == "tool.poetry.dev-dependencies" ||
			strings.HasPrefix(t, "tool.poetry.group.") && strings.HasSuffix(t, ".dependencies")
	}
	cur := -1 // the dependency whose own table this is
	for _, e := range tomlScan(body) {
		switch {
		case e.key == "":
			// [tool.poetry.dependencies.requests]
			cur = -1
			for _, t := range []string{"tool.poetry.dependencies", "tool.poetry.dev-dependencies"} {
				if n, ok := subtable(e.table, t); ok {
					out, cur = append(out, dep{name: pyName(n)}), len(out)
				}
			}
			if strings.HasPrefix(e.table, "tool.poetry.group.") {
				if _, n, ok := strings.Cut(e.table, ".dependencies."); ok {
					out, cur = append(out, dep{name: pyName(strings.Trim(n, `"'`))}), len(out)
				}
			}
		case cur >= 0:
			sourced(out, cur, e)
		case e.table == "project" && e.key == "dependencies",
			e.table == "project.optional-dependencies", e.table == "dependency-groups",
			e.table == "build-system" && e.key == "requires",
			e.table == "tool.uv" && (e.key == "dev-dependencies" || e.key == "constraint-dependencies" || e.key == "override-dependencies"):
			reqs(e.value)
		case e.table == "project" && e.key == "optional-dependencies":
			reqs(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(e.value), "{"), "}")) // the inline table's arrays
		case e.table == "tool.uv.sources":
			out = append(out, dep{pyName(tomlKey(e.key)), "uv source " + strings.Join(strings.Fields(e.value), " ")})
		case (e.table == "tool.uv" || e.table == "tool.uv.pip") && (e.key == "index-url" || e.key == "extra-index-url" || e.key == "find-links"):
			out = append(out, dep{"package index", e.key + " " + strings.Join(strings.Fields(e.value), " ")})
		case strings.HasPrefix(e.table, "tool.uv.index") || e.table == "tool.poetry.source" || strings.HasPrefix(e.table, "tool.poetry.source"):
			if e.key == "url" {
				out = append(out, dep{"package index", strings.Trim(e.value, `"'`)})
			}
		case poetry(e.table):
			if n := pyName(tomlKey(e.key)); n != "python" {
				out = append(out, dep{n, tomlSource(e.value)})
			}
		}
	}
	return out, nil
}

func cargoToml(body string) ([]dep, error) {
	var out []dep
	deps := func(t string) bool {
		for _, s := range []string{"dependencies", "dev-dependencies", "build-dependencies"} {
			if t == s || strings.HasSuffix(t, "."+s) {
				return true
			}
		}
		return false
	}
	cur := -1 // the dependency whose own table this is
	for _, e := range tomlScan(body) {
		switch {
		case e.key == "":
			cur = -1
			if _, name, ok := strings.Cut(e.table, "dependencies."); ok {
				// [dependencies.serde]: its keys follow.
				out, cur = append(out, dep{name: strings.Trim(name, `"'`)}), len(out)
			}
		case cur >= 0:
			sourced(out, cur, e)
		case e.key != "" && strings.HasPrefix(e.table, "patch."):
			out = append(out, dep{tomlKey(e.key), "patch " + strings.TrimPrefix(e.table, "patch.") + " " + tomlSource(e.value)})
		case e.key != "" && e.table == "replace":
			out = append(out, dep{strings.Trim(e.key, `"'`), "replace " + tomlSource(e.value)})
		case e.key != "" && deps(e.table):
			out = append(out, dep{tomlKey(e.key), tomlSource(e.value)})
		}
	}
	return out, nil
}

// lockPackages reads the names of a [[package]] lockfile's packages
// (poetry.lock, uv.lock, Cargo.lock) whose source's kind keep says count,
// with the source when it isn't registry.
func lockPackages(keep func(key string) bool, registry string) func(string) ([]dep, error) {
	keyRE := regexp.MustCompile(`^\{\s*([\w-]+)\s*=\s*"([^"]*)"`)
	return func(body string) ([]dep, error) {
		var out []dep
		name, source, in := "", "", false
		flush := func() {
			if !in || name == "" {
				name, source = "", ""
				return
			}
			key, val := "", ""
			if m := keyRE.FindStringSubmatch(source); m != nil {
				key, val = m[1], m[2] // uv: { registry = "..." }
			} else if source != "" {
				key, val = "source", strings.Trim(source, `"'`) // Cargo: "registry+https://..."
			}
			if keep(key) {
				d := dep{name: name}
				if val != registry && val != "" {
					d.from = key + " " + val
				}
				out = append(out, d)
			}
			name, source = "", ""
		}
		for _, e := range tomlScan(body) {
			switch {
			case e.key == "":
				if e.table == "package" {
					flush()
					in = true
				} else if !strings.HasPrefix(e.table, "package.") {
					flush()
					in = false
				}
			case !in:
			case e.table == "package" && e.key == "name":
				name = strings.Trim(e.value, `"'`)
			case e.table == "package" && e.key == "source":
				source = e.value
			case e.table == "package.source" && e.key == "url":
				source = fmt.Sprintf("{ url = %s }", e.value) // poetry's [package.source]
			}
		}
		flush()
		return out, nil
	}
}
