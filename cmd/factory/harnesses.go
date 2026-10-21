package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/claptimes5/factory/internal/config"
)

// Claude and Codex are equals: either one is enough to run the factory,
// and with both, the one connected first builds and the other reviews what
// it builds, since a model of another vendor catches more of its blind
// spots. Setup routes jobs to match whenever a harness is connected or
// dropped, keeping how hard the factory works.

// harnessesIn are the harnesses vals, the env file's values, give a key or
// sign-in, Claude first.
func harnessesIn(vals map[string]string) []string {
	var out []string
	if vals["ANTHROPIC_API_KEY"] != "" || vals["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		out = append(out, "claude")
	}
	if vals["OPENAI_API_KEY"] != "" || vals["CODEX_API_KEY"] != "" || vals["CODEX_ACCESS_TOKEN"] != "" || vals["FACTORY_CODEX_AUTH"] == codexChatGPT {
		out = append(out, "codex")
	}
	return out
}

// harnessName is how a harness is called to people.
func harnessName(h string) string {
	return map[string]string{"claude": "Claude", "codex": "Codex"}[h]
}

// routeHarnesses makes the config at path route to after, the harnesses
// connected now (before, those connected until now): one does everything;
// with two, the one connected first builds and the other reviews. It keeps
// the preset (Balanced when set by hand) and says what it changed, or ""
// when the routing already fits.
func routeHarnesses(path string, before, after []string) (string, error) {
	if len(after) == 0 {
		return "", nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	c, err := config.Parse(src)
	if err != nil {
		return "", err
	}
	builder, reviewer := c.Harnesses()
	fits := true
	for _, r := range c.Routing {
		fits = fits && slices.Contains(after, r.Harness)
	}
	switch {
	case len(after) == 1:
		builder, reviewer = after[0], after[0]
	case len(before) == 1 && !slices.Contains(before, other(before[0])):
		// The second one arrived: the first builds, it reviews.
		builder, reviewer = before[0], other(before[0])
	case fits && builder != reviewer:
		return "", nil
	default:
		builder, reviewer = after[0], after[1]
	}
	if cb, cr := c.Harnesses(); fits && cb == builder && cr == reviewer {
		return "", nil
	}
	preset, _ := config.PresetNow(src)
	if preset == "" {
		preset = "balanced"
	}
	changes, err := c.PresetChanges(preset, builder, reviewer)
	if err != nil {
		return "", err
	}
	out, err := config.Edit(src, changes)
	if err != nil {
		return "", err
	}
	if err := config.WriteFile(path, out); err != nil {
		return "", err
	}
	if builder == reviewer {
		return fmt.Sprintf("%s now builds and reviews.", harnessName(builder)), nil
	}
	return fmt.Sprintf("%s now builds, and %s reviews what it builds.", harnessName(builder), harnessName(reviewer)), nil
}

// other is the harness that isn't h.
func other(h string) string {
	if h == "claude" {
		return "codex"
	}
	return "claude"
}
