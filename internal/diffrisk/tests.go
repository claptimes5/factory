package diffrisk

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/gates"
)

// A deleted test is a test file the change deletes, or a test the patch
// takes out more often than it puts one of that name back in the same
// directory: a test function or case, matched by its name, counted. So
// moving a test within its package, or renaming its file, isn't deleting
// it, but a generic name added elsewhere doesn't excuse one taken out.
// The diff guard already blocks a deleted test file; a test taken out of
// a file it keeps gets through the gates, and waits for a person here.
// A test that's skipped or disabled rather than taken out isn't found.

// testDecls find a test's name on one line of a test file, by language.
var testDecls = []struct {
	match func(file string) bool
	re    *regexp.Regexp
}{
	// Plain tests, and testify suites' methods.
	{ext(".go"), regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?((?:Test|Fuzz|Benchmark|Example)\w*)\s*\(`)},
	{ext(".py"), regexp.MustCompile(`^\s*(?:async\s+)?def\s+(test\w*)\s*\(`)},
	{ext(".rb"), regexp.MustCompile(`^\s*(?:it|specify|scenario|test)\s*\(?\s*(["'][^"']+["'])`)},
	{ext(".rb"), regexp.MustCompile(`^\s*def\s+(test_\w+)`)},
	{ext(".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".vue", ".svelte"), regexp.MustCompile("^\\s*(?:it|test)(?:\\.(?:only|concurrent|each\\([^)]*\\)))?\\s*\\(\\s*([\"'`][^\"'`]+[\"'`])")},
}

// rustTest is a Rust test's attribute, which lives beside the code it
// tests: #[test], #[tokio::test(flavor = "multi_thread")], #[rstest],
// #[test_case(1)] and the like.
var rustTest = regexp.MustCompile(`^\s*#\[\s*(?:\w+::)*(?:test|rstest|test_case|quickcheck|proptest|wasm_bindgen_test)\b`)

func ext(exts ...string) func(string) bool {
	return func(f string) bool {
		for _, e := range exts {
			if path.Ext(f) == e {
				return true
			}
		}
		return false
	}
}

// patchPath is the path a patch's ---/+++ line names: without git's a/
// or b/, the tab git adds after a name with a space, or the C quotes it
// puts around a name with a quote, a backslash or a control character.
func patchPath(s, prefix string) (string, bool) {
	if s == "/dev/null" {
		return "", false
	}
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	} else {
		s = strings.TrimSuffix(s, "\t")
	}
	return strings.TrimPrefix(s, prefix), true
}

// removedTests reads a patch for tests it takes out without putting back.
func removedTests(patch string) []Item {
	type key struct{ dir, ext, name string }
	net := map[key]int{}      // taken out, less put back
	where := map[key]string{} // the file one was taken out of
	rust := map[string]int{}  // test attributes taken out, less those put in, by directory
	rustAt := map[string]string{}
	file, header, gone := "", false, false // gone: a deleted file, which Rate names whole
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			header, gone = true, false
			continue
		case header && strings.HasPrefix(line, "--- "):
			if f, ok := patchPath(line[4:], "a/"); ok {
				file = f
			}
			continue
		case header && strings.HasPrefix(line, "+++ "):
			if f, ok := patchPath(line[4:], "b/"); ok {
				file = f
			} else {
				gone = true
			}
			continue
		case strings.HasPrefix(line, "@@"):
			header = false
			continue
		case header || gone || line == "" || (line[0] != '-' && line[0] != '+'):
			continue
		}
		minus, text := line[0] == '-', line[1:]
		step := 1
		if !minus {
			step = -1
		}
		if path.Ext(file) == ".rs" && rustTest.MatchString(text) {
			d := path.Dir(file)
			rust[d] += step
			if minus {
				rustAt[d] = file
			}
			continue
		}
		if !gates.IsTest(file) {
			continue
		}
		for _, d := range testDecls {
			if !d.match(file) {
				continue
			}
			if m := d.re.FindStringSubmatch(text); m != nil {
				k := key{path.Dir(file), path.Ext(file), m[1]}
				net[k] += step
				if minus {
					where[k] = file
				}
				break
			}
		}
	}
	var out []Item
	for k, n := range net {
		if n > 0 {
			out = append(out, Item{Name: strings.Trim(k.name, "\"'`"), File: where[k], Count: n})
		}
	}
	for d, n := range rust {
		if n > 0 {
			out = append(out, Item{Name: "#[test]", File: rustAt[d], Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Name < out[j].Name
	})
	return out
}
