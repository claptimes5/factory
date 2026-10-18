package config

import (
	"os"
	"strings"
	"testing"
)

func TestConfirmOver(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	y := string(b)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets.ConfirmOver != 0 || c.ConfirmOver("claptimes5/factory") != 0 {
		t.Fatal("off unless set")
	}
	set := func(budgets, repos string) string {
		out := strings.Replace(y, "  awaiting_human_max: 5\n", "  awaiting_human_max: 5\n"+budgets, 1)
		return strings.Replace(out, "repo: claptimes5/factory", repos, 1)
	}
	c, err = Parse([]byte(set("  confirm_over: 5\n", "repos: [o/a, {name: o/b, confirm_over: 20}, {name: o/c, confirm_over: 0}]")))
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfirmOver("o/a") != 5 || c.ConfirmOver("O/B") != 20 || c.ConfirmOver("o/c") != 0 || c.ConfirmOver("o/gone") != 5 {
		t.Fatalf("by repo: %v %v %v", c.ConfirmOver("o/a"), c.ConfirmOver("o/b"), c.ConfirmOver("o/c"))
	}
	for name, bad := range map[string]string{
		"negative":      set("  confirm_over: -1\n", "repo: o/a"),
		"negative repo": set("", "repos: [{name: o/a, confirm_over: -2}]"),
	} {
		if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "confirm_over") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
