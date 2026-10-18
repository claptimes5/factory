package verdict

import (
	"errors"
	"fmt"
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
