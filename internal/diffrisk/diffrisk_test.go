package diffrisk

import (
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/pathglob"
)

var rules = Rules{
	MediumLines: 150, HighLines: 300, MediumFiles: 10, HighFiles: 25,
	HighPaths: []string{"deploy/"}, MediumPaths: []string{"Dockerfile"},
	Auth:        []pathglob.Area{{Name: "auth", Globs: []string{"auth", "auth.*", "session*", "login*"}}, {Name: "crypto", Globs: []string{"*crypt*"}}},
	Migrations:  []pathglob.Area{{Name: "migrations", Globs: []string{"migrations/", "migrate/"}}},
	NeedsPerson: Kinds,
}

// change is a Change from files at the base and the head (absent where
// a map has no entry).
func change(patch string, files []File, before, after map[string]string) Change {
	read := func(m map[string]string) func(string) (string, bool, error) {
		return func(p string) (string, bool, error) {
			b, ok := m[p]
			return b, ok, nil
		}
	}
	return Change{Files: files, Patch: patch, Before: read(before), After: read(after)}
}

func reason(a Assessment, kind string) *Reason {
	for i := range a.Reasons {
		if a.Reasons[i].Kind == kind {
			return &a.Reasons[i]
		}
	}
	return nil
}

// manifest is a change to one manifest, from before to after ("" for
// absent).
func manifest(name, before, after string) Change {
	b, a := map[string]string{}, map[string]string{name: after}
	status := "A"
	if before != "" {
		b[name], status = before, "M"
	}
	return change("", []File{{Path: name, Status: status, Added: 1}}, b, a)
}

