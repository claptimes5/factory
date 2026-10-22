package stack

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

const railsGemfile = "source 'https://rubygems.org'\ngem \"rails\"\ngem 'pg'\n"

// A Rails app with a test stage and a compose file: the job image is built
// from the stage, and the database and its settings come from compose, not
// the stack's guess.
func TestDockerfileAndCompose(t *testing.T) {
	s, err := Detect(context.Background(), fakeFiles{
		"Gemfile": railsGemfile,
		"Dockerfile": "# syntax=docker/dockerfile:1.7\nARG RUBY_VERSION=4.0.3\nFROM ruby:$RUBY_VERSION-slim AS base\nFROM base AS node\nFROM node AS build\n" +
			"RUN --mount=type=secret,id=railsmasterkey,dst=/app/config/credentials/master.key bin/assets\n" +
			"FROM base AS runtime\nENV RAILS_ENV=production\nFROM node AS test\nENTRYPOINT [\"sh\", \"/usr/bin/entrypoint.sh\"]\n",
		"docker-compose.yml": `services:
  db:
    image: postgres
    environment:
      POSTGRES_USER: 'shop'
      POSTGRES_PASSWORD: 'devpass'
    ports: ["6543:5432"]
  web:
    build: .
    environment:
      PGHOST: "db"
      PGUSER: "shop"
      PGPASSWORD: "devpass"
      RAILS_LOG_TO_STDOUT: 'true'
      SECRET: "${SECRET}"
  mailcatcher:
    image: schickling/mailcatcher
`,
		".gitlab-ci.yml": "test:\n  script:\n    - docker buildx build --push --target test --secret id=railsmasterkey,env=TEST_MASTER_KEY --build-arg RAILS_ENV=test --build-arg V=$CI_VERSION .\n" +
			"deploy:\n  script:\n    - docker buildx build --target runtime --build-arg RAILS_ENV=production .\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ImageBuild == nil || s.ImageBuild.Target != "test" || s.ImageBuild.Dockerfile != "" || s.ImageFrom != "" {
		t.Fatalf("image: %+v from %q", s.ImageBuild, s.ImageFrom)
	}
	// Built as its CI builds that stage: the literal args, not the deploy's.
	if len(s.ImageBuild.Args) != 1 || s.ImageBuild.Args["RAILS_ENV"] != "test" || s.ImageBuild.Secrets != nil {
		t.Fatalf("args %v secrets %v", s.ImageBuild.Args, s.ImageBuild.Secrets)
	}
	db := s.Services["db"]
	if len(s.Services) != 1 || db.Image != "postgres" || db.Env["POSTGRES_USER"] != "shop" || db.Ready != "pg_isready -U shop" {
		t.Fatalf("services: %+v", s.Services)
	}
	if got := strings.Join(slices.Sorted(maps.Keys(s.Env)), ","); got != "PGHOST,PGPASSWORD,PGUSER" || s.Env["PGHOST"] != "db" {
		t.Fatalf("env: %v", s.Env)
	}
	if notes := strings.Join(s.Notes, " "); !strings.Contains(notes, "Dockerfile's test stage") || !strings.Contains(notes, "also runs mailcatcher") ||
		!strings.Contains(notes, "build secret railsmasterkey, which its CI takes from TEST_MASTER_KEY") {
		t.Fatalf("notes: %v", s.Notes)
	}
}

func TestDockerfileStages(t *testing.T) {
	for name, c := range map[string]struct {
		files       fakeFiles
		build, note string // "" for none
	}{
		"no test stage": {fakeFiles{"Gemfile": railsGemfile, "Dockerfile": "FROM ruby:3.4 AS base\nFROM base AS runtime\n"},
			"", "no test, ci or development stage"},
		"alpine": {fakeFiles{"Gemfile": railsGemfile, "Dockerfile": "FROM ruby:3.4-alpine AS base\nFROM base AS test\n"},
			"", "built on ruby:3.4-alpine"},
		"dev container": {fakeFiles{"Gemfile": railsGemfile, ".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile", "context": ".."}}`},
			"Dockerfile=.devcontainer/Dockerfile context= target=", "dev container's Dockerfile"},
		// Made for the tests, it beats the dev container.
		"test stage first": {fakeFiles{"Gemfile": railsGemfile, "Dockerfile": "FROM ruby:3.4 AS ci\n", ".devcontainer.json": `{"dockerFile": "dev.Dockerfile"}`},
			"Dockerfile= context= target=ci", "Dockerfile's ci stage"},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Detect(context.Background(), c.files)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if b := s.ImageBuild; b != nil {
				got = "Dockerfile=" + b.Dockerfile + " context=" + b.Context + " target=" + b.Target
			} else if s.ImageFrom == "" {
				t.Fatal("no image at all")
			}
			if got != c.build || !strings.Contains(strings.Join(s.Notes, " "), c.note) {
				t.Fatalf("build %q notes %v", got, s.Notes)
			}
		})
	}
}
