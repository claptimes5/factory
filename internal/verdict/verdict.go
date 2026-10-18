// Package verdict defines and validates the structured verdict each stage
// must end with. A missing or invalid verdict fails the job (fail closed).
package verdict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Triage is the triage stage's verdict.
type Triage struct {
	Decision   string `json:"decision"`   // ready|park
	Complexity string `json:"complexity"` // XS|S|M|L
	Risk       string `json:"risk"`       // low|high
	Question   string `json:"question"`   // set when parking: what is missing
	Summary    string `json:"summary"`
}

// Implement is the builder's report. The daemon re-runs every gate itself;
// this only says whether the builder believes it finished.
type Implement struct {
	Status     string   `json:"status"` // done|blocked
	Summary    string   `json:"summary"`
	TestsAdded []string `json:"tests_added"`
	Blocker    string   `json:"blocker"`
	// Pages are the app's pages the change touches, as paths, for its
	// screenshots; only asked for when the repo has previews.
	Pages []string `json:"pages,omitempty"`
}

// Finding is one reviewer finding.
type Finding struct {
	Severity string `json:"severity"` // critical|major|minor
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
}

// Review is the adversarial reviewer's verdict.
type Review struct {
	Decision              string    `json:"decision"` // pass|block|questions
	Findings              []Finding `json:"findings"`
	ChallengedAssumptions []string  `json:"challenged_assumptions"`
}

// Blocks reports whether the review sends the work back: a block decision
// or any critical finding.
func (r *Review) Blocks() bool {
	if r.Decision == "block" {
		return true
	}
	for _, f := range r.Findings {
		if f.Severity == "critical" {
			return true
		}
	}
	return false
}

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s = %q, want one of %s", field, v, strings.Join(allowed, "|"))
}

func (t *Triage) Validate() error {
	errs := []error{oneOf("decision", t.Decision, "ready", "park")}
	if t.Decision == "ready" {
		errs = append(errs, oneOf("complexity", t.Complexity, "XS", "S", "M", "L"), oneOf("risk", t.Risk, "low", "high"))
	}
	if t.Decision == "park" && strings.TrimSpace(t.Question) == "" {
		errs = append(errs, errors.New("park needs a question"))
	}
	return errors.Join(errs...)
}

func (i *Implement) Validate() error {
	return oneOf("status", i.Status, "done", "blocked")
}

func (r *Review) Validate() error {
	errs := []error{oneOf("decision", r.Decision, "pass", "block", "questions")}
	for n, f := range r.Findings {
		if err := oneOf(fmt.Sprintf("findings[%d].severity", n), f.Severity, "critical", "major", "minor"); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(f.Claim) == "" {
			errs = append(errs, fmt.Errorf("findings[%d].claim is empty", n))
		}
	}
	if r.Findings == nil {
		errs = append(errs, errors.New("findings is required (use [])"))
	}
	return errors.Join(errs...)
}

type validator interface{ Validate() error }

// Parse strictly decodes raw JSON into v and validates it. Text around a
// single JSON object (such as a model's final message wrapped in a code
// fence) is tolerated; anything else is an error.
func Parse(raw []byte, v validator) error {
	raw = extractObject(raw)
	if len(raw) == 0 {
		return errors.New("verdict: empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("verdict: %w", err)
	}
	if err := v.Validate(); err != nil {
		return fmt.Errorf("verdict: %w", err)
	}
	return nil
}

func extractObject(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		return b
	}
	i, j := bytes.IndexByte(b, '{'), bytes.LastIndexByte(b, '}')
	if i < 0 || j < i {
		return nil
	}
	return b[i : j+1]
}

// Load reads the verdict file if it exists, otherwise falls back to the
// harness's final message (read-only runs cannot write files).
func Load(path string, final string, v validator) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		raw = []byte(final)
	}
	if err := Parse(raw, v); err != nil {
		return raw, err
	}
	norm, _ := json.Marshal(v)
	return norm, nil
}

// Schema returns the strict JSON schema for a stage's verdict, suitable for
// `codex exec --output-schema` and `claude --json-schema`.
func Schema(stage string) (string, error) {
	s, ok := schemas[stage]
	if !ok {
		return "", fmt.Errorf("no verdict schema for stage %q", stage)
	}
	return s, nil
}

var schemas = map[string]string{
	"triage": `{"type":"object","additionalProperties":false,
"required":["decision","complexity","risk","question","summary"],
"properties":{
 "decision":{"type":"string","enum":["ready","park"]},
 "complexity":{"type":"string","enum":["XS","S","M","L"]},
 "risk":{"type":"string","enum":["low","high"]},
 "question":{"type":"string"},
 "summary":{"type":"string"}}}`,
	"implement": `{"type":"object","additionalProperties":false,
"required":["status","summary","tests_added","blocker"],
"properties":{
 "status":{"type":"string","enum":["done","blocked"]},
 "summary":{"type":"string"},
 "tests_added":{"type":"array","items":{"type":"string"}},
 "blocker":{"type":"string"},
 "pages":{"type":"array","items":{"type":"string"}}}}`,
	"review": `{"type":"object","additionalProperties":false,
"required":["decision","findings","challenged_assumptions"],
"properties":{
 "decision":{"type":"string","enum":["pass","block","questions"]},
 "findings":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["severity","file","line","claim","evidence"],
   "properties":{
    "severity":{"type":"string","enum":["critical","major","minor"]},
    "file":{"type":"string"},"line":{"type":"integer"},
    "claim":{"type":"string"},"evidence":{"type":"string"}}}},
 "challenged_assumptions":{"type":"array","items":{"type":"string"}}}}`,
}
