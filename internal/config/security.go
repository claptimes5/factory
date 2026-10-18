package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Security is the security pass: a review of its own, on routing.security
// with prompts/security.md, of each change that touches code where a
// mistake is an exploit. It reads the change beside the reviewer, and its
// findings block whatever the reviewer decides.
type Security struct {
	// Enabled is on unless set to false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Paths are the globs of files whose change starts a pass; left out,
	// DefaultSecurityPaths, and [] for none, so only Content and the
	// feature map start one. See Match for how they match.
	Paths []string `yaml:"paths,omitempty"`
	// ExtraPaths are added to Paths, to extend the defaults without
	// repeating them.
	ExtraPaths []string `yaml:"extra_paths,omitempty"`
	// Content also starts a pass on code the change adds that runs a
	// command, builds SQL from strings, evaluates code, deserializes
	// unsafe formats, writes raw HTML or turns off TLS checks. On unless
	// false.
	Content *bool `yaml:"content,omitempty"`
}

// On says whether the security pass runs.
func (s Security) On() bool { return s.Enabled == nil || *s.Enabled }

// ContentOn says whether added code can start a pass.
func (s Security) ContentOn() bool { return s.Content == nil || *s.Content }

// DefaultSecurityPaths are the files whose change gets a security pass
// when security.paths is left out: authentication, sessions and
// permissions, cryptography, keys and secrets, untrusted input (uploads,
// webhooks, deserializers, redirects), running commands, SQL and
// migrations, CI workflows, and dependency manifests, lockfiles and the
// images and infrastructure a repo runs on. A glob with no / matches any
// part of a path, so *auth* catches AuthController.php and internal/auth/
// alike (and authors.go: a false start is cheaper than a miss).
var DefaultSecurityPaths = []string{
	// Authentication, sessions, permissions.
	"*auth*", "*oidc*", "*saml*", "jwt*", "*_jwt*", "*login*", "*logout*", "*signin*", "*signup*", "*session*",
	"*cookie*", "*password*", "*permission*", "*rbac*", "acl", "acl.*", "*accesscontrol*", "*access_control*",
	"*csrf*", "*cors*", "*middleware*",
	// Cryptography, keys and secrets.
	"*crypt*", "*cipher*", "*hmac*", "tls", "tls.*", "cert", "certs", "cert.*", "cert_*", "cert-*", "*certificate*",
	"*.pem", "*.key", "*.crt", "*.p12", "*.pfx", "id_rsa*", "*secret*", "*credential*", "*token*", "*apikey*",
	"*api_key*", ".env", ".env.*", "*.env", ".npmrc", ".pypirc",
	// Untrusted input, files and outbound requests.
	"*sanitiz*", "*upload*", "*download*", "*multipart*", "*serializ*", "*webhook*", "*redirect*", "*proxy*",
	"*unzip*", "*untar*", "*archive*",
	// Running commands.
	"exec", "exec.*", "exec_*", "exec-*", "*exec", "*exec.*", "*_exec_*", "*shell*", "*subprocess*",
	// SQL and migrations.
	"*.sql", "*sql*", "migrations", "migrate", "migrate.*",
	// CI, which runs with the repo's secrets.
	".github/workflows/", ".gitlab-ci.yml",
	// Dependencies, and the images and infrastructure the code runs on.
	"go.mod", "go.sum", "go.work", "vendor", "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock",
	"pnpm-lock.yaml", "bun.lock", "bun.lockb", "requirements*.txt", "pipfile", "pipfile.lock", "poetry.lock",
	"pyproject.toml", "uv.lock", "setup.py", "gemfile", "gemfile.lock", "*.gemspec", "cargo.toml", "cargo.lock",
	"composer.json", "composer.lock", "pom.xml", "build.gradle", "build.gradle.kts", "gradle.lockfile", "*.csproj",
	"packages.lock.json", "mix.exs", "mix.lock", "package.swift", "package.resolved", "dockerfile", "dockerfile.*",
	"*.dockerfile", "containerfile", "docker-compose*.y*ml", "*.tf",
}

// SecurityGlobs are the globs that start a pass: Paths (or the defaults,
// when Paths is left out) and ExtraPaths.
func (s Security) SecurityGlobs() []string {
	globs := s.Paths
	if globs == nil {
		globs = DefaultSecurityPaths
	}
	return append(append([]string{}, globs...), s.ExtraPaths...)
}

// Match says which glob a changed file matches, if any. Matching ignores
// case. A glob with no / (`*auth*`, `go.mod`) matches any one part of the
// path: a directory's name or the file's. Any other is matched from the
// repo's root, part by part, where * stops at / and a part that is only
// ** matches any number of parts; a leading / or ./ changes nothing. It
// matches what it names and everything under it, so `internal/billing`,
// `internal/billing/` and `app/*/keys` each match the files in those
// directories.
func (s Security) Match(file string) (string, bool) {
	parts := strings.Split(strings.ToLower(file), "/")
	for _, g := range s.SecurityGlobs() {
		segs, rooted, err := parseGlob(g)
		if err != nil {
			continue // Validate refuses it
		}
		if !rooted {
			for _, p := range parts {
				if ok, _ := path.Match(segs[0], p); ok {
					return g, true
				}
			}
			continue
		}
		if matchParts(segs, parts) {
			return g, true
		}
	}
	return "", false
}

