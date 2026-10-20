// Package stack looks at a repository's files and suggests how the factory
// should check it: the job image's base, the install step and the gate
// commands.
package stack

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// Files reads a repository: List names a directory's entries ("" is the
// root; directories end in "/"), Read returns a file.
type Files interface {
	List(ctx context.Context, dir string) ([]string, error)
	Read(ctx context.Context, path string) (string, error)
}

// Suggestion is what Detect proposes for one repository.
type Suggestion struct {
	Stack     string            // for people, e.g. "Ruby on Rails"
	ImageFrom string            // base for the job image; "" is the default image
	Install   string            // dependency install command
	Required  []string          // gate names, in order
	Commands  map[string]string // gate name -> command
	Go        bool              // a Go module: the top-level gates already fit
	Notes     []string          // what it saw but can't set up yet
}

// Detect inspects the repository root. It returns nil when it recognizes
// nothing.
func Detect(ctx context.Context, f Files) (*Suggestion, error) {
	names, err := f.List(ctx, "")
	if err != nil {
		return nil, err
	}
	has := func(n string) bool { return slices.Contains(names, n) }
	read := func(n string) string {
		if !has(n) {
			return ""
		}
		s, _ := f.Read(ctx, n)
		return s
	}
	s := &Suggestion{Commands: map[string]string{}}
	add := func(gate, cmd string) {
		if cmd == "" || s.Commands[gate] != "" {
			return
		}
		s.Required = append(s.Required, gate)
		s.Commands[gate] = cmd
	}
	var stacks []string
	switch {
	case has("go.mod"):
		s.Go = true
		stacks = append(stacks, "Go")
	case has("Gemfile"):
		// test/test_helper.rb is Minitest's, whatever else the repo has.
		minitest := false
		if has("test/") {
			names, _ := f.List(ctx, "test/")
			minitest = slices.Contains(names, "test_helper.rb")
		}
		stacks = append(stacks, ruby(s, read, has, minitest, add))
	case has("pyproject.toml") || has("requirements.txt"):
		stacks = append(stacks, python(s, read, has, add))
	case has("Cargo.toml"):
		stacks = append(stacks, "Rust")
		s.ImageFrom = "rust:1-bookworm"
		s.Install = "cargo fetch"
		add("lint", "cargo clippy --all-targets -- -D warnings")
		add("test", "cargo test")
		add("build", "cargo build")
	}
	if has("package.json") {
		if s.Go {
			// The Go gates come from the top level; this doesn't add the
			// frontend's to them.
			s.Notes = append(s.Notes, "It also has a package.json. Only the Go code is checked; add the frontend's install and commands to its gates to check it too.")
		} else if st := node(s, read, has, add); st != "" {
			stacks = append(stacks, st)
		}
	}
	devcontainer(ctx, s, f, names, read)
	if len(stacks) == 0 {
		return nil, nil
	}
	s.Stack = strings.Join(stacks, " + ")
	return s, nil
}

func ruby(s *Suggestion, read func(string) string, has func(string) bool, minitest bool, add func(string, string)) string {
	gemfile := read("Gemfile")
	gem := func(name string) bool {
		return regexp.MustCompile(`(?m)^\s*gem\s+["']` + regexp.QuoteMeta(name) + `["']`).MatchString(gemfile)
	}
	s.ImageFrom = "ruby:" + version(read(".ruby-version"), "3.4") + "-bookworm"
	s.Install = "bundle install"
	stack := "Ruby"
	if gem("rails") {
		stack = "Ruby on Rails"
	}
	if gem("rubocop") || strings.Contains(gemfile, "rubocop") {
		add("lint", "bundle exec rubocop")
	}
	// A repo may have rspec-rails for its matchers, or spec/ for
	// factories, and run Minitest: .rspec is what RSpec suites have.
	switch {
	case has(".rspec"):
		add("test", "bundle exec rspec")
	case minitest && stack == "Ruby on Rails":
		add("test", "bin/rails db:prepare test")
	case minitest:
		add("test", "bundle exec rake test")
	case gem("rspec-rails") || gem("rspec") || has("spec/"):
		add("test", "bundle exec rspec")
	case stack == "Ruby on Rails":
		add("test", "bin/rails db:prepare test")
	default:
		add("test", "bundle exec rake test")
	}
	switch {
	case gem("pg"):
		s.Notes = append(s.Notes, "It uses PostgreSQL. Declare the database under the repo's services in factory.yaml, or tests that need it will fail.")
	case gem("mysql2") || gem("trilogy"):
		s.Notes = append(s.Notes, "It uses MySQL. Declare the database under the repo's services in factory.yaml, or tests that need it will fail.")
	}
	return stack
}

