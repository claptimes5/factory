// Package features reads a repo's feature map, features.yaml: what the
// product does, where each feature lives and what proves it works. The
// factory uses it to find what a change touches (its blast radius), run
// the checks that prove those features, and raise review for risky ones.
package features

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/gates"
)

// File is where the map lives, at the repo root.
const File = "features.yaml"

// Files are the names the map may have; the first found is used.
var Files = []string{"features.yaml", "features.yml"}

// Feature is one user-facing capability.
type Feature struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Paths are the files that implement it: a file, a directory ending
	// in /, or a glob (*, ?, [ ]) matched against the whole path.
	Paths []string `yaml:"paths"`
	// Tests are the test files that prove it; [] says it has none yet.
	Tests []string `yaml:"tests"`
	// Check runs only the tests that prove it, such as
	// `bundle exec rspec spec/system/login_spec.rb`. When a change touches
	// the feature the factory runs it as a gate.
	Check string `yaml:"check,omitempty"`
	// Risk is low, medium or high. A change that touches a high-risk
	// feature is reviewed as high risk, whatever triage said.
	Risk string `yaml:"risk,omitempty"`
}

// Map is a parsed features.yaml.
type Map struct {
	Features []Feature `yaml:"features"`
}

// Parse reads and checks a map.
func Parse(b []byte) (*Map, error) {
	var m Map
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	var errs []error
	seen := map[string]bool{}
	for i, f := range m.Features {
		switch {
		case strings.TrimSpace(f.Name) == "":
			errs = append(errs, fmt.Errorf("feature %d has no name", i+1))
		case seen[strings.ToLower(f.Name)]:
			errs = append(errs, fmt.Errorf("feature %q is listed twice", f.Name))
		}
		seen[strings.ToLower(f.Name)] = true
		if len(f.Paths) == 0 {
			errs = append(errs, fmt.Errorf("feature %q has no paths", f.Name))
		}
		for _, p := range append(slices.Clone(f.Paths), f.Tests...) {
			if _, err := path.Match(p, ""); err != nil {
				errs = append(errs, fmt.Errorf("feature %q: bad pattern %q", f.Name, p))
			}
		}
		if f.Risk != "" && f.Risk != "low" && f.Risk != "medium" && f.Risk != "high" {
			errs = append(errs, fmt.Errorf("feature %q: risk must be low, medium or high, not %q", f.Name, f.Risk))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return &m, nil
}

// Match reports whether file is covered by pattern: a file, a directory
// (with or without a trailing /), or a glob matched against the whole
// path, where * stops at / and there is no **.
func Match(pattern, file string) bool { return match(pattern, file) }

// match reports whether file is covered by pattern.
func match(pattern, file string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	switch {
	case strings.HasSuffix(pattern, "/"):
		return strings.HasPrefix(file, pattern)
	case strings.ContainsAny(pattern, "*?["):
		ok, _ := path.Match(pattern, file)
		return ok
	}
	return file == pattern || strings.HasPrefix(file, pattern+"/")
}

func matchAny(patterns []string, file string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return match(p, file) })
}

// Hit is a feature a change touches, and the changed files that do.
type Hit struct {
	Feature Feature
	Files   []string
}

// Touched finds the features the changed files touch: their code, or the
// tests that prove them. Unmapped are the changed files, other than tests
// and the map itself, that no feature covers.
func (m *Map) Touched(changed []string) (hits []Hit, unmapped []string) {
	for _, f := range m.Features {
		var files []string
		for _, c := range changed {
			if matchAny(f.Paths, c) || matchAny(f.Tests, c) {
				files = append(files, c)
			}
		}
		if len(files) > 0 {
			hits = append(hits, Hit{Feature: f, Files: files})
		}
	}
	for _, c := range changed {
		if gates.IsTest(c) || slices.Contains(Files, c) {
			continue
		}
		if !slices.ContainsFunc(m.Features, func(f Feature) bool { return matchAny(f.Paths, c) }) {
			unmapped = append(unmapped, c)
		}
	}
	return hits, unmapped
}

var riskRank = map[string]int{"": 0, "low": 1, "medium": 2, "high": 3}

