package web

import (
	"context"
	"fmt"
	"strings"

	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// APIHappened is how an item that stopped got there, from its jobs and
// events: the item's header shows only where it is now.
type APIHappened struct {
	// Summary is the whole story in a sentence ("The build ran out of
	// turns (job #12), then its retry was cancelled by you (job #14).").
	Summary string `json:"summary"`
	// Steps are its parts, in order, each with its job to link.
	Steps []APIHappenedStep `json:"steps"`
	// Note says what to do when the last build failed a check that fails
	// on the base branch too, as the setup check found: no change passes
	// it until the base does. "" otherwise.
	Note string `json:"note"`
}

// APIHappenedStep is one thing that happened.
type APIHappenedStep struct {
	Text  string `json:"text"`
	JobID *int64 `json:"job_id"`
}

// APISplit is the "Split it" button: a chat message asking the
// orchestrator's model to split a stopped item into smaller issues.
type APISplit struct {
	Prompt string `json:"prompt"`
	// Unavailable says why chat can't take it now, or "".
	Unavailable string `json:"unavailable"`
	// Workspace is the item's, which the chat that splits it starts in.
	Workspace string `json:"workspace"`
}

// happenedJobs is how many of the latest jobs that ended badly it tells.
const happenedJobs = 3

// stoppedJobs are the item's latest jobs that ended badly, oldest first,
// once nothing of it runs or waits. Jobs run inside another are left out.
func stoppedJobs(jobs []*store.Job) []*store.Job {
	var out []*store.Job
	for i := len(jobs) - 1; i >= 0 && len(out) < happenedJobs; i-- {
		j := jobs[i]
		if store.IsSubKind(j.Kind) {
			continue
		}
		if !j.State.Terminal() {
			return nil
		}
		if j.State == state.Succeeded {
			break
		}
		out = append([]*store.Job{j}, out...)
	}
	return out
}

// whatHappened tells how a parked item, or one whose latest jobs ended
// badly, stopped; nil for one that hasn't.
func (s *Server) whatHappened(it *store.WorkItem, jobs []*store.Job, evs []store.Event) *APIHappened {
	if it.Stage == state.Done || it.Stage == state.Epic {
		return nil
	}
	bad := stoppedJobs(jobs)
	if len(bad) == 0 && it.Stage != state.Parked {
		return nil
	}
	h := &APIHappened{Steps: []APIHappenedStep{}}
	var prev string
	for _, j := range bad {
		noun := "the " + jobNoun(j.Kind)
		if j.Kind == prev {
			noun = "its retry"
		}
		prev = j.Kind
		id := j.ID
		h.Steps = append(h.Steps, APIHappenedStep{Text: noun + " " + s.howItEnded(j, evs), JobID: &id})
	}
	if len(bad) == 0 {
		h.Steps = append(h.Steps, APIHappenedStep{Text: parkedText(evs)})
	}
	var parts []string
	for _, st := range h.Steps {
		t := st.Text
		if st.JobID != nil {
			t += fmt.Sprintf(" (job #%d)", *st.JobID)
		}
		parts = append(parts, t)
	}
	h.Summary = capitalFirst(strings.Join(parts, ", then "))
	if !strings.ContainsAny(h.Summary[len(h.Summary)-1:], ".?!") {
		h.Summary += "."
	}
	h.Steps[0].Text = capitalFirst(h.Steps[0].Text)
	if len(bad) > 0 {
		h.Note = s.baseRedNote(it.Repo, bad[len(bad)-1])
	}
	return h
}

// baseRedNote names the checks a failed build failed that the setup
// check found failing on the base branch too.
func (s *Server) baseRedNote(repo string, j *store.Job) string {
	rep, err := readiness.Load(s.ReadinessDir, repo)
	if s.ReadinessDir == "" || err != nil || rep == nil {
		return ""
	}
	var red []string
	for _, gate := range rep.RedOnBase(j.Error) {
		red = append(red, "`"+gate+"`")
	}
	if len(red) == 0 {
		return ""
	}
	verb := "fails"
	if len(red) > 1 {
		verb = "fail"
	}
	return fmt.Sprintf("%s %s on %s too, as the setup check found, and every change has to pass it, so retrying or splitting this won't help. "+
		"Make it pass on %s first (see Setup), or take it out of the required gates in factory.yaml until it does.", strings.Join(red, " and "), verb, rep.Branch, rep.Branch)
}

// sizeStop says whether a build stopped because the work was too big for
// it: out of turns or budget, out of time, or over the diff size limit.
// Splitting helps those, not a check that fails.
func (s *Server) sizeStop(j *store.Job) bool {
	return j.State == state.TimedOut || s.jobStop(j) != nil || strings.Contains(j.Error, "split the task")
}

// howItEnded is a job's ending as the store has it.
func (s *Server) howItEnded(j *store.Job, evs []store.Event) string {
	switch j.State {
	case state.TimedOut:
		return "timed out"
	case state.Cancelled:
		for i := len(evs) - 1; i >= 0; i-- {
			if e := evs[i]; e.Type == "job_cancelled" && e.JobID != nil && *e.JobID == j.ID {
				return "was cancelled " + byWhom(e.Actor)
			}
		}
		return "was cancelled"
	}
	if st := s.jobStop(j); st != nil {
		if st.Reason == "budget" {
			return fmt.Sprintf("reached its $%.2f budget", st.BudgetUSD)
		}
		return "ran out of turns"
	}
	if gates := failedGates(j.Error); len(gates) > 0 {
		noun := "check"
		if len(gates) > 1 {
			noun = "checks"
		}
		return "failed the " + joinAnd(gates) + " " + noun
	}
	if why := reasonLine(j.Error); why != "" {
		return "failed: " + why
	}
	return "failed"
}

// failedGates are the gates a build's "GATES: FAIL a=pass b=fail" line
// says failed, as a person reads them.
func failedGates(err string) []string {
	line, _, _ := strings.Cut(err, "\n")
	if !strings.HasPrefix(line, "GATES: FAIL") {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(line) {
		if g, ok := strings.CutSuffix(f, "=fail"); ok {
			out = append(out, strings.ReplaceAll(g, "_", " "))
		} else if g, ok := strings.CutSuffix(f, "=misconfigured"); ok {
			out = append(out, strings.ReplaceAll(g, "_", " ")+" (misconfigured)")
		}
	}
	return out
}

// joinAnd is "a", "a and b", or "a, b and c".
func joinAnd(xs []string) string {
	if len(xs) <= 1 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// parkedText is why the item was parked, from its latest move to parked.
func parkedText(evs []store.Event) string {
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Type != "stage" || !strings.Contains(e.Data, `"to":"parked"`) {
			continue
		}
		t := "it was parked " + byWhom(e.Actor)
		if why := reasonLine(e.Reason); why != "" {
			t += ": " + strings.TrimSuffix(why, ".")
		}
		return t
	}
	return "it was parked"
}

func byWhom(actor string) string {
	switch actor {
	case "human", "":
		return "by you"
	case "daemon":
		return "by the factory"
	}
	if strings.HasPrefix(actor, "@") {
		return "by " + actor
	}
	return "by the " + actor
}

// jobNoun is a job kind as a person says it.
func jobNoun(kind string) string {
	switch kind {
	case "implement":
		return "build"
	case "critic":
		return "critic's review"
	case "plan_check":
		return "plan check"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

// reasonLine is a message's first line, cut short.
func reasonLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return trim(s, 160)
}

func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// splitOffer is the "Split it" button for an item whose build failed or
// ran out of turns, or that was parked; nil for others.
func (s *Server) splitOffer(ctx context.Context, it *store.WorkItem, jobs []*store.Job, h *APIHappened) *APISplit {
	if h == nil {
		return nil
	}
	var failed *store.Job
	for _, j := range stoppedJobs(jobs) {
		if j.Kind == "implement" && (j.State == state.Failed || j.State == state.TimedOut) {
			failed = j
		}
	}
	if failed == nil && it.Stage != state.Parked {
		return nil
	}
	// A build that failed its checks isn't helped by smaller pieces.
	if failed != nil && !s.sizeStop(failed) {
		return nil
	}
	sp := &APISplit{Prompt: SplitPrompt(it, h.Summary, failed), Workspace: s.workspaceOf(it.Repo)}
	if s.Chat == nil {
		sp.Unavailable = "Chat isn't available in this dashboard."
	} else {
		sp.Unavailable = s.Chat.ChatUnavailable(ctx, sp.Workspace)
	}
	return sp
}

// SplitPrompt asks chat to split a stopped item into smaller issues.
// failed is the build whose log to read, or nil.
func SplitPrompt(it *store.WorkItem, happened string, failed *store.Job) string {
	var b strings.Builder
	// The title is the issue's author's text: fenced, it reads as data.
	fmt.Fprintf(&b, "Split item #%d (%s#%d) into smaller issues. Its title:\n%s", it.ID, it.Repo, it.IssueNumber, fenced(it.Title))
	fmt.Fprintf(&b, "Its build stopped: %s ", happened)
	if failed != nil {
		fmt.Fprintf(&b, "Read the issue and the failed run's log (job #%d), ", failed.ID)
	} else {
		b.WriteString("Read the issue, ")
	}
	fmt.Fprintf(&b, "then open smaller issues, each with parent %d (this item, %s#%d), and close it in favor of them with close_split.", it.ID, it.Repo, it.IssueNumber)
	return b.String()
}

// fenced puts text in a code block longer than any run of backticks in
// it, so nothing in it reads as the message's own words.
func fenced(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "text\n" + strings.TrimRight(s, "\n") + "\n" + f + "\n"
}
