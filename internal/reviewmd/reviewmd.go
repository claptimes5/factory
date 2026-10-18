// Package reviewmd reads a repo's REVIEW.md: short, human-written guidance
// for the factory's reviewers (what failure looks like in the repo, paths
// to skip, real bugs from its past), with an optional front matter block
// the daemon acts on itself:
//
//	---
//	skip: [docs/, "*.lock", internal/gen/]
//	max_minor: 3
//	---
//
// The file is read from the base branch, never from the change, so a
// builder can't change what reviews it. Its text is the repo's, written by
// people with write access: guidance for the reviewer, never permission
// to pass anything.
package reviewmd

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/features"
)

// Files are where REVIEW.md may be, in the order they are looked for: the
// first one found is used.
var Files = []string{"REVIEW.md", ".github/REVIEW.md"}

// MaxBytes is the most guidance text a reviewer is given; the rest is
// left out, with a note.
const MaxBytes = 16 << 10

// Guide is a parsed REVIEW.md.
type Guide struct {
	File string // where it was found, such as REVIEW.md
	// Text is the guidance: HTML comments and the front matter taken out,
	// invisible and control characters dropped, at most MaxBytes.
	Text string
	// Skip are path globs whose findings are notes only (see Skips).
	Skip []string
	// MaxMinor caps the minor findings kept as notes; nil for no cap.
	MaxMinor *int
	// Notes say what of the file the daemon ignored or cut, for people.
	Notes []string
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// frontMatter is a block of YAML between --- lines at the very top.
	frontMatter = regexp.MustCompile(`(?s)\A---[ \t]*\r?\n(.*?)\r?\n---[ \t]*(?:\r?\n|\z)`)
)

// Parse reads REVIEW.md's contents, found at file.
func Parse(file, raw string) *Guide {
	g := &Guide{File: file}
	raw = strings.TrimPrefix(raw, "\ufeff")
	// Comments hide text from the people who read the file on the forge,
	// so nothing in one reaches a reviewer, nor counts as front matter.
	raw = htmlComment.ReplaceAllString(raw, "")
	if i := strings.Index(raw, "<!--"); i >= 0 {
		raw = raw[:i] // one that never closes hides the rest
	}
	if m := frontMatter.FindStringSubmatchIndex(raw); m != nil {
		g.settings(raw[m[2]:m[3]])
		raw = raw[m[1]:]
	}
	text := clean(raw)
	if len(text) > MaxBytes {
		cut := MaxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl > MaxBytes/2 {
			cut = nl
		}
		text = strings.TrimRight(text[:cut], " \t\n") + "\n\n(The rest of " + file + " is left out: it's over " + fmt.Sprint(MaxBytes>>10) + " KB.)"
		g.Notes = append(g.Notes, fmt.Sprintf("%s is over %d KB, so reviewers get only its first %d KB", file, MaxBytes>>10, MaxBytes>>10))
	}
	g.Text = strings.TrimSpace(text)
	return g
}

// settings reads the front matter. A key it doesn't know, or a value it
// can't use, is left out with a note; the rest still counts.
func (g *Guide) settings(y string) {
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		g.Notes = append(g.Notes, "its front matter isn't valid YAML, so it's ignored: "+oneLine(err.Error()))
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := m[k]
		switch k {
		case "skip":
			var list []string
			if n.Kind == yaml.ScalarNode {
				list = []string{n.Value}
			} else if err := n.Decode(&list); err != nil {
				g.Notes = append(g.Notes, "skip must be a list of paths or globs, so it's ignored")
				continue
			}
			for _, s := range list {
				s = strings.TrimPrefix(strings.TrimSpace(s), "./")
				switch {
				case s == "" || s == "/" || s == "*" || s == ".":
					g.Notes = append(g.Notes, fmt.Sprintf("skip %q would skip everything, so it's ignored", oneLine(s)))
				case strings.HasPrefix(s, "/") || strings.Contains(s, ".."):
					g.Notes = append(g.Notes, fmt.Sprintf("skip %q isn't a path in the repo, so it's ignored", oneLine(s)))
				default:
					if !slices.Contains(g.Skip, s) {
						g.Skip = append(g.Skip, s)
					}
				}
			}
		case "max_minor":
			var v int
			if err := n.Decode(&v); err != nil || v < 0 {
				g.Notes = append(g.Notes, "max_minor must be a whole number, 0 or more, so it's ignored")
				continue
			}
			g.MaxMinor = &v
		default:
			g.Notes = append(g.Notes, fmt.Sprintf("unknown key %q is ignored (the factory reads skip and max_minor)", oneLine(k)))
		}
	}
}

// Skips says whether REVIEW.md's skip list covers a path. Whether a skip
// applies at all is the caller's to decide: never on a protected or
// security-sensitive path (see Sensitive).
func (g *Guide) Skips(path string) bool {
	if g == nil || path == "" {
		return false
	}
	path = strings.TrimPrefix(path, "./")
	return slices.ContainsFunc(g.Skip, func(p string) bool { return features.Match(p, path) })
}

// sensitive are path words that suggest auth, secrets, money or the
// build itself: a skip never covers them.
var sensitive = regexp.MustCompile(`(?i)auth|login|logout|session|passw|secret|credential|crypt|token|oauth|saml|sso|permission|acl|rbac|security|payment|billing|invoice|checkout|wallet|\.env|(^|/)dockerfile|(^|/)\.github/|(^|/)\.gitlab|migrat`)

// Sensitive says whether a path looks security-sensitive by its name, so
// REVIEW.md can't skip it.
func Sensitive(path string) bool { return sensitive.MatchString(path) }

// Summary is the guide as a reviewer is shown it: its settings, then its
// text. It is still the repo's text, for the caller to fence.
func (g *Guide) Summary() string {
	var b strings.Builder
	if len(g.Skip) > 0 {
		var s []string
		for _, p := range g.Skip {
			s = append(s, oneLine(p))
		}
		fmt.Fprintf(&b, "skip: %s\n", strings.Join(s, ", "))
	}
	if g.MaxMinor != nil {
		fmt.Fprintf(&b, "max_minor: %d\n", *g.MaxMinor)
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(g.Text)
	return strings.TrimSpace(b.String())
}

// Empty says there's nothing in it for a reviewer.
func (g *Guide) Empty() bool {
	return g == nil || (g.Text == "" && len(g.Skip) == 0 && g.MaxMinor == nil)
}

// clean drops control and invisible characters (bidi overrides,
// zero-width spaces) but keeps lines and tabs.
func clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return ' '
		case unicode.Is(unicode.Cf, r), r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(clean(s)), " ")
	if utf8.RuneCountInString(s) > 100 {
		s = string([]rune(s)[:99]) + "…"
	}
	return s
}
