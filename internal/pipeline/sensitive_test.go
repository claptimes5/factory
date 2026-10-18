package pipeline

import (
	"os"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/diffrisk"
	"github.com/claptimes5/factory/internal/reviewmd"
)

// With the repo's factory.yaml, one sensitive list decides the security
// pass, the diff's risk and REVIEW.md's skip alike: auth, crypto, secrets
// and payments code gets a pass, is high risk and waits for a person, and
// none of it can be skipped. Files whose names only look like it get the
// pass and the guard, but not the person.
func TestSensitiveCodeIsTreatedAlike(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseIn(b, "../..")
	if err != nil {
		t.Fatal(err)
	}
	for file, auth := range map[string]bool{
		"app/Http/Controllers/AuthController.php": true,
		"internal/crypto/aes.go":                  true,
		"pkg/secrets/vault.go":                    true,
		"internal/api/tokens.go":                  false, // *token*: a pass, but a lexer's tokens.go looks the same
		"internal/payments/charge.go":             true,
		"lib/jwt.go":                              true,
		"internal/upload/handler.go":              false, // input: a pass, but not the auth kind
		"internal/parser/token.go":                false,
		"web/src/lexer/tokenizer.ts":              false,
		"web/src/styles/tokens.css":               false,
		"internal/ratelimit/token_bucket.go":      false,
		"app/models/author.rb":                    false,
		"app/views/authors/index.html.erb":        false,
		"internal/middleware/logging.go":          false,
		"internal/server/cors.go":                 false,
		"web/src/CookieConsentBanner.tsx":         false,
		"app/models/possession.rb":                false,
		"internal/git/checkout.go":                false,
		"web/src/StripedTable.tsx":                false,
		"cmd/migrate.go":                          false, // sql: not a migration
	} {
		if g, ok := c.SecurityMatch(file); !ok {
			t.Errorf("%s: no security pass", file)
		} else if len(securityPaths(c.SecurityMatch, []string{file}, "")) != 1 {
			t.Errorf("%s: matched %s but no reason", file, g)
		}
		a := diffrisk.Rate(diffrisk.Change{Files: []diffrisk.File{{Path: file, Status: "M", Added: 1}}}, c.RiskRules())
		np := a.NeedsPerson()
		if raised := a.Risk == "high" && len(np) == 1 && np[0].Kind == diffrisk.Auth; raised != auth || !auth && len(np) > 0 {
			t.Errorf("%s: risk %s, needs a person for %+v", file, a.Risk, np)
		}
		if !reviewmd.SensitiveIn(c.SensitiveAreas(), file) {
			t.Errorf("%s: REVIEW.md could skip it", file)
		}
	}
	a := diffrisk.Rate(diffrisk.Change{Files: []diffrisk.File{{Path: "internal/crypto/aes.go", Status: "M", Added: 1}}}, c.RiskRules())
	if got, want := a.Reasons[0].Text(prCode), "changes crypto code: `*crypt*` in `internal/crypto/aes.go`"; got != want {
		t.Fatalf("reason %q, want %q", got, want)
	}
}
