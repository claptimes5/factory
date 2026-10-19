package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A build that runs out of turns or hits its dollar cap hasn't failed:
// it ran out of room, usually partway through. Starting it over would
// most likely run out again, so the factory doesn't: it keeps the
// checkout as the builder left it and the CLI's session, parks the item
// and asks a person. Continue picks up in that checkout and session with
// a fresh allowance (more if they add some) and their note, for example
// to narrow the scope; Retry and Tier up start over as before.
//
// The kept work never leaves the machine: the checkouts move to
// <job>/kept under the data directory, and nothing is pushed until a
// build's gates pass, as always. <job>/stop.json says what happened and
// <job>/saved.patch is the work against the base branch, for the job
// page; they stay after the checkout goes.

// ContinueInput is carried by a build that continues a stopped one.
type ContinueInput struct {
	From int64  `json:"from"`           // the stopped job
	Note string `json:"note,omitempty"` // a person's note to the builder
	// Extra adds turns and budget as a fraction of the usual: 0.5 is
	// half as much again.
	Extra float64 `json:"extra,omitempty"`
}

// MaxContinueExtra is the most Extra a person may add: three times the
// usual turns and budget in all.
const MaxContinueExtra = 2.0

// keepStoppedFor is how long a stopped build's checkout is kept for a
// parked item no one has come back to.
const keepStoppedFor = 14 * 24 * time.Hour

// StopRecord is <job>/stop.json: how a build stopped, in plain words.
type StopRecord struct {
	Reason    string  `json:"reason"` // turns or budget
	Turns     int     `json:"turns"`
	MaxTurns  int     `json:"max_turns"`
	CostUSD   float64 `json:"cost_usd"`
	BudgetUSD float64 `json:"budget_usd"`
	Summary   string  `json:"summary"`
	// Note is what the builder wrote down before it stopped (see
	// harness.ProgressFile), scrubbed of secrets.
	Note string `json:"note,omitempty"`
	// Kept is false when the checkout couldn't be kept; KeptError says why.
	Kept      bool   `json:"kept"`
	KeptError string `json:"kept_error,omitempty"`
	At        string `json:"at"`
}

// keptCheckout is <job>/kept/kept.json: enough to rebuild the parts.
type keptCheckout struct {
	Job   int64      `json:"job"`
	Parts []keptPart `json:"parts"`
	At    string     `json:"at"`
}

type keptPart struct {
	Repo    string   `json:"repo"`
	Folder  string   `json:"folder"`
	RepoDir string   `json:"repo_dir"` // relative to the job directory
	WorkDir string   `json:"work_dir"`
	BaseSHA string   `json:"base_sha"`
	Start   string   `json:"start"`
	Existed bool     `json:"existed"`
	Placed  []string `json:"placed,omitempty"`
}

// checkoutDirs are what a build's checkouts are made of, in its job
// directory: its own repo's, or a workspace's.
var checkoutDirs = []string{"repo", "repos", "work"}

func keptDir(jobDir string) string { return filepath.Join(jobDir, "kept") }

// stopSummary says how the run stopped.
func stopSummary(reason string, turns, maxTurns int, budget float64) string {
	if reason == "budget" {
		return fmt.Sprintf("Stopped: it reached its $%.2f budget before it finished.", budget)
	}
	if turns == 0 {
		turns = maxTurns
	}
	return fmt.Sprintf("Stopped: it ran out of turns (%d of %d) before it finished.", turns, maxTurns)
}

