package stack

import (
	"context"
	"errors"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"
)

type fakeFiles map[string]string

func (f fakeFiles) List(_ context.Context, dir string) ([]string, error) {
	seen := map[string]bool{}
	for p := range f {
		if rest, ok := strings.CutPrefix(p, dir); ok {
			name, sub, nested := strings.Cut(rest, "/")
			if nested && sub != "" {
				name += "/"
			}
			seen[name] = true
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

func (f fakeFiles) Read(_ context.Context, p string) (string, error) {
	if s, ok := f[path.Clean(p)]; ok {
		return s, nil
	}
	return "", errors.New("not found")
}

func TestRailsAndVite(t *testing.T) {
	ctx := context.Background()
	api, err := Detect(ctx, fakeFiles{
		"Gemfile":         "source 'https://rubygems.org'\ngem \"rails\", \"~> 8.0\"\ngem 'pg'\ngroup :development do\n  gem \"rubocop-rails-omakase\", require: false\nend\n",
		".ruby-version":   "ruby-3.3.6\n",
		"app/models/x.rb": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.Stack != "Ruby on Rails" || api.ImageFrom != "ruby:3.3.6-bookworm" || api.Install != "bundle install" {
		t.Fatalf("rails: %+v", api)
	}
	if strings.Join(api.Required, ",") != "lint,test" || api.Commands["test"] != "bin/rails db:prepare test" {
		t.Fatalf("rails gates: %v %v", api.Required, api.Commands)
	}
	// Its tests need the database: a service, and the URL Rails reads.
	if pg := api.Services["postgres"]; pg.Image != "postgres:16" || pg.Ready == "" || api.Env["DATABASE_URL"] != "postgres://postgres:factory@postgres:5432/test" || len(api.Notes) != 0 {
		t.Fatalf("rails services: %+v env %v notes %v", api.Services, api.Env, api.Notes)
	}

	web, err := Detect(ctx, fakeFiles{
		"package.json":      `{"scripts":{"dev":"vite","build":"tsc -b && vite build","lint":"eslint .","test":"vitest"},"dependencies":{"react":"^19"},"devDependencies":{"vite":"^8","typescript":"~5.9"}}`,
		"package-lock.json": "{}",
		"tsconfig.json":     "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if web.Stack != "React (Vite)" || web.ImageFrom != "" || web.Install != "npm ci" {
		t.Fatalf("vite: %+v", web)
	}
	// The build script already type-checks, so there's no separate types gate.
	if strings.Join(web.Required, ",") != "lint,test,build" || web.Commands["test"] != "CI=true npm run test" {
		t.Fatalf("vite gates: %v %v", web.Required, web.Commands)
	}
}

func TestOtherStacks(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		files fakeFiles
		want  string // Stack | ImageFrom | Install | gates
	}{
		"go":      {fakeFiles{"go.mod": "module x"}, "Go |  |  | "},
		"nothing": {fakeFiles{"README.md": "hi"}, ""},
		"python": {fakeFiles{"pyproject.toml": "[project.optional-dependencies]\ndev = [\"pytest\", \"ruff\"]", ".python-version": "3.12.4"},
			"Python | python:3.12.4-bookworm | python -m venv .venv | lint test"},
		"python package": {fakeFiles{"pyproject.toml": "[build-system]\n[project.optional-dependencies]\ndev = [\"pytest\"]"},
			"Python | python:3.13-bookworm | python -m venv .venv && .venv/bin/pip install -e '.[dev,test]' | test"},
		"python groups": {fakeFiles{"pyproject.toml": "[dependency-groups]\ndev = [\"pytest\"]"},
			"Python | python:3.13-bookworm | python -m venv .venv && .venv/bin/pip install --upgrade pip && .venv/bin/pip install --group dev | test"},
		"uv": {fakeFiles{"pyproject.toml": "[dependency-groups]\ndev = [\"ruff\"]", "uv.lock": ""},
			"Python | python:3.13-bookworm | python -m venv .venv && .venv/bin/pip install uv && .venv/bin/uv sync --frozen --all-groups | lint"},
		"yarn1": {fakeFiles{"package.json": `{"scripts":{"lint":"eslint"}}`, "yarn.lock": "# yarn lockfile v1\n"},
			"Node |  | corepack yarn install --frozen-lockfile | lint"},
		"yarn berry": {fakeFiles{"package.json": `{}`, "yarn.lock": "__metadata:\n  version: 8\n"},
			"Node |  | corepack yarn install --immutable | "},
		"go and node": {fakeFiles{"go.mod": "module x", "package.json": `{"scripts":{"build":"vite build"}}`}, "Go |  |  | "},
		"rust":        {fakeFiles{"Cargo.toml": "[package]"}, "Rust | rust:1-bookworm | cargo fetch | lint test build"},
		"pnpm": {fakeFiles{"package.json": `{"scripts":{"typecheck":"tsc","test":"echo \"Error: no test specified\" && exit 1"}}`, "pnpm-lock.yaml": ""},
			"Node |  | corepack pnpm install --frozen-lockfile | types"},
		"devcontainer": {fakeFiles{"package.json": `{}`, ".devcontainer/devcontainer.json": "{\n  // the image\n  \"image\": \"mcr.microsoft.com/devcontainers/typescript-node:22\", /* x */\n}\n"},
			"Node | mcr.microsoft.com/devcontainers/typescript-node:22 | npm install | "},
	} {
		s, err := Detect(ctx, c.files)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if s != nil {
			got = strings.Join([]string{s.Stack, s.ImageFrom, s.Install, strings.Join(s.Required, " ")}, " | ")
		}
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}
}

func TestVersion(t *testing.T) {
	for in, want := range map[string]string{"ruby-3.3.6\n": "3.3.6", "3.12": "3.12", "3.12.4rc1": "3.12.4", "": "9.9", "system": "9.9"} {
		if got := version(in, "9.9"); got != want {
			t.Errorf("version(%q) = %s", in, got)
		}
	}
}

// A Rails app that keeps rspec-rails for its matchers and spec/ for
// factories, but runs Minitest, is tested with Minitest; one with .rspec
// is tested with RSpec.
func TestRubyTestRunner(t *testing.T) {
	ctx := context.Background()
	gemfile := "gem \"rails\"\ngem \"rspec-rails\"\ngem \"minitest-reporters\"\n"
	for name, c := range map[string]struct {
		files fakeFiles
		want  string
	}{
		"minitest":       {fakeFiles{"Gemfile": gemfile, "spec/factories/x.rb": "", "test/test_helper.rb": "", "test/models/x_test.rb": ""}, "bin/rails db:prepare test"},
		"rspec":          {fakeFiles{"Gemfile": gemfile, ".rspec": "--require spec_helper", "spec/models/x_spec.rb": "", "test/test_helper.rb": ""}, "bundle exec rspec"},
		"rspec, no test": {fakeFiles{"Gemfile": gemfile, "spec/models/x_spec.rb": ""}, "bundle exec rspec"},
		"plain minitest": {fakeFiles{"Gemfile": "gem \"minitest\"\n", "test/test_helper.rb": ""}, "bundle exec rake test"},
	} {
		s, err := Detect(ctx, c.files)
		if err != nil || s.Commands["test"] != c.want {
			t.Errorf("%s: test = %q, want %q (%v)", name, s.Commands["test"], c.want, err)
		}
	}
}

// The test command the repo's AGENTS.md gives wins over the guess from its
// files, and says where it came from.
func TestCommandsFromAgentInstructions(t *testing.T) {
	ctx := context.Background()
	agents := "## Conventions\n\n- **Tests** are Minitest under `test/`; run with `bin/rails test <path>`. Never write `RSpec.describe`.\n\n" +
		"```sh\n$ bundle exec rubocop -a app/\n```\n"
	s, err := Detect(ctx, fakeFiles{"Gemfile": "gem \"rails\"\ngem \"rspec-rails\"\n", "spec/factories/x.rb": "", "AGENTS.md": agents, "CLAUDE.md": "Use `bundle exec rspec`."})
	if err != nil {
		t.Fatal(err)
	}
	if s.Commands["test"] != "bin/rails db:prepare test" || s.Sources["test"] != "AGENTS.md" {
		t.Fatalf("test = %q from %q", s.Commands["test"], s.Sources["test"])
	}
	if s.Commands["lint"] != "bundle exec rubocop" || s.Sources["lint"] != "AGENTS.md" {
		t.Fatalf("lint = %q from %q", s.Commands["lint"], s.Sources["lint"])
	}
	// No instructions: the guess, from no file.
	s, _ = Detect(ctx, fakeFiles{"package.json": `{"scripts":{"test":"vitest"}}`, "CLAUDE.md": "Run `pnpm test` before pushing."})
	if s.Commands["test"] != "pnpm test" || s.Sources["test"] != "CLAUDE.md" {
		t.Fatalf("node test = %q from %q", s.Commands["test"], s.Sources["test"])
	}
}
