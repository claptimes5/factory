package verdict

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Groom is the groom stage's verdict: issues it thinks are duplicates,
// stale, or too big for one change. The factory posts the text of each on
// the issue, so Sift holds it to plain prose about issues it was shown.
type Groom struct {
	Duplicates []GroomDuplicate `json:"duplicates"`
	Stale      []GroomStale     `json:"stale"`
	TooBig     []GroomTooBig    `json:"too_big"`
	Summary    string           `json:"summary"`
}

// GroomDuplicate is a group of issues that ask for the same thing, and
// the one to keep.
type GroomDuplicate struct {
	Canonical  int    `json:"canonical"`
	Duplicates []int  `json:"duplicates"`
	Why        string `json:"why"`
}

// GroomStale is an issue that's likely obsolete.
type GroomStale struct {
	Issue int    `json:"issue"`
	Why   string `json:"why"`
}

// GroomTooBig is an issue too big for one change, and how to split it.
type GroomTooBig struct {
	Issue int          `json:"issue"`
	Why   string       `json:"why"`
	Split []GroomChild `json:"split"`
}

// GroomChild is one proposed sub-issue.
type GroomChild struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Caps on a groom verdict.
const (
	MaxGroomEntries = 20 // of each kind
	MaxDuplicates   = 4  // besides the canonical one, per group
	MinSplit        = 2
	MaxSplit        = 5
	MaxGroomWhy     = 600
	MaxChildTitle   = 120
	MaxChildBody    = 2000
)

// Validate checks the verdict's shape. What it says about which issues is
// checked by Sift, which drops a bad proposal rather than the whole run.
func (g *Groom) Validate() error {
	var errs []error
	if g.Duplicates == nil || g.Stale == nil || g.TooBig == nil {
		errs = append(errs, errors.New("duplicates, stale and too_big are required (use [])"))
	}
	for name, n := range map[string]int{"duplicates": len(g.Duplicates), "stale": len(g.Stale), "too_big": len(g.TooBig)} {
		if n > MaxGroomEntries {
			errs = append(errs, fmt.Errorf("at most %d %s, got %d", MaxGroomEntries, name, n))
		}
	}
	return errors.Join(errs...)
}

// GroomScope is what a groom job was shown.
type GroomScope struct {
	// Flaggable are the issues it may flag.
	Flaggable map[int]bool
	// Old are the flaggable issues idle long enough to be stale.
	Old map[int]bool
	// Known are every issue it was shown, flaggable or not: a duplicate's
	// canonical issue, and any #N in its text, must be one.
	Known map[int]bool
	// NotCanonical are issues already proposed closed (a duplicate, or
	// stale): none may be another's canonical issue.
	NotCanonical map[int]bool
}

// Sift keeps the proposals that hold up against what the job was shown,
// and says why it dropped each of the others. Each issue is flagged once,
// the first proposal naming it winning, and a duplicate's canonical issue
// is never flagged itself: of two issues, only one can be proposed closed.
func (g *Groom) Sift(s GroomScope) (Groom, []string) {
	out := Groom{Duplicates: []GroomDuplicate{}, Stale: []GroomStale{}, TooBig: []GroomTooBig{}, Summary: g.Summary}
	var dropped []string
	taken := map[int]bool{} // flagged
	canon := map[int]bool{} // kept as some duplicate's canonical issue
	drop := func(f string, a ...any) { dropped = append(dropped, fmt.Sprintf(f, a...)) }
	free := func(at string, n int) bool {
		switch {
		case taken[n]:
			drop("%s: #%d is already flagged", at, n)
		case canon[n]:
			drop("%s: #%d is a duplicate's canonical issue", at, n)
		default:
			return true
		}
		return false
	}
	for i, d := range g.Duplicates {
		at := fmt.Sprintf("duplicates[%d]", i)
		var keep []int
		switch {
		case !s.Known[d.Canonical]:
			drop("%s: canonical #%d wasn't in the input", at, d.Canonical)
			continue
		case s.NotCanonical[d.Canonical]:
			drop("%s: canonical #%d is itself proposed closed", at, d.Canonical)
			continue
		case taken[d.Canonical]:
			drop("%s: canonical #%d is already flagged", at, d.Canonical)
			continue
		case len(d.Duplicates) == 0 || len(d.Duplicates) > MaxDuplicates:
			drop("%s: names %d duplicates; 1 to %d", at, len(d.Duplicates), MaxDuplicates)
			continue
		}
		why, err := s.posted(d.Why, MaxGroomWhy, false)
		if err != nil {
			drop("%s: why: %v", at, err)
			continue
		}
		for _, n := range d.Duplicates {
			switch {
			case n == d.Canonical:
				drop("%s: #%d is its own canonical", at, n)
			case !s.Flaggable[n]:
				drop("%s: #%d wasn't in the input, or may not be flagged", at, n)
			case free(at, n):
				taken[n] = true
				keep = append(keep, n)
			}
		}
		if len(keep) > 0 {
			canon[d.Canonical] = true
			out.Duplicates = append(out.Duplicates, GroomDuplicate{Canonical: d.Canonical, Duplicates: keep, Why: why})
		}
	}
	for i, st := range g.Stale {
		at := fmt.Sprintf("stale[%d]", i)
		why, err := s.posted(st.Why, MaxGroomWhy, false)
		switch {
		case !s.Flaggable[st.Issue]:
			drop("%s: #%d wasn't in the input, or may not be flagged", at, st.Issue)
		case !s.Old[st.Issue]:
			drop("%s: #%d had activity within grooming.stale_after", at, st.Issue)
		case !free(at, st.Issue):
		case err != nil:
			drop("%s: why: %v", at, err)
		default:
			taken[st.Issue] = true
			out.Stale = append(out.Stale, GroomStale{Issue: st.Issue, Why: why})
		}
	}
	for i, tb := range g.TooBig {
		at := fmt.Sprintf("too_big[%d]", i)
		switch {
		case !s.Flaggable[tb.Issue]:
			drop("%s: #%d wasn't in the input, or may not be flagged", at, tb.Issue)
			continue
		case !free(at, tb.Issue):
			continue
		case len(tb.Split) < MinSplit || len(tb.Split) > MaxSplit:
			drop("%s: splits #%d into %d; %d to %d", at, tb.Issue, len(tb.Split), MinSplit, MaxSplit)
			continue
		}
		why, err := s.posted(tb.Why, MaxGroomWhy, false)
		kept := GroomTooBig{Issue: tb.Issue, Why: why}
		for k, c := range tb.Split {
			if err != nil {
				break
			}
			var title, body string
			if title, err = s.posted(c.Title, MaxChildTitle, true); err != nil {
				err = fmt.Errorf("split[%d].title: %w", k, err)
			} else if body, err = s.posted(c.Body, MaxChildBody, false); err != nil {
				err = fmt.Errorf("split[%d].body: %w", k, err)
			}
			kept.Split = append(kept.Split, GroomChild{Title: title, Body: body})
		}
		if err != nil {
			drop("%s: %v", at, err)
			continue
		}
		taken[tb.Issue] = true
		out.TooBig = append(out.TooBig, kept)
	}
	return out, dropped
}

