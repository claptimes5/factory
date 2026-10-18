package config

import (
	"fmt"
	"slices"

	"github.com/claptimes5/factory/internal/diffrisk"
	"github.com/claptimes5/factory/internal/pathglob"
)

// Risk says how the factory rates a change from its diff once it's
// built, and which kinds of change always wait for you. Triage rates risk
// from the issue's text; after the build the diff is rated too, and the
// item keeps the higher of the two. The diff never lowers it.
type Risk struct {
	// MediumLines and HighLines: a change of at least this many changed
	// lines, lockfiles left out, is at least medium or high risk. Default
	// 150 and 300; -1 turns one off.
	MediumLines int `yaml:"medium_lines,omitempty"`
	HighLines   int `yaml:"high_lines,omitempty"`
	// MediumFiles and HighFiles are the same for changed files: default
	// 10 and 25.
	MediumFiles int `yaml:"medium_files,omitempty"`
	HighFiles   int `yaml:"high_files,omitempty"`
	// MediumPaths and HighPaths: a change touching one is at least medium
	// or high risk. Medium defaults to deploy and infrastructure files;
	// high to none. See pathglob.Match for the patterns.
	MediumPaths []string `yaml:"medium_paths,omitempty"`
	HighPaths   []string `yaml:"high_paths,omitempty"`
	// AuthPaths are auth code and MigrationPaths a database's migrations,
	// added to the sensitive list's: its auth, crypto, secrets and
	// payments areas are auth code, and its migrations area migrations.
	// A change to either is high risk. Set the sensitive list instead.
	AuthPaths      []string `yaml:"auth_paths,omitempty"`
	MigrationPaths []string `yaml:"migration_paths,omitempty"`
	// NeedsPerson are the kinds of change that never merge themselves,
	// whatever their risk: migrations, new_dependencies, auth and
	// deleted_tests, all four by default. [] lists none.
	NeedsPerson []string `yaml:"needs_person"`
}

// Default risk settings. They were picked before any real use; see "To
// revisit after real use" in docs/quality.md.
var (
	defaultMediumPaths = []string{"Dockerfile", "Dockerfile.*", "*.dockerfile", "docker-compose*.yml", "docker-compose*.yaml",
		"*.tf", "terraform/", "infra/", "deploy/", "k8s/", "helm/", "Procfile", "fly.toml", "vercel.json", "netlify.toml"}
)

func (c *Config) riskDefaults() {
	r := &c.Risk
	for _, d := range []struct {
		v   *int
		def int
	}{{&r.MediumLines, 150}, {&r.HighLines, 300}, {&r.MediumFiles, 10}, {&r.HighFiles, 25}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if r.MediumPaths == nil {
		r.MediumPaths = slices.Clone(defaultMediumPaths)
	}
	if r.NeedsPerson == nil {
		r.NeedsPerson = slices.Clone(diffrisk.Kinds)
	}
}

func (c *Config) checkRisk() []error {
	var errs []error
	r := c.Risk
	for _, n := range []struct {
		name string
		v    int
	}{{"medium_lines", r.MediumLines}, {"high_lines", r.HighLines}, {"medium_files", r.MediumFiles}, {"high_files", r.HighFiles}} {
		if n.v < -1 {
			errs = append(errs, fmt.Errorf("risk.%s must be a positive number, or -1 to turn it off", n.name))
		}
	}
	if r.MediumLines > 0 && r.HighLines > 0 && r.MediumLines > r.HighLines {
		errs = append(errs, fmt.Errorf("risk.medium_lines (%d) is above risk.high_lines (%d)", r.MediumLines, r.HighLines))
	}
	if r.MediumFiles > 0 && r.HighFiles > 0 && r.MediumFiles > r.HighFiles {
		errs = append(errs, fmt.Errorf("risk.medium_files (%d) is above risk.high_files (%d)", r.MediumFiles, r.HighFiles))
	}
	for _, l := range []struct {
		name string
		ps   []string
	}{{"medium_paths", r.MediumPaths}, {"high_paths", r.HighPaths}, {"auth_paths", r.AuthPaths}, {"migration_paths", r.MigrationPaths}} {
		for _, p := range l.ps {
			if err := pathglob.Valid(p); err != nil {
				errs = append(errs, fmt.Errorf("risk.%s: glob %q: %v", l.name, p, err))
			}
		}
	}
	for _, k := range r.NeedsPerson {
		if !slices.Contains(diffrisk.Kinds, k) {
			errs = append(errs, fmt.Errorf("risk.needs_person: %q isn't a kind of change; use migrations, new_dependencies, auth or deleted_tests", k))
		}
	}
	return errs
}

// RiskRules are the risk settings as the diff's rating takes them.
func (c *Config) RiskRules() diffrisk.Rules {
	r := c.Risk
	on := func(n int) int { return max(n, 0) } // -1 is off
	auth, migrations := c.SensitiveAreas(authAreas...), c.SensitiveAreas("migrations")
	if r.AuthPaths != nil {
		auth = append(auth, pathglob.Area{Name: "auth", Globs: r.AuthPaths})
	}
	if r.MigrationPaths != nil {
		migrations = append(migrations, pathglob.Area{Name: "migrations", Globs: r.MigrationPaths})
	}
	return diffrisk.Rules{
		MediumLines: on(r.MediumLines), HighLines: on(r.HighLines), MediumFiles: on(r.MediumFiles), HighFiles: on(r.HighFiles),
		MediumPaths: r.MediumPaths, HighPaths: r.HighPaths, Auth: auth, Migrations: migrations, NeedsPerson: r.NeedsPerson,
	}
}