// stopped ends a build that ran out of turns or budget: its work is
// kept and the item waits for a person.
func (p *Pipeline) stopped(ctx context.Context, j *store.Job, it *store.WorkItem, run agentRun, parts []*part, maxTurns int) error {
	dir := p.jobDir(j.ID)
	rec := StopRecord{Reason: run.res.Stopped, Turns: run.res.Turns, MaxTurns: maxTurns, CostUSD: run.cost, BudgetUSD: j.BudgetUSD, At: store.Now()}
	rec.Summary = stopSummary(rec.Reason, rec.Turns, maxTurns, j.BudgetUSD)
	if b, err := harness.ReadJobFile(filepath.Join(dir, "out"), harness.ProgressFile); err == nil {
		rec.Note = truncate(strings.TrimSpace(scrub(string(b), p.Secrets[j.Harness])), 8000)
	}
	diff, err := p.keepCheckout(ctx, j, parts)
	if err != nil {
		rec.KeptError = err.Error()
		p.log().Warn("couldn't keep a stopped build's checkout", "job", j.ID, "err", err)
	} else {
		rec.Kept = true
		p.warn(os.WriteFile(filepath.Join(dir, "saved.patch"), []byte(truncate(scrub(diff, p.Secrets[j.Harness]), 200_000)), 0o644), "save patch")
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	p.warn(os.WriteFile(filepath.Join(dir, "stop.json"), b, 0o644), "save stop record")
	p.finish(ctx, j, run, state.Failed, "", rec.Summary+map[bool]string{true: " Its work is kept.", false: ""}[rec.Kept])
	return p.park(ctx, it, stopReasonText(j, rec))
}

// stopReasonText is why a stopped build's item waits: what happened and
// what each choice does.
func stopReasonText(j *store.Job, rec StopRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Build job %d (%s) %s", j.ID, j.Model, strings.ToLower(rec.Summary[:1])+rec.Summary[1:])
	if !rec.Kept {
		b.WriteString(" Its work couldn't be kept, so it can only start over. Retry starts it from scratch; Tier up starts it from scratch on the next model. Or leave it parked.")
		return b.String()
	}
	b.WriteString(" Its work is kept on the factory's machine (nothing was pushed), so it doesn't restart on its own: running out again is likely without a change. " +
		"Continue picks up where it stopped, in the same checkout and session, with more turns or budget and a note from you (for example, to narrow the scope). " +
		"Retry starts it from scratch; Tier up starts it from scratch on the next model. Or leave it parked.")
	return b.String()
}

// keepCheckout keeps a stopped build's checkouts for a continue, and
// returns its work against the base branch. The builder's tree is read
// as files into the daemon's repository, as Ingest always does, for the
// diff; the branch then moves back so the next build makes one commit.
func (p *Pipeline) keepCheckout(ctx context.Context, j *store.Job, parts []*part) (string, error) {
	dir := p.jobDir(j.ID)
	var diff strings.Builder
	k := keptCheckout{Job: j.ID, At: store.Now()}
	for _, pt := range parts {
		if err := realDir(pt.ws.Work); err != nil {
			return "", err
		}
		g := p.git(pt.repo)
		head, changed, err := g.Ingest(ctx, pt.ws, "factory: work in progress")
		if err != nil {
			return "", err
		}
		if changed {
			base, err := g.MergeBase(ctx, pt.ws.Repo, pt.ws.BaseSHA, head)
			if err != nil {
				return "", err
			}
			var d string
			if pt.folder == "" {
				d, err = g.Diff(ctx, pt.ws.Repo, base, head)
			} else {
				d, err = g.DiffIn(ctx, pt.ws.Repo, base, head, pt.folder)
			}
			if err != nil {
				return "", err
			}
			if d != "" {
				diff.WriteString(d + "\n")
			}
			if err := g.Unstep(ctx, pt.ws.Repo, pt.ws.Start); err != nil {
				return "", err
			}
		}
		rel := func(path string) string {
			r, err := filepath.Rel(dir, path)
			if err != nil {
				return path
			}
			return r
		}
		k.Parts = append(k.Parts, keptPart{Repo: pt.repo, Folder: pt.folder, RepoDir: rel(pt.ws.Repo), WorkDir: rel(pt.ws.Work),
			BaseSHA: pt.ws.BaseSHA, Start: pt.ws.Start, Existed: pt.ws.Existed, Placed: pt.ws.Placed})
	}
	kd := keptDir(dir)
	if err := os.MkdirAll(kd, 0o755); err != nil {
		return "", err
	}
	for _, d := range checkoutDirs {
		if _, err := os.Lstat(filepath.Join(dir, d)); err != nil {
			continue
		}
		if err := os.Rename(filepath.Join(dir, d), filepath.Join(kd, d)); err != nil {
			_ = os.RemoveAll(kd)
			return "", err
		}
	}
	b, _ := json.Marshal(k)
	if err := os.WriteFile(filepath.Join(kd, "kept.json"), b, 0o644); err != nil {
		_ = os.RemoveAll(kd)
		return "", err
	}
	return diff.String(), nil
}

// readKept reads job from's kept checkout, or an error when there is none.
func (p *Pipeline) readKept(from int64) (*keptCheckout, error) {
	kd := keptDir(p.jobDir(from))
	if fi, err := os.Lstat(kd); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("job %d kept no work", from)
	}
	b, err := os.ReadFile(filepath.Join(kd, "kept.json"))
	if err != nil {
		return nil, fmt.Errorf("job %d kept no work", from)
	}
	var k keptCheckout
	if err := json.Unmarshal(b, &k); err != nil || len(k.Parts) == 0 {
		return nil, fmt.Errorf("job %d's kept work can't be read", from)
	}
	return &k, nil
}

