package pathglob

import "testing"

// The cases of the two matchers this one replaces, diffrisk's and the
// security pass's, and how they meet.
func TestMatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pattern, file string
		want          bool
	}{
		// No /: any one part of the path.
		{"auth", "internal/auth/token.go", true},
		{"auth", "author.go", false},
		{"auth.*", "pkg/Auth.go", true},
		{"*auth*", "app/controllers/AuthController.php", true},
		{"*auth*", "internal/harness/codexauth.go", true},
		{"session*", "app/controllers/sessions_controller.rb", true},
		{"Dockerfile", "web/Dockerfile", true},
		{"go.mod", "go.mod", true},
		{"exec.*", "internal/runner/executor.go", false},
		// Ending in /: a directory at any depth, never the file.
		{"migrations/", "internal/store/migrations/0018.sql", true},
		{"migrations/", "migrations/0018.sql", true},
		{"migrations/", "docs/migrations", false},
		{"x/", "x", false},
		{"src/*/", "src/main.go", false}, // the old security matcher matched this
		{"src/*/", "src/app/main.go", true},
		// With a /: from the root, the path and what's under it.
		{"db/migrate/", "db/migrate/1.rb", true},
		{"/db/migrate/", "db/migrate/1.rb", true},
		{"db/migrate/", "app/db/migrate/1.rb", false},
		{"db/migrate/", "db/migrate", false}, // a file, not a directory
		{"./deploy/*.yaml", "deploy/prod.yaml", true},
		{"deploy/*.yaml", "deploy/prod.yaml", true},
		{"deploy/*.yaml", "deploy/eu/prod.yaml", false}, // * stops at /
		{"app/*/keys.rb", "app/models/keys.rb", true},
		{"app/*/keys.rb", "app/models/deep/keys.rb", false},
		{"internal/billing/", "internal/billing/charge.go", true},
		{"internal/billing/", "Internal/Billing/x.go", true}, // case doesn't matter
		{"internal/billing/", "other/internal/billing/x.go", false},
		{"internal/billing", "internal/billing/charge.go", true},
		{"internal/billing", "internal/billing", true},
		{"internal/billing", "internal/billingx/charge.go", false},
		{".github/workflows/", ".github/workflows/ci.yml", true},
		// ** is any number of parts.
		{"./app/**/keys", "app/keys/a.rb", true},
		{"./app/**/keys", "app/models/deep/keys/b.rb", true},
		{"./app/**/keys", "app/models/deep/notkeys/b.rb", false},
		{"/infra/*.tf", "infra/main.tf", true},
		{"/infra/*.tf", "other/infra/main.tf", false},
		{"config/**", "config/prod/secrets.yml", true},
		{"config/**", "web/config/x.yml", false},
		{"**/auth", "web/internal/auth/x.go", true},
		// The file is cleaned; a bad pattern matches nothing.
		{"auth", "./internal//auth/x.go", true},
		{"[", "[", false},
		{"[[]id]", "app/[id]/page.tsx", true}, // a literal bracket; \ is refused
	} {
		if got := Match(c.pattern, c.file); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"[", "/", "./", " ", "", `app\auth`, "a**b", "a/../b", "a//b", "src/**x"} {
		if Valid(bad) == nil {
			t.Errorf("Valid(%q) passed", bad)
		}
	}
	for _, good := range []string{"auth*", "/db/migrate/", "**/auth", "src/**", " go.mod ", "docker-compose*.y*ml"} {
		if err := Valid(good); err != nil {
			t.Errorf("Valid(%q): %v", good, err)
		}
	}
}

func TestInAreas(t *testing.T) {
	t.Parallel()
	areas := []Area{{"auth", []string{"*auth*"}}, {"off", nil}, {"crypto", []string{"*cipher*", "*crypt*"}}}
	if a, g, ok := InAreas(areas, "internal/crypto/aes.go"); !ok || a != "crypto" || g != "*crypt*" {
		t.Fatalf("%s %s %v", a, g, ok)
	}
	if _, _, ok := InAreas(areas, "web/app.go"); ok {
		t.Fatal("matched nothing")
	}
}
