package eval

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Planted bugs: a deterministic change to a line a merged PR added, which
// the reviewer must block with a finding at that line. Go only for now.

// Mutations are the bugs Plant knows, in the order capture tries them.
var Mutations = []string{"flip-comparison", "off-by-one", "drop-err-check", "drop-nil-check"}

// Plant is where a planted bug went.
type Plant struct {
	Mutation string `yaml:"mutation"`
	File     string `yaml:"file"`
	Line     int    `yaml:"line"` // 1-based, in the file as planted
}

// Plantable says whether Plant can put bugs in a file.
func Plantable(file string) bool {
	return path.Ext(file) == ".go" && !strings.HasSuffix(file, "_test.go")
}

var (
	errCheckRE = regexp.MustCompile(`^\s*if err != nil \{$`)
	nilCheckRE = regexp.MustCompile(`^\s*if [A-Za-z_][A-Za-z0-9_.]* == nil \{$`)
	// The comparison operators gofmt spaces, and what each flips to.
	flips = [][2]string{{" == ", " != "}, {" != ", " == "}, {" <= ", " > "}, {" >= ", " < "}, {" < ", " >= "}, {" > ", " <= "}}
)

// PlantBug applies mutation to the first line of src that added holds
// (the lines the PR added, 1-based) and the mutation fits. It returns the
// changed source and the line the bug is at: the changed line, or for a
// removed check the line that now stands where it was.
func PlantBug(src string, added map[int]bool, mutation string) (string, int, error) {
	if !slices.Contains(Mutations, mutation) {
		return "", 0, fmt.Errorf("no mutation %q", mutation)
	}
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if !added[i+1] {
			continue
		}
		code := strings.TrimSpace(l)
		if strings.HasPrefix(code, "//") {
			continue
		}
		switch mutation {
		case "flip-comparison":
			if !strings.HasPrefix(code, "if ") || errCheckRE.MatchString(l) {
				continue
			}
			for _, f := range flips {
				if j := strings.Index(l, f[0]); j >= 0 {
					lines[i] = l[:j] + f[1] + l[j+len(f[0]):]
					return strings.Join(lines, "\n"), i + 1, nil
				}
			}
		case "off-by-one":
			if !strings.HasPrefix(code, "for ") && !strings.HasPrefix(code, "if ") {
				continue
			}
			for _, f := range [][2]string{{" < len(", " <= len("}, {" <= len(", " < len("}} {
				if j := strings.Index(l, f[0]); j >= 0 {
					lines[i] = l[:j] + f[1] + l[j+len(f[0]):]
					return strings.Join(lines, "\n"), i + 1, nil
				}
			}
		case "drop-err-check", "drop-nil-check":
			re := errCheckRE
			if mutation == "drop-nil-check" {
				re = nilCheckRE
			}
			if !re.MatchString(l) {
				continue
			}
			indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
			for k := i + 1; k < len(lines); k++ {
				if lines[k] == indent+"}" {
					out := append(append([]string{}, lines[:i]...), lines[k+1:]...)
					return strings.Join(out, "\n"), i + 1, nil
				}
				if strings.TrimSpace(lines[k]) != "" && !strings.HasPrefix(lines[k], indent+"\t") && !strings.HasPrefix(lines[k], indent+" ") {
					break // not a block this simple shape covers
				}
			}
		}
	}
	return "", 0, fmt.Errorf("%s: nothing the change added fits", mutation)
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// AddedLines reads a unified diff for the lines it adds to each file, by
// their line numbers in the new file.
func AddedLines(diff string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	var file string
	n := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "):
		case strings.HasPrefix(l, "@@"):
			if m := hunkRE.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case file == "":
		case strings.HasPrefix(l, "+"):
			if out[file] == nil {
				out[file] = map[int]bool{}
			}
			out[file][n] = true
			n++
		case strings.HasPrefix(l, " "):
			n++
		}
	}
	return out
}