// takeKept moves job from's kept checkouts into dir, for the build that
// continues it, and returns them as parts.
func (p *Pipeline) takeKept(dir string, from int64) ([]*part, error) {
	k, err := p.readKept(from)
	if err != nil {
		return nil, err
	}
	kd := keptDir(p.jobDir(from))
	for _, d := range checkoutDirs {
		if _, err := os.Lstat(filepath.Join(kd, d)); err != nil {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, d))
		if err := os.Rename(filepath.Join(kd, d), filepath.Join(dir, d)); err != nil {
			return nil, err
		}
	}
	_ = os.RemoveAll(kd)
	var parts []*part
	for _, kp := range k.Parts {
		in := func(rel string) (string, error) {
			if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
				return "", fmt.Errorf("job %d's kept work names %q", from, rel)
			}
			return filepath.Join(dir, rel), nil
		}
		repo, err := in(kp.RepoDir)
		if err != nil {
			return nil, err
		}
		work, err := in(kp.WorkDir)
		if err != nil {
			return nil, err
		}
		if err := realDir(work); err != nil {
			return nil, err
		}
		parts = append(parts, &part{repo: kp.Repo, folder: kp.Folder, ws: &Workspace{Repo: repo, Work: work, BaseSHA: kp.BaseSHA,
			Start: kp.Start, Existed: kp.Existed, Placed: kp.Placed}})
	}
	return parts, nil
}

// sweepKept removes kept checkouts no one can continue any more: the
// item is done or gone, a later build started over, or it's been
// keepStoppedFor.
func (p *Pipeline) sweepKept(ctx context.Context) {
	dirs, _ := filepath.Glob(filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "jobs", "*", "kept"))
	for _, kd := range dirs {
		id, err := strconv.ParseInt(filepath.Base(filepath.Dir(kd)), 10, 64)
		if err != nil {
			continue
		}
		if why := p.keptStale(ctx, id); why != "" {
			p.log().Info("removing a stopped build's kept checkout", "job", id, "why", why)
			p.warn(os.RemoveAll(kd), "remove kept checkout")
		}
	}
}