// MaxRisk is the highest risk among the hits, and the feature that has
// it; "" when none sets one.
func MaxRisk(hits []Hit) (risk, feature string) {
	for _, h := range hits {
		if riskRank[h.Feature.Risk] > riskRank[risk] {
			risk, feature = h.Feature.Risk, h.Feature.Name
		}
	}
	return risk, feature
}

// Higher reports whether risk a is above b.
func Higher(a, b string) bool { return riskRank[a] > riskRank[b] }

// Health is how well the map fits the repo's files.
type Health struct {
	Code     int      // non-test source files
	Covered  int      // of those, the ones some feature's paths cover
	Untested []string // features with no tests
	Stale    []string // "feature: pattern" for paths or tests that match no file
}

// TestFiles are the files among files that the feature's tests name.
func (f Feature) TestFiles(files []string) []string {
	var out []string
	for _, file := range files {
		if matchAny(f.Tests, file) {
			out = append(out, file)
		}
	}
	return out
}

// PropertyMarkers are what a property-based or fuzz test uses, by stack:
// the library's import or name. They match case-insensitively.
var PropertyMarkers = []string{
	"testing/quick", "pgregory.net/rapid", "flyingmutant/rapid", "gopter", // Go
	"from hypothesis", "import hypothesis", "import atheris", // Python
	"fast-check", "fc.assert", "fc.property", "jsverify", // JS/TS
	"prop_check", "rantly", // Ruby
	"proptest", "quickcheck", "fuzz_target!", "libfuzzer_sys", "bolero", // Rust, Haskell
	"exunitproperties", "streamdata", // Elixir
	"jqwik", "junit-quickcheck", "kotest.property", "org.scalacheck", // JVM
	"fscheck", "cscheck", // .NET
	"rapidcheck", "swiftcheck", "eris\\", // C++, Swift, PHP
}

// goFuzz is a Go fuzz test's signature.
var goFuzz = regexp.MustCompile(`func Fuzz\w*\(\s*\w+\s+\*testing\.F\s*\)`)

// HasProperties reports whether a test file's text uses property-based
// or fuzz testing. Lines that are only comments don't count.
func HasProperties(text string) bool {
	var code strings.Builder
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//") || t == "#" || strings.HasPrefix(t, "# ") || strings.HasPrefix(t, "--") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		code.WriteString(line + "\n")
	}
	s := code.String()
	if goFuzz.MatchString(s) {
		return true
	}
	l := strings.ToLower(s)
	return slices.ContainsFunc(PropertyMarkers, func(m string) bool { return strings.Contains(l, m) })
}

// Check measures the map against the repo's files.
func (m *Map) Check(files []string) Health {
	var h Health
	for _, f := range files {
		if !isSource(f) || gates.IsTest(f) {
			continue
		}
		h.Code++
		if slices.ContainsFunc(m.Features, func(ft Feature) bool { return matchAny(ft.Paths, f) }) {
			h.Covered++
		}
	}
	for _, ft := range m.Features {
		if len(ft.Tests) == 0 && ft.Check == "" {
			h.Untested = append(h.Untested, ft.Name)
		}
		for _, p := range append(slices.Clone(ft.Paths), ft.Tests...) {
			if !slices.ContainsFunc(files, func(f string) bool { return match(p, f) }) {
				h.Stale = append(h.Stale, ft.Name+": "+p)
			}
		}
	}
	return h
}

var sourceExt = map[string]bool{
	".go": true, ".rb": true, ".py": true, ".rs": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".mjs": true, ".cjs": true, ".vue": true, ".svelte": true, ".erb": true, ".java": true, ".kt": true,
	".swift": true, ".php": true, ".cs": true, ".ex": true, ".exs": true, ".scala": true, ".c": true, ".cc": true, ".cpp": true, ".h": true,
}

// isSource is hand-written code: not minified bundles or type stubs.
func isSource(f string) bool {
	base := path.Base(f)
	return sourceExt[path.Ext(f)] && !strings.Contains(base, ".min.") && !strings.HasSuffix(base, ".d.ts")
}