func python(s *Suggestion, read func(string) string, has func(string) bool, add func(string, string)) string {
	s.ImageFrom = "python:" + version(read(".python-version"), "3.13") + "-bookworm"
	pyproject := read("pyproject.toml")
	deps := pyproject + read("requirements.txt") + read("requirements-dev.txt")
	// Dependencies go in a virtualenv inside the checkout, so the gates
	// after install see them.
	install := "python -m venv .venv && .venv/bin/pip install"
	pkg := strings.Contains(pyproject, "[build-system]")
	switch {
	case has("uv.lock"):
		install = "python -m venv .venv && .venv/bin/pip install uv && .venv/bin/uv sync --frozen --all-groups"
	case strings.Contains(pyproject, "[dependency-groups]"):
		// Dev tools in dependency groups need pip 25.1 or later.
		install += " --upgrade pip && .venv/bin/pip install --group dev"
		if pkg {
			install += " -e ."
		}
	case pkg:
		install += " -e '.[dev,test]'"
	case has("requirements.txt"):
		install += " -r requirements.txt"
	default:
		s.Notes = append(s.Notes, "Its pyproject.toml has no [build-system], so it can't be installed as a package; set the install step yourself.")
		install = "python -m venv .venv"
	}
	if has("requirements-dev.txt") && !has("uv.lock") {
		install += " && .venv/bin/pip install -r requirements-dev.txt"
	}
	s.Install = install
	if strings.Contains(deps, "ruff") {
		add("lint", ".venv/bin/ruff check .")
	}
	if strings.Contains(deps, "mypy") {
		add("types", ".venv/bin/mypy .")
	}
	if strings.Contains(deps, "pytest") || has("tests/") {
		add("test", ".venv/bin/python -m pytest")
	}
	return "Python"
}

func node(s *Suggestion, read func(string) string, has func(string) bool, add func(string, string)) string {
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal([]byte(read("package.json")), &pkg) != nil {
		return ""
	}
	dep := func(n string) bool { return pkg.Dependencies[n] != "" || pkg.DevDependencies[n] != "" }
	run, install := "npm run", "npm ci"
	switch {
	case has("pnpm-lock.yaml"):
		run, install = "corepack pnpm run", "corepack pnpm install --frozen-lockfile"
	case has("yarn.lock"):
		// Yarn 2+ lockfiles start with __metadata; yarn 1 has its own flag.
		run, install = "corepack yarn run", "corepack yarn install --frozen-lockfile"
		if strings.Contains(read("yarn.lock"), "__metadata:") {
			install = "corepack yarn install --immutable"
		}
	case !has("package-lock.json"):
		install = "npm install"
	}
	if s.Install != "" {
		// Another stack's install runs first; node's follows it.
		s.Install += " && " + install
	} else {
		s.Install = install
	}
	script := func(n string) string {
		if c := pkg.Scripts[n]; c != "" && !strings.Contains(c, "no test specified") {
			return run + " " + n
		}
		return ""
	}
	build := pkg.Scripts["build"]
	switch {
	case script("typecheck") != "":
		add("types", script("typecheck"))
	case script("type-check") != "":
		add("types", script("type-check"))
	case dep("typescript") && has("tsconfig.json") && !strings.Contains(build, "tsc"):
		add("types", "npx tsc --noEmit")
	}
	add("lint", script("lint"))
	// CI=true keeps watch-mode test runners (vitest, jest) from waiting.
	if t := script("test"); t != "" {
		add("test", "CI=true "+t)
	}
	add("build", script("build"))
	stack := "Node"
	switch {
	case dep("vite"):
		stack = "Vite"
	case dep("next"):
		stack = "Next.js"
	}
	if dep("react") {
		stack = "React (" + stack + ")"
	}
	return stack
}

// devcontainer uses the repo's dev container image as the job image's
// base when it names one.
func devcontainer(ctx context.Context, s *Suggestion, f Files, names []string, read func(string) string) {
	raw := read(".devcontainer.json")
	if raw == "" && slices.Contains(names, ".devcontainer/") {
		raw, _ = f.Read(ctx, ".devcontainer/devcontainer.json")
	}
	if raw == "" {
		return
	}
	var dc struct {
		Image      string `json:"image"`
		Build      any    `json:"build"`
		DockerFile string `json:"dockerFile"`
	}
	if json.Unmarshal([]byte(stripComments(raw)), &dc) != nil {
		s.Notes = append(s.Notes, "Its devcontainer.json couldn't be read, so the image is picked from the stack instead.")
		return
	}
	switch {
	case dc.Image != "":
		s.ImageFrom = dc.Image
		s.Notes = append(s.Notes, "The job image is built on its dev container's image, "+dc.Image+".")
	case dc.Build != nil || dc.DockerFile != "":
		s.Notes = append(s.Notes, "Its dev container is built from a Dockerfile, which setup can't use yet, so the image is picked from the stack instead.")
	}
}

// stripComments drops // and /* */ comments and trailing commas outside
// strings, which devcontainer.json allows.
func stripComments(s string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(c)
		}
	}
	return regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(b.String(), "$1")
}

// version turns a .ruby-version or .python-version ("ruby-3.3.6",
// "3.12") into an image tag, or def. A patch version is kept: a Gemfile
// can pin it, and the official images tag every release.
func version(file, def string) string {
	v := strings.TrimSpace(file)
	if i := strings.LastIndex(v, "-"); i >= 0 {
		v = v[i+1:]
	}
	m := regexp.MustCompile(`^\d+\.\d+(\.\d+)?`).FindString(v)
	if m == "" {
		return def
	}
	return m
}
