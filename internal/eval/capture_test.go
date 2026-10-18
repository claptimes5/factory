package eval

import "testing"

func TestCaseName(t *testing.T) {
	for in, want := range map[string]string{
		"Add an Add function!": "pr-1-add-an-add-function",
		"  ---  ":              "pr-1",
		"Fix ../../etc/passwd": "pr-1-fix-etc-passwd",
		"Ünïcode only: ✓":      "pr-1-n-code-only",
		"a very long title that goes on and on and on forever": "pr-1-a-very-long-title-that-goes-on-and-on-an",
	} {
		if got := CaseName("pr-1", in); got != want {
			t.Errorf("CaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClosedIssue(t *testing.T) {
	for body, want := range map[string]int{
		"Closes #12\n\nBuilt by the factory.": 12,
		"This fixes #3 and more":              3,
		"Resolved #40.":                       40,
		"See #5":                              0,
		"closes#5":                            0,
	} {
		if got := ClosedIssue(body); got != want {
			t.Errorf("ClosedIssue(%q) = %d, want %d", body, got, want)
		}
	}
}

func TestSafePaths(t *testing.T) {
	for p, ok := range map[string]bool{"a/b_test.go": true, "../x_test.go": false, "/etc/x_test.go": false, "a/../b_test.go": false, "..": false} {
		if safePath(p) != ok {
			t.Errorf("safePath(%q) = %v", p, !ok)
		}
	}
	for n, ok := range map[string]bool{"pr-1-x": true, "..": false, "a/b": false, "": false, "-x": false} {
		if safeName(n) != ok {
			t.Errorf("safeName(%q) = %v", n, !ok)
		}
	}
}
