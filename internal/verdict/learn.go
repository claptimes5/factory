package verdict

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Learn is the learn stage's verdict: lines it proposes for the repo's
// AGENTS.md, or why it proposes none.
type Learn struct {
	Lessons    []Lesson `json:"lessons"`
	SkipReason string   `json:"skip_reason"`
}

// Lesson is one proposed line and what it was learned from.
type Lesson struct {
	Line     string   `json:"line"`     // one imperative sentence
	Why      string   `json:"why"`      // what happened, citing items and PRs
	Evidence []string `json:"evidence"` // issue or PR references, such as #12 or PR #101
}

// MaxLessons is the most lines one learn run may propose.
const MaxLessons = 3

func (l *Learn) Validate() error {
	var errs []error
	if l.Lessons == nil {
		errs = append(errs, errors.New("lessons is required (use [])"))
	}
	if len(l.Lessons) > MaxLessons {
		errs = append(errs, fmt.Errorf("at most %d lessons, got %d", MaxLessons, len(l.Lessons)))
	}
	for n, s := range l.Lessons {
		if strings.TrimSpace(s.Line) == "" {
			errs = append(errs, fmt.Errorf("lessons[%d].line is empty", n))
		} else if why := unsafeLine(s.Line); why != "" {
			errs = append(errs, fmt.Errorf("lessons[%d].line %s", n, why))
		}
		if strings.TrimSpace(s.Why) == "" {
			errs = append(errs, fmt.Errorf("lessons[%d].why is empty", n))
		}
	}
	if len(l.Lessons) == 0 && strings.TrimSpace(l.SkipReason) == "" {
		errs = append(errs, errors.New("no lessons needs a skip_reason"))
	}
	return errors.Join(errs...)
}

// mdLink is a markdown link or image, inline ([x](y)) or by reference
// ([x][y], or a [y]: definition).
var mdLink = regexp.MustCompile(`\]\s*[(\[]|^\s*\[[^\]]*\]:`)

// unsafeLine says why a line can't go into an instructions file builders
// read, or "": HTML (a comment hides text from the PR's reviewer), an
// @mention or @import (Claude Code reads @path as a file to load), or a
// link or image.
func unsafeLine(l string) string {
	switch {
	case strings.Contains(l, "<"):
		return "has a <, which could be HTML"
	case strings.Contains(l, "@"):
		return "has an @, which could mention someone or import a file"
	case mdLink.MatchString(l):
		return "has a markdown link or image"
	}
	return ""
}

func init() {
	schemas["learn"] = `{"type":"object","additionalProperties":false,
"required":["lessons","skip_reason"],
"properties":{
 "lessons":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["line","why","evidence"],
   "properties":{
    "line":{"type":"string"},"why":{"type":"string"},
    "evidence":{"type":"array","items":{"type":"string"}}}}},
 "skip_reason":{"type":"string"}}}`
}
