package config

import (
	"strings"
	"testing"
)

func TestSecurityDefaultsMatch(t *testing.T) {
	t.Parallel()
	var c Config
	if !c.Security.On() || !c.Security.ContentOn() {
		t.Fatal("the security pass and its content signals are on by default")
	}
	for file, want := range map[string]string{
		"internal/auth/check.go":             "auth: auth",
		"app/controllers/AuthController.php": "auth: *authcontroller*",
		"src/authService.ts":                 "auth: *authservice*",
		"internal/harness/codexauth.go":      "access: *auth*",
		"app/models/user_session.rb":         "access: *session*",
		"web/src/UserLogin.tsx":              "access: *login*",
		"lib/jwt.go":                         "auth: *jwt*",
		"pkg/crypto/aes.go":                  "crypto: *crypt*",
		"internal/payments/charge.go":        "payments: payment*",
		"db/migrations/0001_init.sql":        "sql: *.sql",
		"internal/store/migrations/0002.go":  "migrations: migrations",
		"go.mod":                             "dependencies: go.mod",
		"web/package-lock.json":              "dependencies: package-lock.json",
		"Gemfile.lock":                       "dependencies: gemfile.lock",
		"deploy/Dockerfile":                  "infra: dockerfile",
		"internal/runner/exec.go":            "exec: exec.*",
		"api/webhooks/github.py":             "input: *webhook*",
		"requirements-dev.txt":               "dependencies: requirements*.txt",
		// Look-alikes don't.
		".github/workflows/ci.yml":        "ci: .github/workflows/",
		".env.production":                 "secrets: .env.*",
		"go.work":                         "dependencies: go.work",
		"internal/lexer/tokens.go":        "access: *token*",
		"deploy/docker-compose.prod.yaml": "infra: docker-compose*.y*ml",
		"internal/runner/executor.go":     "",
		"docs/certification.go":           "",
		"internal/pipeline/add.go":        "",
		"cmd/factory/main.go":             "",
		"web/src/screens/Metrics.tsx":     "",
	} {
		g, ok := c.SecurityMatch(file)
		if want == "" && ok || want != "" && (!ok || !strings.EqualFold(g, want)) {
			t.Errorf("%s: matched %q (%v), want %q", file, g, ok, want)
		}
	}
}

// security.paths, when set, replaces the sensitive list for the pass;
// extra_paths adds to either.
func TestSecurityPathsReplaceAndExtend(t *testing.T) {
	t.Parallel()
	c := Config{Security: Security{Paths: []string{"app/*/keys.rb"}, ExtraPaths: []string{"internal/billing/", "*.tf"}}}
	for file, want := range map[string]bool{
		"app/models/keys.rb":          true,
		"app/models/deep/keys.rb":     false, // * stops at /
		"internal/billing/charge.go":  true,
		"Internal/Billing/x.go":       true,  // case doesn't matter
		"other/internal/billing/x.go": false, // a directory glob starts at the root
		"infra/main.tf":               true,
		"go.mod":                      false, // the sensitive list is replaced
	} {
		if _, ok := c.SecurityMatch(file); ok != want {
			t.Errorf("%s: %v", file, ok)
		}
	}
	c = Config{Security: Security{ExtraPaths: []string{"internal/ledger/"}}}
	for _, f := range []string{"go.mod", "internal/ledger/post.go"} {
		if _, ok := c.SecurityMatch(f); !ok {
			t.Errorf("extra_paths alone extends the sensitive list: %s", f)
		}
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
	if err := c.Validate(); err != nil {
		t.Fatalf("security on the reviewer's model is a warning, not an error: %v", err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "as routing.review does") {
		t.Fatalf("security on the reviewer's model: %v", w)
	}
	if r, rv := c.SecurityRoute(), c.Routing["review"]; r.Harness != rv.Harness || r.Model != rv.Model {
		t.Fatalf("set on the reviewer's model, the security pass still runs there: %+v", r)
	}
	c = load(t)
	c.Routing["security"] = Route{Harness: "codex", Model: "gpt-6.1-sol", Effort: "high"}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "as routing.review does") {
		t.Fatalf("security on the reviewer's model at another effort: %v", w)
	}
	if w := load(t).Warnings(); len(w) != 0 {
		t.Fatalf("factory.yaml warns: %v", w)
	}
	c = load(t)
	c.Security.Paths = []string{"db/", "*.sql"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityBlockParses(t *testing.T) {
	t.Parallel()
	y, err := exampleYAML()
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

// security.paths: [] is no path globs at all: only the feature map and
// content start a pass. (pathglob's tests have the glob forms.)
func TestSecurityPathsNone(t *testing.T) {
	t.Parallel()
	none := Config{Security: Security{Paths: []string{}}}
	if _, ok := none.SecurityMatch("go.mod"); ok {
		t.Fatal("paths: [] still matched a default")
	}
	y, err := exampleYAML()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(strings.Replace(string(y), "\nescalation:", "\nsecurity: {paths: []}\n\nescalation:", 1)))
	if _, ok := c.SecurityMatch("internal/auth/a.go"); err != nil || c.Security.Paths == nil || ok {
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