// keptStale says why job id's kept checkout should go, or "".
func (p *Pipeline) keptStale(ctx context.Context, id int64) string {
	j, err := p.Store.GetJob(ctx, id)
	if err != nil || j.ItemID == nil {
		return "its job is gone"
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if err != nil || it.Stage == state.Done {
		return "its item is done"
	}
	if k, err := p.readKept(id); err == nil {
		if at, err := time.Parse(store.TimeFormat, k.At); err == nil && time.Since(at) > keepStoppedFor {
			return "no one continued it in " + roundDur(keepStoppedFor)
		}
	}
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if err != nil {
		return ""
	}
	for _, later := range jobs {
		if later.ID <= id {
			continue
		}
		var in ImplementInput
		_ = jsonUnmarshal(later.Input, &in)
		if in.Continue != nil && in.Continue.From == id && !later.State.Terminal() {
			continue // about to take it
		}
		return fmt.Sprintf("job %d started over", later.ID)
	}
	return ""
}

// Continue picks up a build that stopped on its turn or dollar cap, in
// the checkout and session it kept. extra adds turns and budget (0.5 is
// half as much again), up to MaxContinueExtra; note goes to the builder.
func (p *Pipeline) Continue(ctx context.Context, jobID int64, extra float64, note string) error {
	p.act.Lock()
	defer p.act.Unlock()
	if extra < 0 || extra > MaxContinueExtra {
		return fmt.Errorf("extra must be between 0 and %g (up to %d%% more)", MaxContinueExtra, int(MaxContinueExtra*100))
	}
	note = strings.TrimSpace(note)
	if len(note) > 4000 {
		return errors.New("the note is too long; keep it under 4000 characters")
	}
	j, it, in, err := p.canContinue(ctx, jobID)
	if err != nil {
		return err
	}
	if held, err := p.holdForPlan(ctx, it, "human"); err != nil {
		return err
	} else if held {
		return fmt.Errorf("item %d needs its plan approved before it's built", it.ID)
	}
	reason := "continue"
	if extra > 0 {
		reason = fmt.Sprintf("continue with %d%% more turns and budget", int(extra*100+0.5))
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, JobID: &j.ID, Type: "continue", Reason: reason}), "AddEvent")
	if err := p.toImplementing(ctx, it, "human", "continue the stopped build"); err != nil {
		return err
	}
	in.Continue = &ContinueInput{From: j.ID, Note: note, Extra: extra}
	ok, err := p.enqueueImplement(ctx, it, j.Attempt, in)
	if err == nil && !ok {
		return fmt.Errorf("no implement route at attempt %d", j.Attempt)
	}
	return err
}

// canContinue checks that job jobID is a stopped build whose work is
// kept and whose item is free to build, and returns its input.
func (p *Pipeline) canContinue(ctx context.Context, jobID int64) (*store.Job, *store.WorkItem, ImplementInput, error) {
	var in ImplementInput
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, in, err
	}
	if j.Kind != "implement" || j.ItemID == nil {
		return nil, nil, in, errors.New("only a build that stopped on its turn or dollar cap can continue")
	}
	if !j.State.Terminal() {
		return nil, nil, in, fmt.Errorf("job %d is still %s", jobID, j.State)
	}
	if _, err := p.readKept(jobID); err != nil {
		return nil, nil, in, errors.New("this build kept no work to continue: only one that ran out of turns or budget does, until the item moves on")
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if err != nil {
		return nil, nil, in, err
	}
	if it.Stage == state.Done || it.Stage == state.AwaitingHuman {
		return nil, nil, in, fmt.Errorf("item is %s", it.Stage)
	}
	if active, err := p.activeJob(ctx, it.ID); err != nil || active != nil {
		if active != nil {
			return nil, nil, in, fmt.Errorf("item already has job %d %s", active.ID, active.State)
		}
		return nil, nil, in, err
	}
	if p.estimateNeedsOK(ctx, it.ID) {
		return nil, nil, in, fmt.Errorf("item %d needs an OK on its cost estimate first", it.ID)
	}
	if j.Input != "" {
		if err := jsonUnmarshal(j.Input, &in); err != nil {
			return nil, nil, in, err
		}
	}
	in.Continue = nil
	in.PreviousFailure = ""
	return j, it, in, nil
}