func TestRate(t *testing.T) {
	t.Parallel()
	many := func(n int) []File {
		var fs []File
		for i := range n {
			fs = append(fs, File{Path: "pkg/f" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".go", Status: "M", Added: 1})
		}
		return fs
	}
	for _, c := range []struct {
		name   string
		ch     Change
		risk   string
		kind   string
		text   string
		person bool
	}{
		{"a small change", change("", []File{{Path: "add.go", Status: "M", Added: 3, Deleted: 1}}, nil, nil), "low", "", "", false},
		{"medium by lines", change("", []File{{Path: "add.go", Status: "M", Added: 100, Deleted: 60}}, nil, nil), "medium", KindLines, "changes 160 lines (risk.medium_lines is 150)", false},
		{"high by lines", change("", []File{{Path: "add.go", Status: "M", Added: 300}}, nil, nil), "high", KindLines, "changes 300 lines (risk.high_lines is 300)", false},
		{"binaries count as the diff guard counts them", change("", []File{{Path: "a.png", Status: "A", Binary: true}, {Path: "b.png", Status: "A", Binary: true}}, nil, nil), "medium", KindLines, "changes 200 lines", false},
		{"lockfiles don't count", change("", []File{{Path: "go.sum", Status: "M", Added: 900}, {Path: "web/package-lock.json", Status: "M", Added: 900}}, nil, map[string]string{"web/package-lock.json": `{"packages":{}}`}), "low", "", "", false},
		{"medium by files", change("", many(12), nil, nil), "medium", KindFiles, "changes 12 files (risk.medium_files is 10)", false},
		{"high by files", change("", many(30), nil, nil), "high", KindFiles, "changes 30 files", false},
		{"a high path", change("", []File{{Path: "deploy/prod.yaml", Status: "M", Added: 1}}, nil, nil), "high", KindPaths, "touches a path in risk.high_paths: `deploy/prod.yaml`", false},
		{"a medium path", change("", []File{{Path: "web/Dockerfile", Status: "M", Added: 1}}, nil, nil), "medium", KindPaths, "risk.medium_paths: `web/Dockerfile`", false},
		{"a new migration", change("", []File{{Path: "db/migrate/2_add_users.rb", Status: "A", Added: 9}}, nil, nil), "high", Migrations, "adds or changes a migration: `db/migrate/2_add_users.rb`", true},
		{"a migration folder's readme isn't one", change("", []File{{Path: "db/migrate/README.md", Status: "M", Added: 1}}, nil, nil), "low", "", "", false},
		{"auth code", change("", []File{{Path: "internal/auth/token.go", Status: "M", Added: 2}}, nil, nil), "high", Auth, "changes auth code: `auth` in `internal/auth/token.go`", true},
		{"crypto code counts as auth", change("", []File{{Path: "internal/crypto/aes.go", Status: "M", Added: 2}}, nil, nil), "high", Auth, "changes crypto code: `*crypt*` in `internal/crypto/aes.go`", true},
		{"auth by file name", change("", []File{{Path: "app/controllers/sessions_controller.rb", Status: "M", Added: 2}}, nil, nil), "high", Auth, "sessions_controller.rb", true},
		{"an auth test isn't auth code", change("", []File{{Path: "internal/auth/token_test.go", Status: "M", Added: 2}}, nil, nil), "low", "", "", false},
		{"auth docs aren't auth code", change("", []File{{Path: "docs/auth.md", Status: "M"}, {Path: "web/login.png", Status: "M"}}, nil, nil), "low", "", "", false},
		{"vendored auth isn't ours", change("", []File{{Path: "vendor/x/auth/a.go", Status: "M"}, {Path: "web/node_modules/oauth/i.js", Status: "M"}}, nil, nil), "low", "", "", false},
		{"a deleted test file", change("", []File{{Path: "add_test.go", Status: "D", Deleted: 9}}, nil, nil), "medium", DeletedTests, "deletes tests: `add_test.go`", true},
		{"a test function taken out", change("diff --git a/add_test.go b/add_test.go\n--- a/add_test.go\n+++ b/add_test.go\n@@ -1,4 +1,0 @@\n-func TestAdd(t *testing.T) {\n-\tt.Fatal()\n-}\n",
			[]File{{Path: "add_test.go", Status: "M", Deleted: 3}}, nil, nil), "medium", DeletedTests, "deletes tests: `TestAdd` in `add_test.go`", true},
		{"a testify suite method taken out", change("diff --git a/s_test.go b/s_test.go\n--- a/s_test.go\n+++ b/s_test.go\n@@ -1 +0,0 @@\n-func (s *AuthSuite) TestLogin() {\n",
			[]File{{Path: "s_test.go", Status: "M"}}, nil, nil), "medium", DeletedTests, "`TestLogin` in `s_test.go`", true},
		{"a test moved within its package isn't deleted", change("diff --git a/a_test.go b/a_test.go\n--- a/a_test.go\n+++ b/a_test.go\n@@ -1 +0,0 @@\n-func TestAdd(t *testing.T) {}\ndiff --git a/b_test.go b/b_test.go\n--- a/b_test.go\n+++ b/b_test.go\n@@ -0,0 +1 @@\n+func TestAdd(t *testing.T) {}\n",
			[]File{{Path: "a_test.go", Status: "M"}, {Path: "b_test.go", Status: "M"}}, nil, nil), "low", "", "", false},
		{"a name put back elsewhere doesn't excuse it", change("diff --git a/api/x.test.ts b/api/x.test.ts\n--- a/api/x.test.ts\n+++ b/api/x.test.ts\n@@ -3 +2,0 @@\n-  it('works', () => {\ndiff --git a/web/y.test.ts b/web/y.test.ts\n--- a/web/y.test.ts\n+++ b/web/y.test.ts\n@@ -0,0 +1 @@\n+  it('works', () => {\n",
			[]File{{Path: "api/x.test.ts", Status: "M"}, {Path: "web/y.test.ts", Status: "M"}}, nil, nil), "medium", DeletedTests, "`works` in `api/x.test.ts`", true},
		{"two of a name, one put back", change("diff --git a/x_test.py b/x_test.py\n--- a/x_test.py\n+++ b/x_test.py\n@@ -1,2 +1,1 @@\n-    def test_ok(self):\n-    def test_ok(self):\n+    def test_ok(self):\n",
			[]File{{Path: "x_test.py", Status: "M"}}, nil, nil), "medium", DeletedTests, "deletes tests: `test_ok` in `x_test.py`", true},
		{"a JS case taken out", change("diff --git a/x.test.ts b/x.test.ts\n--- a/x.test.ts\n+++ b/x.test.ts\n@@ -3 +2,0 @@\n-  it('rejects a bad token', () => {\n",
			[]File{{Path: "x.test.ts", Status: "M"}}, nil, nil), "medium", DeletedTests, "`rejects a bad token` in `x.test.ts`", true},
		{"a Python test taken out", change("diff --git a/tests/test_x.py b/tests/test_x.py\n--- a/tests/test_x.py\n+++ b/tests/test_x.py\n@@ -3 +2,0 @@\n-def test_login():\n",
			[]File{{Path: "tests/test_x.py", Status: "M"}}, nil, nil), "medium", DeletedTests, "`test_login`", true},
		{"Rust tests taken out", change("diff --git a/src/lib.rs b/src/lib.rs\n--- a/src/lib.rs\n+++ b/src/lib.rs\n@@ -3,2 +2,0 @@\n-    #[test]\n-    fn adds() {}\n-    #[tokio::test(flavor = \"multi_thread\")]\n-    #[rstest]\n-    #[test_case(1)]\n",
			[]File{{Path: "src/lib.rs", Status: "M"}}, nil, nil), "medium", DeletedTests, "4 × `#[test]` in `src/lib.rs`", true},
		{"a path with a space or a quote", change("diff --git a/my tests/a_test.go b/my tests/a_test.go\n--- a/my tests/a_test.go\t\n+++ b/my tests/a_test.go\t\n@@ -1 +0,0 @@\n-func TestA(t *testing.T) {}\n"+
			"diff --git \"a/q\\\"x_test.go\" \"b/q\\\"x_test.go\"\n--- \"a/q\\\"x_test.go\"\n+++ \"b/q\\\"x_test.go\"\n@@ -1 +0,0 @@\n-func TestQ(t *testing.T) {}\n",
			[]File{{Path: "my tests/a_test.go", Status: "M"}, {Path: `q"x_test.go`, Status: "M"}}, nil, nil), "medium", DeletedTests, "`TestA` in `my tests/a_test.go`, `TestQ` in `q\"x_test.go`", true},
		{"a removed line that only looks like a test", change("diff --git a/add.go b/add.go\n--- a/add.go\n+++ b/add.go\n@@ -1 +0,0 @@\n-func TestHelper() {}\n",
			[]File{{Path: "add.go", Status: "M"}}, nil, nil), "low", "", "", false},
		{"a manifest the factory doesn't read", change("", []File{{Path: "api/composer.json", Status: "M"}, {Path: "App.csproj", Status: "A"}}, nil, nil), "medium", NewDependencies,
			"changes a dependency manifest the factory doesn't read, which counts as a new dependency: `App.csproj`, `api/composer.json`", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := Rate(c.ch, rules)
			if a.Risk != c.risk || len(a.Unread) > 0 {
				t.Fatalf("risk %q (unread %+v), want %q: %+v", a.Risk, a.Unread, c.risk, a.Reasons)
			}
			if c.kind == "" {
				if len(a.Reasons) > 0 {
					t.Fatalf("reasons for an ordinary change: %+v", a.Reasons)
				}
				return
			}
			r := reason(a, c.kind)
			if r == nil || !strings.Contains(r.Text(Code), c.text) || r.Risk != c.risk || r.NeedsPerson != c.person {
				t.Fatalf("reason %s = %q, want %q", c.kind, r.Text(Code), c.text)
			}
			if got := len(a.NeedsPerson()) > 0; got != c.person {
				t.Fatalf("needs a person = %v, want %v", got, c.person)
			}
		})
	}
}

