package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/claptimes5/factory/internal/pathglob"
)

// Sensitive is the one list of sensitive code: code where a mistake is an
// exploit, a leak or lost money. Each area is a list of globs (see
// pathglob.Match). A change to any of them gets the security pass, and
// REVIEW.md's skip never covers one; one to auth, crypto, secrets or
// payments code is the auth kind of change, and to migrations a
// migration, both high risk. Setting an area replaces its defaults, []
// turns it off, and an area of another name is added to them.
type Sensitive map[string][]string

// DefaultSensitive are the areas, in order, and their globs when
// sensitive leaves them out. A glob with no / matches any part of a
// path, so *auth* catches AuthController.php and internal/auth/ alike
// (and authors.go: a false start is cheaper than a miss).
var DefaultSensitive = []pathglob.Area{
	// Authentication, sessions, permissions.
	{Name: "auth", Globs: []string{"*auth*", "*oidc*", "*saml*", "*jwt*", "sso", "sso.*", "sso_*", "*login*", "*logout*", "*signin*",
		"*signup*", "*session*", "*cookie*", "*password*", "passwd*", "*permission*", "*rbac*", "acl", "acl.*", "*accesscontrol*",
		"*access_control*", "*csrf*", "*cors*", "*middleware*", "devise*"}},
	// Cryptography.
	{Name: "crypto", Globs: []string{"*crypt*", "*cipher*", "*hmac*", "tls", "tls.*", "cert", "certs", "cert.*", "cert_*", "cert-*", "*certificate*"}},
	// Keys and secrets.
	{Name: "secrets", Globs: []string{"*.pem", "*.key", "*.crt", "*.p12", "*.pfx", "id_rsa*", "*secret*", "*credential*", "*token*", "*apikey*",
		"*api_key*", ".env", ".env.*", "*.env", ".npmrc", ".pypirc"}},
	// Money.
	{Name: "payments", Globs: []string{"*payment*", "*billing*", "*invoice*", "*checkout*", "*wallet*", "*stripe*"}},
	// Untrusted input, files and outbound requests.
	{Name: "input", Globs: []string{"*sanitiz*", "*upload*", "*download*", "*multipart*", "*serializ*", "*webhook*", "*redirect*", "*proxy*",
		"*unzip*", "*untar*", "*archive*"}},
	// Running commands.
	{Name: "exec", Globs: []string{"exec", "exec.*", "exec_*", "exec-*", "*exec", "*exec.*", "*_exec_*", "*shell*", "*subprocess*"}},
	// SQL.
	{Name: "sql", Globs: []string{"*.sql", "*sql*"}},
	// A database's migrations.
	{Name: "migrations", Globs: []string{"migrations", "migrate", "migrate.*", "migration/", "alembic/", "flyway/", "liquibase/"}},
	// CI, which runs with the repo's secrets.
	{Name: "ci", Globs: []string{".github/workflows/", ".gitlab-ci.yml"}},
	// Dependency manifests and lockfiles.
	{Name: "dependencies", Globs: []string{"go.mod", "go.sum", "go.work", "vendor", "package.json", "package-lock.json", "npm-shrinkwrap.json",
		"yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb", "requirements*.txt", "pipfile", "pipfile.lock", "poetry.lock", "pyproject.toml",
		"uv.lock", "setup.py", "gemfile", "gemfile.lock", "*.gemspec", "cargo.toml", "cargo.lock", "composer.json", "composer.lock", "pom.xml",
		"build.gradle", "build.gradle.kts", "gradle.lockfile", "*.csproj", "packages.lock.json", "mix.exs", "mix.lock", "package.swift",
		"package.resolved"}},
	// The images and infrastructure the code runs on.
	{Name: "infra", Globs: []string{"dockerfile", "dockerfile.*", "*.dockerfile", "containerfile", "docker-compose*.y*ml", "*.tf"}},
}

// authAreas are the areas whose change is the auth kind of change.
var authAreas = []string{"auth", "crypto", "secrets", "payments"}

// SensitiveAreas are the areas named, or all of them when none is: the
// defaults in order, as sensitive sets them, then its own in name order.
func (c *Config) SensitiveAreas(names ...string) []pathglob.Area {
	var out []pathglob.Area
	add := func(name string, def []string) {
		if len(names) > 0 && !slices.Contains(names, name) {
			return
		}
		globs, ok := c.Sensitive[name]
		if !ok {
			globs = def
		}
		out = append(out, pathglob.Area{Name: name, Globs: globs})
	}
	for _, a := range DefaultSensitive {
		add(a.Name, a.Globs)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Sensitive)) {
		if !slices.ContainsFunc(DefaultSensitive, func(a pathglob.Area) bool { return a.Name == name }) {
			add(name, nil)
		}
	}
	return out
}

// SensitiveMatch says which area and glob a changed file matches, as
// "crypto: *crypt*", if any.
func (c *Config) SensitiveMatch(file string) (string, bool) {
	if a, g, ok := pathglob.InAreas(c.SensitiveAreas(), file); ok {
		return a + ": " + g, true
	}
	return "", false
}

var areaName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func (c *Config) checkSensitive() []error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Sensitive)) {
		if !areaName.MatchString(name) {
			errs = append(errs, fmt.Errorf("sensitive: %q isn't an area's name: use lowercase letters, digits, _ and -", name))
		}
		for _, g := range c.Sensitive[name] {
			if err := pathglob.Valid(g); err != nil {
				errs = append(errs, fmt.Errorf("sensitive.%s: glob %q: %v", name, g, err))
			}
		}
	}
	return errs
}
