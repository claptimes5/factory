package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/store"
)

func TestParseCoverage(t *testing.T) {
	out := "ok  \tfixture/calc\t0.002s\tcoverage: 75.0% of statements\n" +
		"\tfixture/empty\t\tcoverage: 0.0% of statements\n" +
		"FAIL\tfixture/bad\t0.01s\n" +
		"?   \tfixture/none\t[no test files]\n" +
		"web/src/util 92.5%\n"
	got := parseCoverage(out)
	want := map[string]float64{"fixture/calc": 75, "fixture/empty": 0, "web/src/util": 92.5}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// Code added without tests lowers its package's coverage and fails the
// gate; the same code with a test passes.
func TestCoverageFloor(t *testing.T) {
	seed := map[string]string{
		"calc/calc.go":      "package calc\n\nfunc Double(x int) int { return 2 * x }\n",
		"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal()\n\t}\n}\n",
	}
	untested := func(work string) error {
		if err := writeFile(work, "calc/calc.go", "package calc\n\nfunc Double(x int) int { return 2 * x }\n\nfunc Half(x int) int {\n\tif x < 0 {\n\t\treturn -(-x / 2)\n\t}\n\treturn x / 2\n}\n"); err != nil {
			return err
		}
		return goodBuild(work)
	}
	tested := func(work string) error {
		if err := untested(work); err != nil {
			return err
		}
		writeFile(work, "calc/half_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestHalf(t *testing.T) {\n\tif Half(4) != 2 || Half(-4) != -2 {\n\t\tt.Fatal()\n\t}\n}\n")
		return commitIn(work, "test Half")
	}
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		build func(string) error
		fail  bool
	}{{"untested", untested, true}, {"tested", tested, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := setupWith(t, seed)
			e.p.Cfg.Gates.Coverage = "go test -count=1 -cover {pkgs}"
			e.addIssue(1)
			e.claude.builds = []func(string) error{c.build}
			e.codex.reviews = []string{pass}
			e.p.Drain(ctx)
			jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 1).ID, Kind: "implement"})
			if len(jobs) == 0 {
				t.Fatal("no implement job")
			}
			last := jobs[len(jobs)-1]
			if got := strings.Contains(last.Error, "coverage=fail"); got != c.fail {
				t.Fatalf("coverage failed = %v, want %v; error: %s", got, c.fail, last.Error)
			}
			if c.fail && !strings.Contains(last.Error, "fixture/calc: 100.0% → 25.0%") {
				t.Fatalf("error doesn't say what fell: %s", last.Error)
			}
		})
	}
}