// posted checks text and returns it as the factory posts it: trimmed,
// with an amount such as $5 or ~5 in a code span, where GitLab reads no
// reference.
func (s GroomScope) posted(t string, max int, oneLine bool) (string, error) {
	if err := s.text(t, max, oneLine); err != nil {
		return "", err
	}
	return softRE.ReplaceAllString(strings.TrimSpace(t), "$1`$2`"), nil
}

var (
	linkRE    = regexp.MustCompile(`(?i)\b(?:https?|ftp|file|mailto|javascript|data|vbscript):|\bwww\.|\]\(|\]\[|\]:`)
	mentionRE = regexp.MustCompile(`@[A-Za-z0-9_-]`)
	htmlRE    = regexp.MustCompile(`<[A-Za-z!/?]|&#|&[a-z]+;`)
	// References that would link or notify: GitLab's merge requests,
	// labels, milestones, epics, iterations and vulnerabilities, another
	// repo's issues, and GitHub's GH-123 advisories.
	glRefRE = regexp.MustCompile(`![0-9]|~[A-Za-z"]|%[A-Za-z0-9"]|&[0-9]|\*iteration:|\[vulnerability:|\b[Gg][Hh]-[0-9]|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+[#!][0-9]`)
	// softRE finds an amount, such as $5 or ~5, that GitLab would read as a
	// snippet or label reference.
	softRE    = regexp.MustCompile("(^|[^`0-9A-Za-z])([$~][0-9][0-9.,]*)")
	issueRefs = regexp.MustCompile(`#([0-9]+)`)
)

// text checks model text the factory will post: plain prose of at most
// max runes, with no link, mention, HTML or reference that would act or
// notify anyone, and #N only for an issue the job was shown. A quick
// action (a line starting with /) would run on GitLab. oneLine allows no
// line break.
func (s GroomScope) text(t string, max int, oneLine bool) error {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return errors.New("is empty")
	case utf8.RuneCountInString(t) > max:
		return fmt.Errorf("is over %d characters", max)
	case oneLine && strings.ContainsAny(t, "\r\n"):
		return errors.New("must be one line")
	case linkRE.MatchString(t):
		return errors.New("has a link")
	case mentionRE.MatchString(t):
		return errors.New("has an @mention")
	case htmlRE.MatchString(t):
		return errors.New("has HTML")
	case glRefRE.MatchString(t):
		return errors.New("has a reference to something else")
	case strings.ContainsAny(t, "\x00\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"):
		return errors.New("has control characters")
	}
	for _, l := range strings.Split(t, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "/") {
			return errors.New("has a line starting with / (a quick action)")
		}
	}
	for _, m := range issueRefs.FindAllStringSubmatch(t, -1) {
		if n, err := strconv.Atoi(m[1]); err != nil || !s.Known[n] {
			return fmt.Errorf("refers to #%s, which wasn't in the input", m[1])
		}
	}
	return nil
}

func init() {
	schemas["groom"] = `{"type":"object","additionalProperties":false,
"required":["duplicates","stale","too_big","summary"],
"properties":{
 "duplicates":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["canonical","duplicates","why"],
   "properties":{"canonical":{"type":"integer"},"duplicates":{"type":"array","items":{"type":"integer"}},"why":{"type":"string"}}}},
 "stale":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["issue","why"],
   "properties":{"issue":{"type":"integer"},"why":{"type":"string"}}}},
 "too_big":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["issue","why","split"],
   "properties":{"issue":{"type":"integer"},"why":{"type":"string"},
    "split":{"type":"array","items":{"type":"object","additionalProperties":false,
      "required":["title","body"],"properties":{"title":{"type":"string"},"body":{"type":"string"}}}}}}},
 "summary":{"type":"string"}}}`
}
