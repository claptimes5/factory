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
	if api.Stack != "Ruby on Rails" || api.ImageFrom != "ruby:3.3-bookworm" || api.Install != "bundle install" {
		t.Fatalf("rails: %+v", api)
	}
	if strings.Join(api.Required, ",") != "lint,test" || api.Commands["test"] != "bin/rails db:prepare test" {
		t.Fatalf("rails gates: %v %v", api.Required, api.Commands)
	}
	if len(api.Notes) != 1 || !strings.Contains(api.Notes[0], "PostgreSQL") {
		t.Fatalf("rails notes: %v", api.Notes)
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
			"Python | python:3.12-bookworm | python -m venv .venv && .venv/bin/pip install -e '.[dev,test]' | lint test"},
		"rust": {fakeFiles{"Cargo.toml": "[package]"}, "Rust | rust:1-bookworm | cargo fetch | lint test build"},
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
	for in, want := range map[string]string{"ruby-3.3.6\n": "3.3", "3.12": "3.12", "": "9.9", "system": "9.9"} {
		if got := version(in, "9.9"); got != want {
			t.Errorf("version(%q) = %s", in, got)
		}
	}
}
