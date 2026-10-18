package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// NewCodex returns the Codex CLI adapter:
//
//	codex exec --json --model <m> --sandbox <read-only|workspace-write> [--output-schema f] -
//
// The prompt is sent on stdin. Codex reports tokens, not dollars, so the
// caller prices the usage (see config.CostOf).
func NewCodex(r runner.Runner) Harness {
	return &cliHarness{name: "codex", a: codexAdapter{}, runner: r}
}

type codexAdapter struct{}

func (codexAdapter) skillsPath(p runner.Paths) string { return path.Join(p.Home, ".codex", "skills") }

func (codexAdapter) command(req RunRequest, p runner.Paths) ([]string, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("codex: model is required")
	}
	sandbox := "workspace-write"
	if req.ReadOnly {
		sandbox = "read-only"
	}
	args := []string{"codex", "exec", "--json", "--model", req.Model, "--sandbox", sandbox, "--skip-git-repo-check", "--cd", p.Work}
	if req.Schema != "" {
		args = append(args, "--output-schema", path.Join(p.Out, "schema.json"))
	}
	if req.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	args = append(args, "-")
	if req.CaptureLimits {
		// Codex writes its usage-limit windows only to its session file:
		// keep the last reading, and Codex's exit code.
		script := `codex "$@"; s=$?; f=$(ls -t "${CODEX_HOME:-$HOME/.codex}"/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -n 1); ` +
			`if [ -n "$f" ]; then grep '"rate_limits"' "$f" | tail -n 1 > ` + path.Join(p.Out, codexLimitsFile) + `; fi; exit $s`
		args = append([]string{"sh", "-c", script, "sh"}, args[1:]...)
	}
	return args, nil
}

const codexLimitsFile = "codex-limits.json"

// limitsAfter reads the usage-limit windows the wrapper saved.
func (codexAdapter) limitsAfter(outDir string, res *Result) {
	b, err := os.ReadFile(filepath.Join(outDir, codexLimitsFile))
	if err != nil {
		return
	}
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			RateLimits map[string]*struct {
				UsedPercent     float64 `json:"used_percent"`
				WindowMinutes   int     `json:"window_minutes"`
				ResetsInSeconds int64   `json:"resets_in_seconds"`
				ResetsAt        int64   `json:"resets_at"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return
	}
	for _, key := range []string{"primary", "secondary"} {
		w := rec.Payload.RateLimits[key]
		if w == nil {
			continue
		}
		lim := Limit{Window: key, UsedPercent: w.UsedPercent}
		switch {
		case w.WindowMinutes > 0 && w.WindowMinutes <= 6*60:
			lim.Window = "5h"
		case w.WindowMinutes >= 6*24*60:
			lim.Window = "weekly"
		}
		switch {
		case w.ResetsAt > 0:
			lim.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
		case w.ResetsInSeconds > 0 && !rec.Timestamp.IsZero():
			lim.ResetsAt = rec.Timestamp.Add(time.Duration(w.ResetsInSeconds) * time.Second).UTC()
		}
		res.setLimit(lim)
	}
}

type codexLine struct {
	Type string `json:"type"`
	Item *struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func (codexAdapter) parse(line []byte, res *Result, emit func(Event)) {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	switch l.Type {
	case "item.completed":
		if l.Item == nil {
			return
		}
		switch l.Item.Type {
		case "agent_message":
			res.Final = l.Item.Text
			b, _ := json.Marshal(l.Item.Text)
			emit(Event{Kind: "text", Payload: b})
		case "command_execution":
			b, _ := json.Marshal(l.Item.Command)
			emit(Event{Kind: "tool_call", Payload: b})
		}
	case "turn.completed":
		if l.Usage != nil {
			res.TokensIn += l.Usage.InputTokens // cached included
			res.TokensCached += l.Usage.CachedInputTokens
			res.TokensOut += l.Usage.OutputTokens
		}
		res.Turns++
		emit(Event{Kind: "usage", Payload: line})
	case "turn.failed":
		res.IsError = true
		if l.Error != nil {
			res.Error = "codex: " + l.Error.Message
		}
		emit(Event{Kind: "error", Payload: line})
	case "error":
		res.IsError = true
		res.Error = "codex: " + l.Message
		emit(Event{Kind: "error", Payload: line})
	}
}