// continueTurns is a continue's turn cap.
func (p *Pipeline) continueTurns(c *ContinueInput) int {
	t := p.Cfg.Budgets.MaxTurns
	if c != nil && c.Extra > 0 {
		t = int(float64(t)*(1+c.Extra) + 0.5)
	}
	return t
}

// readStop reads job id's stop record, or nil.
func (p *Pipeline) readStop(id int64) *StopRecord {
	b, err := os.ReadFile(filepath.Join(p.jobDir(id), "stop.json"))
	if err != nil {
		return nil
	}
	var r StopRecord
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

// continueText is what a continuing builder is told: in a resumed
// session all of it, as the task is already there; in a new one, after
// the task.
func (p *Pipeline) continueText(c *ContinueInput, kept bool, turns int, budget float64, outDir string) string {
	var b strings.Builder
	b.WriteString("## Continue where you stopped\n\n")
	rec := p.readStop(c.From)
	how := "stopped before it finished"
	if rec != nil {
		how = strings.TrimSuffix(strings.ToLower(rec.Summary[:1])+rec.Summary[1:], ".")
		how = strings.TrimPrefix(how, "stopped: it ")
		how = "stopped: it " + how
	}
	fmt.Fprintf(&b, "Your last run on this issue (job %d) %s.", c.From, how)
	if kept {
		b.WriteString(" Its work is still here, exactly as it left it, committed or not: check `git status`, `git diff` and `git log` to see where it got to, and carry on from there rather than starting over.")
	} else {
		b.WriteString(" Its checkout couldn't be kept, so you start from a fresh one; use what it learned.")
	}
	fmt.Fprintf(&b, " You have %d turns and $%.2f for this run. Finish the task as first instructed: run the checks, commit, and write `%s/verdict.json`.\n", turns, budget, outDir)
	if rec != nil && rec.Note != "" {
		fence := fenceFor(rec.Note)
		fmt.Fprintf(&b, "\nBefore it stopped, it wrote this note on what was done and what was left:\n\n%s\n%s\n%s\n", fence, rec.Note, fence)
	}
	if c.Note != "" {
		fmt.Fprintf(&b, "\nA person read where it stopped and says:\n\n%s\n", c.Note)
	}
	return b.String()
}

// fenceFor is a code fence s cannot close.
func fenceFor(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

// Steer sends a running build a message from a person, which it reads at
// its next step.
func (p *Pipeline) Steer(ctx context.Context, jobID int64, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("write a message first")
	}
	if len(text) > 4000 {
		return errors.New("the message is too long; keep it under 4000 characters")
	}
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.State != state.Running {
		return fmt.Errorf("job %d is %s, not running", jobID, j.State)
	}
	if why := steerUnsupported(j); why != "" {
		return errors.New(why)
	}
	v, ok := p.steer.Load(jobID)
	if !ok {
		return errors.New("the builder isn't running its agent right now (it may be setting up or running the checks); try again in a moment")
	}
	errc := make(chan error, 1)
	select {
	case v.(chan harness.SteerMsg) <- harness.SteerMsg{Text: text, Err: errc}:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("the agent didn't take the message; it may be finishing")
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("the agent didn't take the message; it may be finishing")
	}
}

// steerUnsupported says why job j can't take messages while it runs, or "".
func steerUnsupported(j *store.Job) string {
	switch {
	case j.Kind != "implement":
		return "only a running build takes messages"
	case j.Harness != "claude":
		return "Codex reads its whole prompt when it starts, so it can't take messages while it runs"
	}
	return ""
}

// SteerUnsupported says why job j can't take messages, or "".
func SteerUnsupported(j *store.Job) string { return steerUnsupported(j) }

// Choice is one thing a person can do with a job that ended, said
// plainly, with the model it runs on.
type Choice struct {
	Action    string // continue, retry, tier_up or park
	Label     string
	Model     string // harness/model it runs, "" for park
	Explain   string // what it does
	Available bool
	Why       string // why it isn't available
}

// Choices are what a person can do next with job jobID and its item.
func (p *Pipeline) Choices(ctx context.Context, jobID int64) []Choice {
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil || j.ItemID == nil {
		return nil
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if err != nil {
		return nil
	}
	var out []Choice
	if j.Kind == "implement" {
		c := Choice{Action: "continue", Label: "Continue", Model: j.Harness + "/" + j.ModelLabel()}
		if _, _, in, err := p.canContinue(ctx, jobID); err != nil {
			c.Why = err.Error()
			if _, kerr := p.readKept(jobID); kerr != nil {
				c = Choice{} // nothing kept: not a choice at all
			}
		} else {
			c.Available = true
			size := it.Complexity
			if in.Size != "" {
				size = in.Size
			}
			budget := p.Cfg.FastBudget(p.Cfg.JobBudget("implement", size), j.Fast)
			session := "a new session told where the last one stopped"
			if _, docker := p.Runner.(*runner.Docker); docker && j.Harness == "claude" {
				session = "the same Claude session"
			}
			c.Explain = fmt.Sprintf("Picks up where it stopped, in the checkout it kept and %s, on %s, with a fresh %d turns and $%.2f; you can add up to %d%% more. "+
				"A note you write goes to it first, for example to narrow the scope. Nothing is pushed until its checks pass.",
				session, c.Model, p.Cfg.Budgets.MaxTurns, budget, int(MaxContinueExtra*100))
		}
		if c.Action != "" {
			out = append(out, c)
		}
	}
	for _, up := range []bool{false, true} {
		c := Choice{Action: "retry", Label: "Retry"}
		if up {
			c.Action, c.Label = "tier_up", "Tier up"
		}
		if !up || j.Kind == "implement" {
			rp, err := p.planRetry(ctx, "human", jobID, up)
			if err != nil {
				c.Why = err.Error()
			} else {
				c.Available = true
				c.Model = rp.route.Harness + "/" + rp.route.Model
				c.Explain = p.retryExplain(rp, j)
			}
			out = append(out, c)
		}
	}
	park := Choice{Action: "park", Label: "Park", Available: it.Stage != state.Parked && it.Stage != state.Done,
		Explain: "Stops work on the item: cancels its running job, if any, and takes it off the queue. Nothing is lost. " +
			"To bring it back, press Retry (or Continue) here, or add the " + LabelRetry + " label on the issue to send it back through triage."}
	switch it.Stage {
	case state.Parked:
		park.Why = "It's parked now: nothing runs until you choose. Leaving it parked is fine; bring it back with the buttons here, or add the " + LabelRetry + " label on the issue."
	case state.Done:
		park.Why = "The item is done."
	}
	return append(out, park)
}

// retryExplain says what a retry would do.
func (p *Pipeline) retryExplain(rp *retryPlan, j *store.Job) string {
	model := rp.route.Harness + "/" + rp.route.Model
	switch j.Kind {
	case "triage":
		return "Runs triage again on " + model + ", which reads the issue as it is now."
	case "review", "critic":
		return "Runs the review again on " + model + ", on the same commit."
	}
	budget := p.Cfg.FastBudget(p.Cfg.JobBudget("implement", rp.size), rp.route.FastFor(rp.route.Harness, rp.route.Model))
	start := "a fresh checkout"
	if rp.in.FixRound {
		start = "a fresh checkout of the branch the reviewer saw"
	}
	if rp.tier {
		was := j.Model
		return fmt.Sprintf("Starts the build over from scratch on %s, one model tier up (it was %s), with %d turns and $%.2f. Anything this job did is dropped.",
			start, was, p.Cfg.Budgets.MaxTurns, budget) + " It runs on " + model + "."
	}
	return fmt.Sprintf("Starts the build over from scratch on %s, on %s, with %d turns and $%.2f. Anything this job did is dropped.",
		start, model, p.Cfg.Budgets.MaxTurns, budget)
}