// Each ecosystem: a new name counts, a version bump doesn't, and a
// change of where a package comes from does.
func TestNewDependencies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		file          string
		before, after string
		want          string // "" for none
	}{
		{"go: new module", "go.mod", "module x\n\nrequire (\n\tgolang.org/x/term v0.1.0\n)\n", "module x\n\nrequire (\n\tgolang.org/x/term v0.2.0\n\tgithub.com/evil/pkg v1.0.0 // indirect\n)\n", "`github.com/evil/pkg` in `go.mod`"},
		{"go: a bump", "go.mod", "module x\n\nrequire golang.org/x/term v0.1.0\n", "module x\n\nrequire golang.org/x/term v0.2.0\n", ""},
		{"go: require( with no space", "go.mod", "module x\n", "module x\n\nrequire(\n\tgithub.com/a/b v1.0.0\n)\n", "`github.com/a/b`"},
		{"go: a replace", "go.mod", "module x\n\nrequire golang.org/x/term v0.1.0\n", "module x\n\nrequire golang.org/x/term v0.1.0\n\nreplace golang.org/x/term => github.com/evil/term v0.1.0\n", "`golang.org/x/term from replace github.com/evil/term v0.1.0`"},
		{"go: a replace's target changed", "go.mod", "module x\nreplace (\n\ta => ../a\n)\n", "module x\nreplace (\n\ta => ../b\n)\n", "`a from replace ../b`"},
		{"npm: a new package", "package.json", `{"dependencies":{"react":"19"}}`, `{"dependencies":{"react":"19"},"devDependencies":{"left-pad":"1"}}`, "`left-pad` in `package.json`"},
		{"npm: a bump", "package.json", `{"dependencies":{"react":"^18"}}`, `{"dependencies":{"react":"^19.1"}}`, ""},
		{"npm: an alias swap", "package.json", `{"dependencies":{"left-pad":"^1"}}`, `{"dependencies":{"left-pad":"npm:evil-pad@1"}}`, "`left-pad from npm:evil-pad`"},
		{"npm: a git source", "package.json", `{"dependencies":{"x":"1"}}`, `{"dependencies":{"x":"github:evil/x#main"}}`, "`x from github:evil/x#main`"},
		{"npm: overrides and resolutions", "package.json", `{}`, `{"overrides":{"a":{"b":"npm:c@1"}},"resolutions":{"**/d":"2"},"bundleDependencies":["e"]}`, "`override a > b from spec npm:c@1`"},
		{"npm lock v1, nested", "package-lock.json", `{"dependencies":{"a":{}}}`, `{"dependencies":{"a":{"dependencies":{"b":{"resolved":"https://registry.npmjs.org/b/-/b-1.tgz"}}}}}`, "`b` in `package-lock.json`"},
		{"npm lock: another registry", "package-lock.json", `{"packages":{"node_modules/a":{"resolved":"https://registry.npmjs.org/a/-/a-1.tgz"}}}`, `{"packages":{"node_modules/a":{"resolved":"https://evil.example/a/-/a-1.tgz"}}}`, "`a from registry evil.example`"},
		{"yarn berry: a bump", "yarn.lock", "\"lodash@npm:^4.0.0\":\n  version: 4.17.21\n", "\"lodash@npm:^4.1.0\":\n  version: 4.17.22\n", ""},
		{"pip: names, direct refs and indexes", "requirements/base.in", "requests\n", "Requests[socks]>=2.0  # http\nPy_YAML==6\nmylib @ https://evil.example/mylib.whl\n--extra-index-url https://evil.example/simple\ngit+https://evil.example/x.git#egg=x\n",
			"`mylib from https://evil.example/mylib.whl` in `requirements/base.in`, `pip option from --extra-index-url https://evil.example/simple`, `pip option from git+https://evil.example/x.git#egg=x`, `py-yaml`"},
		{"pip: constraints", "constraints-dev.txt", "", "urllib3<2\n", "`urllib3`"},
		{"pyproject", "pyproject.toml", "[project]\ndependencies = [\"httpx>=0.27\"]\n",
			"[build-system]\nrequires = [\"hatchling\"]\n[project]\nname = \"x\"\ndependencies = [\n  \"httpx>=0.28\",\n  \"rich[jupyter]\", # pretty\n]\noptional-dependencies = { dev = [\"pytest\"] }\n[tool.uv]\ndev-dependencies = [\"ruff\"]\n[tool.poetry.dependencies]\npython = \"^3.12\"\nflask = \"*\"\n[tool.poetry.dependencies.evil]\ngit = \"https://evil.example/e.git\"\n",
			"`evil from git https://evil.example/e.git`, `flask`, `hatchling`, `pytest`, `rich`, `ruff`"},
		{"uv.lock: a virtual-looking registry is still a registry", "uv.lock", "",
			"[[package]]\nname = \"me\"\nsource = { virtual = \".\" }\n\n[[package]]\nname = \"httpx\"\nsource = { registry = \"https://art.example/api/pypi/pypi-virtual/simple\" }\n",
			"`httpx from registry https://art.example/api/pypi/pypi-virtual/simple` in `uv.lock`"},
		{"Cargo", "Cargo.toml", "[package]\nname = \"me\"\n[dependencies]\nserde = \"1\"\n",
			"[package]\nname = \"me\"\n[dependencies]\nserde = \"1.0.200\"\ntokio = { version = \"1\", features = [\"full\"] }\n[dev-dependencies.proptest]\nversion = \"1\"\ngit = \"https://evil.example/p\"\n[patch.crates-io]\nserde = { git = \"https://evil.example/serde\" }\n",
			"`proptest from git https://evil.example/p`, `serde from patch crates-io git https://evil.example/serde`, `tokio`"},
		{"Cargo.lock", "Cargo.lock", "[[package]]\nname = \"me\"\n\n[[package]]\nname = \"serde\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n",
			"[[package]]\nname = \"me\"\nversion = \"0.2.0\"\n\n[[package]]\nname = \"other-member\"\n\n[[package]]\nname = \"serde\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n\n[[package]]\nname = \"mio\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n",
			"`mio` in `Cargo.lock`"},
		{"Ruby", "Gemfile", "gem 'rails'\n", "source 'https://evil.example'\ngem 'rails', git: 'https://evil.example/rails'\ngem \"devise\", \"~> 4\"\n", "`devise`, `gem source from https://evil.example`, `rails from git https://evil.example/rails`"},
		{"Gemfile.lock", "Gemfile.lock", "GEM\n  remote: https://rubygems.org/\n  specs:\n    rails (7.1)\n", "GEM\n  remote: https://rubygems.org/\n  specs:\n    devise (4.9)\n      warden (~> 1.2)\n    rails (7.1)\n    warden (1.2.9)\n", "`devise` in `Gemfile.lock`, `warden` in `Gemfile.lock`"},
		{"pnpm", "pnpm-lock.yaml", "lockfileVersion: '9.0'\npackages:\n  lodash@4.17.21:\n    resolution: {}\n",
			"lockfileVersion: '9.0'\npackages:\n  lodash@4.17.22:\n    resolution: {}\n  '@scope/x@1.0.0(react@19)':\n    resolution: {}\n", "`@scope/x` in `pnpm-lock.yaml`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := Rate(manifest(c.file, c.before, c.after), rules)
			if len(a.Unread) > 0 {
				t.Fatalf("unread: %+v", a.Unread)
			}
			r := reason(a, NewDependencies)
			if c.want == "" {
				if r != nil {
					t.Fatalf("a bump counted: %s", r.Text(Code))
				}
				return
			}
			if r == nil || !r.NeedsPerson {
				t.Fatalf("got %+v, want %s", r, c.want)
			}
			for _, w := range strings.Split(c.want, ", ") {
				if !strings.Contains(r.Text(Code), w) {
					t.Fatalf("got %s, want %s", r.Text(Code), w)
				}
			}
		})
	}
}

