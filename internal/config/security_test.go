package config

import (
	"os"
	"strings"
	"testing"
)

func TestSecurityDefaultsMatch(t *testing.T) {
	t.Parallel()
	var s Security
	if !s.On() || !s.ContentOn() {
		t.Fatal("the security pass and its content signals are on by default")
	}
	for file, want := range map[string]string{
		"internal/auth/check.go":             "*auth*",
		"app/controllers/AuthController.php": "*auth*",
		"src/authService.ts":                 "*auth*",
		"internal/harness/codexauth.go":      "*auth*",
		"app/models/user_session.rb":         "*session*",
		"web/src/UserLogin.tsx":              "*login*",
		"pkg/crypto/aes.go":                  "*crypt*",
		"db/migrations/0001_init.sql":        "*.sql",
		"internal/store/migrations/0002.go":  "migrations",
		"go.mod":                             "go.mod",
		"web/package-lock.json":              "package-lock.json",
		"Gemfile.lock":                       "gemfile.lock",
		"deploy/Dockerfile":                  "dockerfile",
		"internal/runner/exec.go":            "exec.*",
		"api/webhooks/github.py":             "*webhook*",
		"requirements-dev.txt":               "requirements*.txt",
		// Look-alikes don't.
		".github/workflows/ci.yml":        ".github/workflows/",
		".env.production":                 ".env.*",
		"go.work":                         "go.work",
		"internal/lexer/tokens.go":        "*token*",
		"deploy/docker-compose.prod.yaml": "docker-compose*.y*ml",
		"internal/runner/executor.go":     "",
		"docs/certification.go":           "",
		"internal/pipeline/add.go":        "",
		"cmd/factory/main.go":             "",
		"web/src/screens/Metrics.tsx":     "",
	} {
		g, ok := s.Match(file)
		if want == "" && ok || want != "" && (!ok || !strings.EqualFold(g, want)) {
			t.Errorf("%s: matched %q (%v), want %q", file, g, ok, want)
		}
	}
}

func TestSecurityPathsReplaceAndExtend(t *testing.T) {
	t.Parallel()
	s := Security{Paths: []string{"app/*/keys.rb"}, ExtraPaths: []string{"internal/billing/", "*.tf"}}
	for file, want := range map[string]bool{
		"app/models/keys.rb":          true,
		"app/models/deep/keys.rb":     false, // * stops at /
		"internal/billing/charge.go":  true,
		"Internal/Billing/x.go":       true,  // case doesn't matter
		"other/internal/billing/x.go": false, // a directory glob starts at the root
		"infra/main.tf":               true,
		"go.mod":                      false, // the defaults are replaced
	} {
		if _, ok := s.Match(file); ok != want {
			t.Errorf("%s: %v", file, ok)
		}
	}
	if g := (Security{ExtraPaths: []string{"*.tf"}}).SecurityGlobs(); len(g) != len(DefaultSecurityPaths)+1 {
		t.Fatalf("extra_paths alone extends the defaults: %d globs", len(g))
	}
}

func TestSecurityConfigIsChecked(t *testing.T) {
	t.Parallel()
	c := load(t)
	if r := c.SecurityRoute(); r.Harness != "claude" || r.Model != "opus" {
		t.Fatalf("factory.yaml's security route %+v", r)
	}
	delete(c.Routing, "security")
	if r, cr := c.SecurityRoute(), c.CriticRoute(); r.Harness != cr.Harness || r.Model != cr.Model || r.Model != "opus" {
		t.Fatalf("left out, the security route is the critic's: %+v, %+v", r, cr)
	}
	for _, bad := range []string{"[", `app\auth`, " ", "a**b", "a/../b", "/"} {
		c := load(t)
		c.Security.ExtraPaths = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "security:") {
			t.Errorf("glob %q accepted: %v", bad, err)
		}
	}
	c = load(t)
	c.Routing["security"] = c.Routing["review"]
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "the security pass needs a model of its own") {
		t.Fatalf("security on the reviewer's model: %v", err)
	}
	c = load(t)
	c.Routing["security"] = Route{Harness: "codex", Model: "gpt-6.1-sol", Effort: "high"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "as routing.review does") {
		t.Fatalf("security on the reviewer's model at another effort: %v", err)
	}
	c = load(t)
	c.Security.Paths = []string{"db/", "*.sql"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityBlockParses(t *testing.T) {
	t.Parallel()
	y, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(strings.Replace(string(y), "\nescalation:", "\nsecurity: {enabled: false, extra_paths: [internal/billing/], content: false}\n\nescalation:", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Security.On() || c.Security.ContentOn() || len(c.Security.ExtraPaths) != 1 {
		t.Fatalf("security %+v", c.Security)
	}
}

// A glob with a / names a directory too, with or without the trailing /;
// ** is any number of parts; a leading / or ./ changes nothing.
func TestSecurityGlobForms(t *testing.T) {
	t.Parallel()
	s := Security{Paths: []string{"internal/billing", "./app/**/keys", "/infra/*.tf", "config/**"}}
	for file, want := range map[string]bool{
		"internal/billing/charge.go":   true,
		"internal/billing":             true,
		"internal/billingx/charge.go":  false,
		"app/keys/a.rb":                true,
		"app/models/deep/keys/b.rb":    true,
		"infra/main.tf":                true,
		"other/infra/main.tf":          false,
		"config/prod/secrets.yml":      true,
		"web/config/x.yml":             false,
		"app/models/deep/notkeys/b.rb": false,
	} {
		if _, ok := s.Match(file); ok != want {
			t.Errorf("%s: %v", file, ok)
		}
	}
	// [] is no path globs at all: only the feature map and content start one.
	none := Security{Paths: []string{}}
	if _, ok := none.Match("go.mod"); ok {
		t.Fatal("paths: [] still matched a default")
	}
	y, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(strings.Replace(string(y), "\nescalation:", "\nsecurity: {paths: []}\n\nescalation:", 1)))
	if err != nil || c.Security.Paths == nil || len(c.Security.SecurityGlobs()) != 0 {
		t.Fatalf("paths: [] parsed to %#v, %v", c.Security.Paths, err)
	}
}

// Left out, the security route is never a reviewer's model, and never one
// that built the item.
func TestSecurityRouteFallback(t *testing.T) {
	t.Parallel()
	c := load(t)
	delete(c.Routing, "security")
	if r := c.SecurityRoute(); r.Model != "opus" {
		t.Fatalf("default %+v", r)
	}
	// An item Opus built gets the next strongest tier.
	if r := c.SecurityRoute(Route{Harness: "claude", Model: "claude-opus-5"}); r.Harness != "claude" || r.Model != "sonnet" {
		t.Fatalf("for an Opus build %+v", r)
	}
	// Set, it still gives way to a model that built the item.
	c.Routing["security"] = Route{Harness: "claude", Model: "opus"}
	if r := c.SecurityRoute(Route{Harness: "claude", Model: "opus"}); r.Model == "opus" {
		t.Fatalf("security on its builder's model %+v", r)
	}
	// A critic on the reviewer's model isn't the security pass's fallback.
	delete(c.Routing, "security")
	c.Routing["critic"] = c.Routing["review"]
	if r := c.SecurityRoute(); r.Harness != "claude" || r.Model != "opus" {
		t.Fatalf("critic on the reviewer's model: %+v", r)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
