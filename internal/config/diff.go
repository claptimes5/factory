package config

import (
	"fmt"
	"strings"
)

// UnifiedDiff is the change from a to b as a unified diff of name, with
// three lines of context, or "" when they are the same. It is for showing
// a proposed edit to factory.yaml, a file of a few hundred lines, so it
// compares every line with every other.
func UnifiedDiff(name string, a, b []byte) string {
	if string(a) == string(b) {
		return ""
	}
	x, y := lines(a), lines(b)
	// lcs[i][j] is the longest common run of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte // ' ', '-' or '+'
		text string
		i, j int // line numbers before it, in a and in b
	}
	var ops []op
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			ops = append(ops, op{' ', x[i], i, j})
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', x[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', y[j], i, j})
			j++
		}
	}
	const context = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		// A hunk runs from context lines before this change to context
		// lines after the last change that is no further than 2*context away.
		start := max(k-context, 0)
		end := k
		for n := k; n < len(ops); n++ {
			if ops[n].kind != ' ' {
				end = n
			} else if n-end > 2*context {
				break
			}
		}
		end = min(end+context+1, len(ops))
		var oldN, newN int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				oldN++
			}
			if o.kind != '-' {
				newN++
			}
		}
		oldStart, newStart := ops[start].i+1, ops[start].j+1
		if oldN == 0 {
			oldStart--
		}
		if newN == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldN, newStart, newN)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String()
}

func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