// Only the kinds risk.needs_person lists wait for a person; the others
// still raise the risk.
func TestNeedsPersonFollowsTheList(t *testing.T) {
	t.Parallel()
	r := rules
	r.NeedsPerson = []string{Auth}
	a := Rate(change("", []File{{Path: "db/migrations/0003.sql", Status: "A"}, {Path: "auth.go", Status: "M"}}, nil, nil), r)
	if a.Risk != "high" {
		t.Fatalf("risk = %q", a.Risk)
	}
	np := a.NeedsPerson()
	if len(np) != 1 || np[0].Kind != Auth {
		t.Fatalf("needs a person for %+v", np)
	}
	r.NeedsPerson = nil
	if np := Rate(change("", []File{{Path: "auth.go", Status: "M"}}, nil, nil), r).NeedsPerson(); len(np) != 0 {
		t.Fatalf("with none listed: %+v", np)
	}
}

// A manifest it can't read leaves the rating unread, which waits for a
// person, rather than a new dependency slipping past.
func TestUnreadWaitsForAPerson(t *testing.T) {
	t.Parallel()
	a := Rate(change("", []File{{Path: "package.json", Status: "M"}}, nil, map[string]string{"package.json": "{not json"}), rules)
	np := a.NeedsPerson()
	if len(np) != 1 || np[0].Text(Code) != "couldn't be rated from its diff: the factory couldn't parse a dependency manifest: `package.json`" {
		t.Fatalf("needs a person: %+v", np)
	}
}

