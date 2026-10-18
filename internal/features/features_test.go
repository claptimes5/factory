package features

import (
	"strings"
	"testing"
)

const sample = `features:
  - name: Login
    description: Sign in with email
    paths: [app/controllers/sessions_controller.rb, app/views/sessions/, "app/models/user*.rb"]
    tests: [spec/system/login_spec.rb]
    check: bundle exec rspec spec/system/login_spec.rb
    risk: high
  - name: Profile
    paths: [app/controllers/profiles_controller.rb, app/models/user.rb]
    tests: []
`

func TestTouched(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	hits, unmapped := m.Touched([]string{"app/models/user.rb", "app/views/sessions/new.html.erb", "spec/system/login_spec.rb", "lib/tasks/x.rake", "features.yaml", "spec/models/user_spec.rb"})
	if len(hits) != 2 || hits[0].Feature.Name != "Login" || strings.Join(hits[0].Files, ",") != "app/models/user.rb,app/views/sessions/new.html.erb,spec/system/login_spec.rb" ||
		hits[1].Feature.Name != "Profile" || strings.Join(hits[1].Files, ",") != "app/models/user.rb" {
		t.Fatalf("hits %+v", hits)
	}
	if strings.Join(unmapped, ",") != "lib/tasks/x.rake" {
		t.Fatalf("unmapped %v", unmapped)
	}
	if risk, f := MaxRisk(hits); risk != "high" || f != "Login" {
		t.Fatalf("risk %s %s", risk, f)
	}
	// A directory without a slash covers what's under it, not a prefix.
	if !match("app/views", "app/views/a.erb") || match("app/view", "app/views/a.erb") {
		t.Fatal("directory match")
	}
}

func TestParseErrors(t *testing.T) {
	for in, want := range map[string]string{
		"features: [{name: A}]": "has no paths",
		"features: [{name: A, paths: [a]}, {name: a, paths: [b]}]": "listed twice",
		"features: [{name: A, paths: [a], risk: severe}]":          "risk must be",
		"features: [{name: A, paths: [a], owner: me}]":             "owner",
		"features: [{name: A, paths: ['[']}]":                      "bad pattern",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", in, err, want)
		}
	}
}

func TestCheck(t *testing.T) {
	m, _ := Parse([]byte(sample))
	h := m.Check([]string{"app/models/user.rb", "app/controllers/sessions_controller.rb", "app/controllers/home_controller.rb", "spec/system/login_spec.rb", "README.md"})
	if h.Code != 3 || h.Covered != 2 || strings.Join(h.Untested, ",") != "Profile" || strings.Join(h.Stale, ",") != "Login: app/views/sessions/,Profile: app/controllers/profiles_controller.rb" {
		t.Fatalf("%+v", h)
	}
}

func TestHasProperties(t *testing.T) {
	for text, want := range map[string]bool{
		"import \"pgregory.net/rapid\"\n":                    true,
		"func FuzzParse(f *testing.F) {}\n":                  true,
		"func FuzzyMatch(t *testing.T) {}\n":                 false,
		"from hypothesis import given\n":                     true,
		"import fc from 'fast-check'\n":                      true,
		"#include <rapidcheck.h>\n":                          true,
		"import org.scalacheck.Prop\n":                       true,
		"import Test.QuickCheck\n":                           true,
		"// TODO: use proptest here\nfn it_works() {}\n":     false,
		"# switch to hypothesis later\ndef test_x(): pass\n": false,
		"def test_total():\n    assert total([1, 2]) == 3\n": false,
	} {
		if got := HasProperties(text); got != want {
			t.Errorf("HasProperties(%q) = %v, want %v", text, got, want)
		}
	}
}
