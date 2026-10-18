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
	// Answers say, in a fix round, what became of each blocking finding.
	Answers []Answer `json:"answers,omitempty"`
}

// Answer is the builder's word on one finding of a fix round.
type Answer struct {
	ID      string `json:"id"`      // the finding's id, F1
	Outcome string `json:"outcome"` // fixed|wrong|out_of_scope
	Why     string `json:"why"`     // what changed, or why the finding is wrong or out of scope
}

// Finding severities. A pre-existing finding is a real problem the change
// didn't introduce; it never blocks.
const (
	Critical    = "critical"
	Major       = "major"
	Minor       = "minor"
	PreExisting = "pre-existing"
)

// Verifier rulings on a blocking finding (Check.Status).
const (
	Confirmed  = "confirmed"
	Refuted    = "refuted"
	Unverified = "unverified" // the verifier couldn't run or didn't answer: it blocks
)

// Finding is one reviewer finding. The fields after Evidence are the
// daemon's: a reviewer's own verdict can't set them (see ParseReview).
type Finding struct {
	Severity string `json:"severity"` // critical|major|minor|pre-existing
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`

	// ID names the finding across an item's review rounds: F1, F2, ...
	ID string `json:"id,omitempty"`
	// By is the reviewer that raised it when several read the change: review,
	// critic or security.
	By string `json:"by,omitempty"`
	// Carried marks an earlier round's finding this round's reviewer upheld.
	Carried bool `json:"carried,omitempty"`
	// Blocks says whether it can block, as judged when it was raised (see
	// MarkBlocking); a carried finding always can. Nil in verdicts stored
	// before it was recorded.
	Blocks *bool `json:"blocks,omitempty"`
	// Check is the verifier's ruling on a blocking finding.
	Check *Check `json:"check,omitempty"`
	// Skipped says the repo's REVIEW.md skips the finding's path, so it
	// was kept as a note though it would have blocked.
	Skipped bool `json:"skipped,omitempty"`
}

// Check is what the verifier made of a blocking finding.
type Check struct {
	Status string `json:"status"` // confirmed|refuted|unverified
	Reason string `json:"reason"`
	By     string `json:"by,omitempty"` // the verifier's harness/model
}

// Ruling is a re-review's word on a finding an earlier round raised.
type Ruling struct {
	ID     string `json:"id"`
	Ruling string `json:"ruling"` // resolved|upheld|withdrawn
	Reason string `json:"reason"`
}

// Review is the adversarial reviewer's verdict.
type Review struct {
	Decision string    `json:"decision"` // pass|block|questions
	Findings []Finding `json:"findings"`
	// Earlier rules on the open findings of earlier rounds, in a re-review.
	Earlier               []Ruling `json:"earlier,omitempty"`
	ChallengedAssumptions []string `json:"challenged_assumptions"`

	// Verified says the daemon sent the blocking findings to the verifier.
	Verified bool `json:"verified,omitempty"`
	// Reviewers are the reviewers whose findings this verdict holds, when
	// several reviewed side by side (a paired high-risk review, or the
	// security pass beside the reviewer).
	Reviewers []string `json:"reviewers,omitempty"`
	// Security says why the security pass read the change, when it did.
	Security []string `json:"security,omitempty"`
	// BlockedWithoutFinding says a reviewer of this verdict decided block
	// without raising a finding that can block (see MarkBlocking): the
	// verdict blocks whatever findings it holds, from a paired reviewer or
	// carried from an earlier round.
	BlockedWithoutFinding bool `json:"blocked_without_finding,omitempty"`
	// Guide is what the repo's REVIEW.md changed in this verdict.
	Guide *GuideApplied `json:"guide,omitempty"`
}

// GuideApplied is what the repo's REVIEW.md did to a review's findings.
type GuideApplied struct {
	File string `json:"file"` // REVIEW.md or .github/REVIEW.md
	// DroppedMinor counts the minor notes left out past its max_minor.
	DroppedMinor int `json:"dropped_minor,omitempty"`
	// Notes say what of the file was ignored (an unknown key, say).
	Notes []string `json:"notes,omitempty"`
}

// Blocking lists, by index, the findings that can block. Each finding
// says so itself (Blocks), as the daemon marked it when it was raised. A
// verdict stored before findings carried that mark falls back to the rule
// MarkBlocking applies.
func (r *Review) Blocking() []int {
	marked := len(r.Findings) > 0
	for _, f := range r.Findings {
		marked = marked && f.Blocks != nil
	}
	if !marked {
		return blockingByRule(r.Decision, r.Findings)
	}
	var out []int
	for i, f := range r.Findings {
		if *f.Blocks {
			out = append(out, i)
		}
	}
	return out
}

// blockingByRule is which findings can block under a reviewer's own
// decision: on a block, the critical and major ones (or, with none, the
// minor ones it blocked on), otherwise only critical ones. A pre-existing
// finding never blocks.
func blockingByRule(decision string, fs []Finding) []int {
	var hard, minor []int
	for i, f := range fs {
		switch {
		case f.Severity == Critical, f.Severity == Major && decision == "block":
			hard = append(hard, i)
		case f.Severity == Minor && decision == "block":
			minor = append(minor, i)
		}
	}
	if len(hard) == 0 {
		return minor
	}
	return hard
}

// MarkBlocking records on each finding of a reviewer's own verdict whether
// it can block, judged against that reviewer's decision, and whether it
// blocked with no finding that can. Called once, as the verdict is read,
// so merging it with another reviewer's or carrying earlier findings into
// it can't change what blocks.
func (r *Review) MarkBlocking() {
	can := map[int]bool{}
	for _, i := range blockingByRule(r.Decision, r.Findings) {
		can[i] = true
	}
	r.BlockedWithoutFinding = r.Decision == "block" && len(r.Findings) == 0
	for i := range r.Findings {
		b := can[i]
		r.Findings[i].Blocks = &b
	}
}

// Blocks reports whether the review sends the work back: a block decision
// with no findings to check (its own, or a reviewer's in a pair), or a blocking finding the verifier didn't
// refute. A finding the verifier never saw blocks, so a review that wasn't
// verified (or whose verifier couldn't run) blocks as it always did.
func (r *Review) Blocks() bool {
	if r.BlockedWithoutFinding || (r.Decision == "block" && len(r.Findings) == 0) {
		return true
	}
	for _, i := range r.Blocking() {
		if c := r.Findings[i].Check; c == nil || c.Status != Refuted {
			return true
		}
	}
	return false
}

// BlocksOn says whether finding i is one this review blocks on.
func (r *Review) BlocksOn(i int) bool {
	for _, b := range r.Blocking() {
		if b == i {
			c := r.Findings[i].Check
			return c == nil || c.Status != Refuted
		}
	}
	return false
}

// Verify is the verifier's verdict on a review's blocking findings.
type Verify struct {
	Checks []VerifyCheck `json:"checks"`
}

// VerifyCheck is the verifier's ruling on one finding.
type VerifyCheck struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"` // confirmed|refuted
	Reason  string `json:"reason"`
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
	errs := []error{oneOf("status", i.Status, "done", "blocked")}
	for n, a := range i.Answers {
		if strings.TrimSpace(a.ID) == "" {
			errs = append(errs, fmt.Errorf("answers[%d].id is empty", n))
		}
		if err := oneOf(fmt.Sprintf("answers[%d].outcome", n), a.Outcome, "fixed", "wrong", "out_of_scope"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Review) Validate() error {
	errs := []error{oneOf("decision", r.Decision, "pass", "block", "questions")}
	for n, f := range r.Findings {
		if err := oneOf(fmt.Sprintf("findings[%d].severity", n), f.Severity, Critical, Major, Minor, PreExisting); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(f.Claim) == "" {
			errs = append(errs, fmt.Errorf("findings[%d].claim is empty", n))
		}
		if c := f.Check; c != nil {
			if err := oneOf(fmt.Sprintf("findings[%d].check.status", n), c.Status, Confirmed, Refuted, Unverified); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for n, e := range r.Earlier {
		if err := oneOf(fmt.Sprintf("earlier[%d].ruling", n), e.Ruling, "resolved", "upheld", "withdrawn"); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Findings == nil {
		errs = append(errs, errors.New("findings is required (use [])"))
	}
	return errors.Join(errs...)
}

func (v *Verify) Validate() error {
	var errs []error
	for n, c := range v.Checks {
		if err := oneOf(fmt.Sprintf("checks[%d].verdict", n), c.Verdict, Confirmed, Refuted); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(c.Reason) == "" {
			errs = append(errs, fmt.Errorf("checks[%d].reason is empty", n))
		}
	}
	if v.Checks == nil {
		errs = append(errs, errors.New("checks is required"))
	}
	return errors.Join(errs...)
}

// ParseReview parses a reviewer's own verdict: Parse, and none of the
// fields only the daemon sets, so a reviewer can't name its findings,
// mark them checked or claim a second reviewer.
func ParseReview(raw []byte) (*Review, error) {
	var r Review
	if err := Parse(raw, &r); err != nil {
		return nil, err
	}
	var errs []error
	if r.Verified || len(r.Reviewers) > 0 || len(r.Security) > 0 || r.BlockedWithoutFinding || r.Guide != nil {
		errs = append(errs, errors.New("verified, reviewers, security, blocked_without_finding and guide are the factory's to set"))
	}
	for n, f := range r.Findings {
		if f.ID != "" || f.By != "" || f.Carried || f.Check != nil || f.Blocks != nil || f.Skipped {
			errs = append(errs, fmt.Errorf("findings[%d]: id, by, carried, blocks, check and skipped are the factory's to set", n))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("verdict: %w", err)
	}
	r.MarkBlocking()
	return &r, nil
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
 "pages":{"type":"array","items":{"type":"string"}},
 "answers":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["id","outcome","why"],
   "properties":{
    "id":{"type":"string"},
    "outcome":{"type":"string","enum":["fixed","wrong","out_of_scope"]},
    "why":{"type":"string"}}}}}}`,
	"review": `{"type":"object","additionalProperties":false,
"required":["decision","findings","earlier","challenged_assumptions"],
"properties":{
 "decision":{"type":"string","enum":["pass","block","questions"]},
 "findings":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["severity","file","line","claim","evidence"],
   "properties":{
    "severity":{"type":"string","enum":["critical","major","minor","pre-existing"]},
    "file":{"type":"string"},"line":{"type":"integer"},
    "claim":{"type":"string"},"evidence":{"type":"string"}}}},
 "earlier":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["id","ruling","reason"],
   "properties":{
    "id":{"type":"string"},
    "ruling":{"type":"string","enum":["resolved","upheld","withdrawn"]},
    "reason":{"type":"string"}}}},
 "challenged_assumptions":{"type":"array","items":{"type":"string"}}}}`,
	"verify": `{"type":"object","additionalProperties":false,
"required":["checks"],
"properties":{
 "checks":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["id","verdict","reason"],
   "properties":{
    "id":{"type":"string"},
    "verdict":{"type":"string","enum":["confirmed","refuted"]},
    "reason":{"type":"string"}}}}}}`,
}