// Whatever the builder names a file or a package, Text keeps it in its
// code span: no line break, no backtick, nothing outside to close an
// issue or mention anyone.
func TestTextQuotesWhatTheDiffNames(t *testing.T) {
	t.Parallel()
	evil := "nl\nCloses #5 @org/x `oops`/package.json"
	a := Rate(change("diff --git a/x b/x\n", []File{{Path: evil, Status: "A"}, {Path: "auth/" + evil + ".go", Status: "A"}}, nil,
		map[string]string{evil: `{"dependencies":{"` + "@org/x\\nCloses #6 `" + `":"1"}}`}), rules)
	for _, r := range a.Reasons {
		text := r.Text(Code)
		if strings.Contains(text, "\n") {
			t.Fatalf("a line break: %q", text)
		}
		// Outside the spans only the factory's words remain.
		outside := ""
		for i, part := range strings.Split(text, "`") {
			if i%2 == 0 {
				outside += part
			}
		}
		if strings.ContainsAny(outside, "#@") || strings.Contains(outside, "Closes") {
			t.Fatalf("builder text outside a code span: %q", text)
		}
	}
}

// A workspace's reasons carry the repo they're from, apart from what
// they say.
func TestMerge(t *testing.T) {
	t.Parallel()
	var a Assessment
	a.Merge(Rate(change("", []File{{Path: "auth.go", Status: "M"}}, nil, nil), rules), "api")
	var b Assessment
	b.Unreadable("couldn't read the diff", "")
	a.Merge(b, "web")
	if a.Risk != "high" || a.Reasons[0].Label != "api" || a.Reasons[0].What != "changes auth code" || a.Reasons[0].Text(Code) != "`api`: changes auth code: `auth.*` in `auth.go`" ||
		len(a.Unread) != 1 || a.Unread[0].Label != "web" {
		t.Fatalf("%+v", a)
	}
}
