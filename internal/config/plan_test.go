package config

import (
	"strings"
	"testing"
)

func TestPlanFor(t *testing.T) {
	t.Parallel()
	b, _ := exampleYAML()
	parse := func(plan string) (*Config, error) { return Parse(append(append([]byte{}, b...), []byte("\n"+plan)...)) }
	c, err := parse("")
	if err != nil {
		t.Fatal(err)
	}
	// By default an L plan waits for a human, and nothing smaller plans.
	for size, want := range map[string]string{"L": PlanHuman, "M": "", "S": "", "XS": "", "": ""} {
		if got := c.PlanFor(size); got != want {
			t.Errorf("default %q: %q, want %q", size, got, want)
		}
	}
	if c.Timeout("plan").Minutes() != 15 {
		t.Errorf("plan timeout %v", c.Timeout("plan"))
	}
	c, err = parse("plan: {sizes: [M, L], medium: critic}")
	if err != nil {
		t.Fatal(err)
	}
	if c.PlanFor("M") != PlanHuman {
		t.Error("a human's size wins over the critic")
	}
	c, _ = parse("plan: {medium: critic}")
	if c.PlanFor("M") != PlanCritic || c.PlanFor("L") != PlanHuman {
		t.Error("medium: critic")
	}
	c, _ = parse("plan: {enabled: false}")
	if c.PlanFor("L") != "" {
		t.Error("off")
	}
	for bad, want := range map[string]string{"plan: {sizes: [XL]}": "plan.sizes", "plan: {medium: human}": "plan.medium"} {
		if _, err := parse(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
