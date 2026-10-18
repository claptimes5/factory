package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// APIActivityEntry is one line of the live feed: something that happened,
// said the way a person would.
type APIActivityEntry struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	ItemID *int64 `json:"item_id"`
	JobID  *int64 `json:"job_id"`
	Actor  string `json:"actor"` // daemon, orchestrator, human, ...
	Ref    string `json:"ref"`   // the item's issue, as on its card; "" for none
	Text   string `json:"text"`  // follows the ref
	Tone   string `json:"tone"`  // live, needs_you, done, bad or info
}

// APIActivity is the live feed, newest first.
type APIActivity struct {
	Entries []APIActivityEntry `json:"entries"`
}

func (s *Server) apiActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo := r.URL.Query().Get("repo")
	evs, err := s.Store.RecentEvents(ctx, 500)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := APIActivity{Entries: []APIActivityEntry{}}
	// Looked up once each; a miss is remembered as nil.
	items := map[int64]*store.WorkItem{}
	jobs := map[int64]*store.Job{}
	for _, e := range evs {
		var it *store.WorkItem
		if e.ItemID != nil {
			var seen bool
			if it, seen = items[*e.ItemID]; !seen {
				it, _ = s.Store.GetItem(ctx, *e.ItemID)
				items[*e.ItemID] = it
			}
			if it == nil {
				continue
			}
		}
		// A repo's feed keeps its own items and the factory-wide lines.
		if repo != "" && it != nil && it.Repo != repo {
			continue
		}
		var j *store.Job
		if e.JobID != nil {
			var seen bool
			if j, seen = jobs[*e.JobID]; !seen {
				j, _ = s.Store.GetJob(ctx, *e.JobID)
				jobs[*e.JobID] = j
			}
		}
		text, tone := describe(e, it, j)
		if text == "" {
			continue
		}
		a := APIActivityEntry{ID: e.ID, At: e.At, ItemID: e.ItemID, JobID: e.JobID, Actor: e.Actor, Text: text, Tone: tone}
		if it != nil {
			a.Ref = s.ref(it.Repo, it.IssueNumber)
		}
		out.Entries = append(out.Entries, a)
		if len(out.Entries) == 50 {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// describe says what an event means for the feed, or "" for the
// bookkeeping no one watching needs.
func describe(e store.Event, it *store.WorkItem, j *store.Job) (text, tone string) {
	if e.Actor == "orchestrator" && e.Reason != "" {
		return trim(e.Reason, 200), "info"
	}
	switch e.Type {
	case "item_created":
		return "came in", "info"
	case "stage":
		var d struct{ To string }
		_ = json.Unmarshal([]byte(e.Data), &d)
		switch state.Stage(d.To) {
		case state.Ready:
			return "is ready to build", "info"
		case state.Reviewing:
			return "passed its gates; review started", "live"
		case state.AwaitingHuman:
			if prs, ok := strings.CutPrefix(e.Reason, "draft PRs "); ok {
				return "PRs " + strings.TrimSuffix(prs, " ready") + " are ready for you", "needs_you"
			}
			if it != nil && it.PRNumber > 0 {
				return fmt.Sprintf("PR #%d is ready for you", it.PRNumber), "needs_you"
			}
			return "is ready for you", "needs_you"
		case state.Parked:
			return withReason("parked", e.Reason), "needs_you"
		case state.Blocked:
			return withReason("blocked", e.Reason), "bad"
		case state.Done:
			return "is done", "done"
		case state.Epic:
			return "became an epic", "info"
		}
	case "job_claimed":
		if j == nil {
			return "", ""
		}
		switch j.Kind {
		case "implement":
			if j.Attempt > 1 {
				return fmt.Sprintf("started building with %s, attempt %d", j.ModelLabel(), j.Attempt), "live"
			}
			return "started building with " + j.ModelLabel(), "live"
		case "review":
			return j.Harness + " started reviewing", "live"
		case "critic":
			return "a second critic started reviewing", "live"
		case "triage":
			return "triage started", "live"
		}
		return j.Kind + " started", "live"
	case "job_finished":
		var d struct{ State string }
		_ = json.Unmarshal([]byte(e.Data), &d)
		kind := "job"
		if j != nil {
			kind = j.Kind
		}
		switch state.JobState(d.State) {
		case state.Failed, state.TimedOut:
			return withReason(kind+" failed", e.Reason), "bad"
		case state.Cancelled:
			return kind + " was cancelled", "info"
		}
	case "retry":
		return withReason("retried", e.Reason), "info"
	case "risk_raised":
		return withReason("risk raised", e.Reason), "info"
	case "budget_exceeded":
		return "went over its budget", "bad"
	case "auto_merge":
		return withReason("set to merge itself once its checks pass", e.Reason), "info"
	case "pr_merged":
		return "merged", "done"
	case "pr_closed":
		return "PR closed without merging", "bad"
	}
	return "", ""
}

func withReason(what, reason string) string {
	if reason = strings.TrimSpace(reason); reason == "" || reason == "seeded" {
		return what
	}
	return what + ": " + trim(reason, 200)
}

// liveActivity is what a running job is doing, from the tail of its log:
// "editing parser/parser.go", "running go test ./...". "" when the log
// says nothing yet. It reads back from the end in growing windows, since
// one line (a long test run's output) can be bigger than a window.
func liveActivity(logPath string) string {
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	for tail := int64(64 << 10); ; tail *= 4 {
		start := max(0, st.Size()-tail)
		b := make([]byte, st.Size()-start)
		if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
			return ""
		}
		lines := strings.Split(string(b), "\n")
		if start > 0 {
			lines = lines[1:] // the first is cut
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if a := lineActivity(lines[i]); a != "" {
				return a
			}
		}
		if start == 0 || tail >= 4<<20 {
			return ""
		}
	}
}

func lineActivity(line string) string {
	var v struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					FilePath string `json:"file_path"`
					Command  string `json:"command"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &v) != nil {
		return ""
	}
	if v.Item.Type == "command_execution" && v.Item.Command != "" {
		return "running " + firstLine(v.Item.Command)
	}
	c := v.Message.Content
	for i := len(c) - 1; i >= 0; i-- {
		if c[i].Type != "tool_use" {
			continue
		}
		in := c[i].Input
		switch c[i].Name {
		case "Edit", "MultiEdit", "Write", "NotebookEdit":
			return "editing " + shortPath(in.FilePath)
		case "Read":
			return "reading " + shortPath(in.FilePath)
		case "Bash":
			return "running " + firstLine(in.Command)
		case "Grep", "Glob":
			return "searching the code"
		case "WebFetch", "WebSearch":
			return "reading the web"
		case "Task", "Agent":
			return "handing a part to a subagent"
		case "TodoWrite":
			return "planning"
		}
		return "using " + c[i].Name
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return trim(s, 60)
}

// shortPath keeps the last two parts of a path, which say which file.
func shortPath(p string) string {
	if p == "" {
		return "a file"
	}
	dir, file := path.Split(path.Clean(p))
	if parent := path.Base(dir); dir != "" && parent != "/" && parent != "." {
		return parent + "/" + file
	}
	return file
}

// cardProgress fills a card's live job and how the latest implement
// attempt's gates went (none, if that attempt never reached them).
func (s *Server) cardProgress(ctx context.Context, c *APICard, jobs []*store.Job) {
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if j.Kind != "implement" {
			continue
		}
		c.Attempt = j.Attempt
		// A done item's gates are history; the board skips the lookup.
		if c.Stage != string(state.Done) {
			gs, _ := s.Store.GateResults(ctx, j.ID)
			for _, g := range gs {
				c.GatesTotal++
				if g.Status == "pass" {
					c.GatesPassed++
				}
			}
		}
		break
	}
	for _, j := range jobs {
		if j.State == state.Running {
			c.LiveKind, c.LiveSince = j.Kind, j.StartedAt
			c.Activity = liveActivity(j.LogPath)
		}
	}
}