// parseGlob reads a security glob into its parts, lowercased, and whether
// it is matched from the root rather than against any one part.
func parseGlob(g string) (segs []string, rooted bool, err error) {
	g = strings.ToLower(strings.TrimSpace(g))
	if strings.Contains(g, `\`) {
		return nil, false, errors.New(`paths use /, not \`)
	}
	rooted = strings.Contains(g, "/") || strings.Contains(g, "**")
	g = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(g, "./"), "/"), "/")
	if g == "" {
		return nil, false, errors.New("is empty")
	}
	for _, s := range strings.Split(g, "/") {
		switch {
		case s == "" || s == "." || s == "..":
			return nil, false, errors.New("has an empty, . or .. part")
		case strings.Contains(s, "**") && s != "**":
			return nil, false, errors.New("** must be a whole part of the path, as in a/**/b")
		}
		if _, err := path.Match(s, "x"); err != nil {
			return nil, false, err
		}
		segs = append(segs, s)
	}
	return segs, rooted, nil
}

// matchParts matches a rooted glob's parts against a path's. A path
// longer than the glob matches when the glob names a directory it is in.
func matchParts(g, f []string) bool {
	if len(g) == 0 {
		return true
	}
	if g[0] == "**" {
		for k := 0; k <= len(f); k++ {
			if matchParts(g[1:], f[k:]) {
				return true
			}
		}
		return false
	}
	if len(f) == 0 {
		return false
	}
	ok, _ := path.Match(g[0], f[0])
	return ok && matchParts(g[1:], f[1:])
}

// reviewRoutes are the routing entries whose model reviews a change.
var reviewRoutes = []string{"review", "review.medium_risk", "review.high_risk"}

// SecurityRoute is the route the security pass runs on for an item built
// on the routes given (its implement jobs' harnesses and models): the
// first of routing.security, the critic's route and the implement routes,
// strongest first, that is neither a reviewer's model nor one that built
// the item. So by default it is another vendor's model than the
// reviewer's, and never the model whose code it reads. With every one
// ruled out, it is the first that isn't a reviewer's.
func (c *Config) SecurityRoute(built ...Route) Route {
	var cands []Route
	if r, ok := c.Routing["security"]; ok {
		cands = append(cands, r)
	}
	cands = append(cands, c.CriticRoute())
	for _, size := range []string{"L", "M", "S", "XS"} {
		if r, ok := c.Routing["implement."+size]; ok {
			r.Skills, r.Use, r.Instructions, r.InstructionsFile, r.Fast = "", nil, "", "", false
			cands = append(cands, r)
		}
	}
	reviewers := c.reviewers()
	first := -1
	for i, r := range cands {
		if sameAsAny(r, reviewers) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !sameAsAny(r, built) {
			return r
		}
	}
	if first < 0 {
		return cands[0]
	}
	return cands[first]
}

// sameAsAny says whether r may be the same harness and model as one of
// routes.
func sameAsAny(r Route, routes []Route) bool {
	for _, o := range routes {
		if o.Harness == r.Harness && sameModel(r.Harness, o.Model, r.Model) {
			return true
		}
	}
	return false
}

func (c *Config) checkSecurity() []error {
	var errs []error
	for _, g := range append(append([]string{}, c.Security.Paths...), c.Security.ExtraPaths...) {
		if _, _, err := parseGlob(g); err != nil {
			errs = append(errs, fmt.Errorf("security: glob %q: %v", g, err))
		}
	}
	if r, ok := c.Routing["security"]; ok {
		for _, rk := range reviewRoutes {
			if rv, ok := c.Routing[rk]; ok && rv.Harness == r.Harness && sameModel(r.Harness, rv.Model, r.Model) {
				errs = append(errs, fmt.Errorf("routing.security runs on %s/%s, as routing.%s does: the security pass needs a model of its own, to catch what the reviewer's misses", r.Harness, r.Model, rk))
				break
			}
		}
	}
	// What it runs on when left out, or a builder's: never a reviewer's.
	if r := c.SecurityRoute(); sameAsAny(r, c.reviewers()) {
		errs = append(errs, fmt.Errorf("the security pass would run on %s/%s, a reviewer's model: set routing.security to another", r.Harness, r.Model))
	}
	return errs
}

func (c *Config) reviewers() []Route {
	var out []Route
	for _, k := range reviewRoutes {
		if r, ok := c.Routing[k]; ok {
			out = append(out, r)
		}
	}
	return out
}
