package config

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/pathglob"
)

func areaNames(as []pathglob.Area) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

func TestSensitiveDefaults(t *testing.T) {
	t.Parallel()
	c := load(t)
	want := []string{"auth", "crypto", "secrets", "payments", "access", "input", "exec", "sql", "migrations", "ci", "dependencies", "infra"}
	if got := areaNames(c.SensitiveAreas()); !slices.Equal(got, want) {
		t.Fatalf("areas %v", got)
	}
	// What risk's and the security pass's own defaults had is all here.
	for _, g := range []string{"auth", "*auth*", "*jwt*", "sso*", "devise*", "*crypt*", "*token*", "*payment*", "*billing*", "*wallet*", "migrations", "migrate/", "migration/", "alembic/", "go.mod", "*.tf"} {
		if !slices.ContainsFunc(c.SensitiveAreas(), func(a pathglob.Area) bool { return slices.Contains(a.Globs, g) }) {
			t.Errorf("no default area has %s", g)
		}
	}
	if got := areaNames(c.SensitiveAreas("payments", "auth")); !slices.Equal(got, []string{"auth", "payments"}) {
		t.Fatalf("named areas, in order: %v", got)
	}
}

// Setting an area replaces its defaults, [] turns it off, and an area of
// another name is added. Risk and the security pass both follow it.
func TestSensitiveOverride(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	set := func(y string) (*Config, error) {
		return ParseIn([]byte(strings.Replace(string(b), "\nrouting:", "\nsensitive: "+y+"\nrouting:", 1)), "../..")
	}
	c, err := set("{crypto: [], payments: [internal/ledger/], ledger_ui: ['*ledger*']}")
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"internal/crypto/aes.go":      "",                           // off
		"internal/ledger/post.go":     "payments: internal/ledger/", // replaced
		"internal/payments/charge.go": "access: *payment*",          // no longer payments, still a pass
		"web/LedgerView.tsx":          "ledger_ui: *ledger*",        // added
		"internal/auth/a.go":          "auth: auth",                 // the rest keep theirs
	} {
		if g, ok := c.SecurityMatch(file); g != want || ok != (want != "") {
			t.Errorf("%s: %q, want %q", file, g, want)
		}
	}
	r := c.RiskRules()
	if !slices.Equal(areaNames(r.Auth), authAreas) || len(r.Auth[1].Globs) != 0 || !slices.Equal(r.Auth[3].Globs, []string{"internal/ledger/"}) {
		t.Fatalf("risk's auth areas %+v", r.Auth)
	}
	for _, bad := range []struct{ y, err string }{
		{"{auth: ['[']}", `sensitive.auth: glob "["`},
		{"{payments: [a/../b]}", "sensitive.payments"},
		{"{Big Area: [x]}", `sensitive: "Big Area" isn't an area's name`},
	} {
		if _, err := set(bad.y); err == nil || !strings.Contains(err.Error(), bad.err) {
			t.Errorf("%s: %v", bad.y, err)
		}
	}
}

// An area left empty or null keeps its defaults: only [] turns one off,
// so a half-written line can't open a hole.
func TestSensitiveNullKeepsDefaults(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	set := func(y string) *Config {
		c, err := ParseIn([]byte(strings.Replace(string(b), "\nrouting:", "\nsensitive: "+y+"\nrouting:", 1)), "../..")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, y := range []string{"{auth: }", "{auth: ~}", "{auth: null}"} {
		if _, g, ok := pathglob.InAreas(set(y).RiskRules().Auth, "internal/auth/a.go"); !ok || g != "auth" {
			t.Errorf("%s turned auth off", y)
		}
	}
	if _, _, ok := pathglob.InAreas(set("{auth: []}").RiskRules().Auth, "internal/auth/a.go"); ok {
		t.Error("auth: [] left auth on")
	}
}

// What security and risk are given themselves is guarded from REVIEW.md's
// skip as the sensitive list is.
func TestSkipGuardAreas(t *testing.T) {
	t.Parallel()
	c := Config{Security: Security{Paths: []string{"keys/"}, ExtraPaths: []string{"ledger/"}}, Risk: Risk{AuthPaths: []string{"gate*"}, MigrationPaths: []string{"schema/"}}}
	for _, f := range []string{"keys/a.go", "ledger/b.go", "app/gatekeeper.go", "schema/1.go", "internal/auth/a.go"} {
		if _, _, ok := pathglob.InAreas(c.SkipGuardAreas(), f); !ok {
			t.Errorf("%s isn't guarded", f)
		}
	}
}

// risk.auth_paths and migration_paths, when set, add to the sensitive
// list's areas.
func TestRiskPathsAddToSensitive(t *testing.T) {
	t.Parallel()
	c := Config{Risk: Risk{AuthPaths: []string{"gatekeeper*"}, MigrationPaths: []string{"db/schema/"}}}
	r := c.RiskRules()
	if a, g, ok := pathglob.InAreas(r.Auth, "app/gatekeeper.go"); !ok || a != "auth" || g != "gatekeeper*" {
		t.Fatalf("%s %s %v", a, g, ok)
	}
	if _, _, ok := pathglob.InAreas(r.Auth, "internal/crypto/aes.go"); !ok {
		t.Fatal("auth_paths replaced the sensitive list")
	}
	if _, _, ok := pathglob.InAreas(r.Migrations, "db/schema/1.sql"); !ok {
		t.Fatal("migration_paths")
	}
}
