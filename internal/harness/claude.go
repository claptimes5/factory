package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// NewClaude returns the Claude Code adapter:
//
//	claude -p --model <m> --output-format stream-json --verbose --permission-mode <mode> ...
//
// The prompt is sent on stdin. Builders run with bypassPermissions because
// the container is the sandbox; read-only stages may only read.
func NewClaude(r runner.Runner) Harness {
	return &cliHarness{name: "claude", a: claudeAdapter{}, runner: r}
}

type claudeAdapter struct{}

func (claudeAdapter) skillsPath(p runner.Paths) string { return path.Join(p.Home, ".claude", "skills") }

func (claudeAdapter) sessionsPath(p runner.Paths) string {
	return path.Join(p.Home, ".claude", "projects")
}

func (claudeAdapter) command(req RunRequest, p runner.Paths) ([]string, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("claude: model is required")
	}
	args := []string{"claude", "-p", "--model", req.Model, "--output-format", "stream-json", "--verbose"}
	if req.ReadOnly {
		// Read-only stages review content the builder wrote, so ignore the
		// checkout's own settings, hooks and MCP servers.
		tools := append([]string{"Read", "Glob", "Grep"}, req.Tools...)
		args = append(args, "--permission-mode", "dontAsk", "--setting-sources", "user", "--strict-mcp-config",
			"--allowedTools", strings.Join(tools, ","))
		if req.MCPConfig != "" {
			args = append(args, "--mcp-config", req.MCPConfig)
		}
	} else {
		args = append(args, "--permission-mode", "bypassPermissions", "--disallowedTools", "WebSearch")
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if req.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.BudgetUSD, 'f', 2, 64))
	}
	if req.Schema != "" && req.ReadOnly {
		args = append(args, "--json-schema", req.Schema)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	settings := map[string]any{}
	if req.Fast {
		settings["fastMode"] = true
	}
	if wrapUpAt(req) > 0 {
		settings["hooks"] = map[string]any{"PostToolUse": []any{map[string]any{"matcher": "*",
			"hooks": []any{map[string]any{"type": "command", "command": "sh " + shellQuote(path.Join(p.Out, wrapUpScript))}}}}}
	}
	if len(settings) > 0 {
		b, _ := json.Marshal(settings)
		args = append(args, "--settings", string(b))
	}
	if req.Steer != nil {
		// The prompt and any later message arrive as JSON lines on stdin,
		// and each comes back on stdout once Claude takes it.
		args = append(args, "--input-format", "stream-json", "--replay-user-messages")
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	return args, nil
}

// Wrapping up: a builder that runs out of turns stops mid-task. A hook
// after each tool call counts them and, near the cap, tells it to commit
// what it has and write down what's left (ProgressFile), so a person can
// continue it. Calls run ahead of turns (one turn can make several), so
// the warning comes early rather than late. Claude can't see its spend,
// so there's no such warning for the dollar cap.

const wrapUpScript = ".factory-wrapup.sh"

// wrapUpAt is the tool call that brings the first warning, 0 for none.
func wrapUpAt(req RunRequest) int {
	if req.ReadOnly || req.MaxTurns < 8 {
		return 0
	}
	if req.MaxTurns >= 30 {
		return req.MaxTurns - 10
	}
	return req.MaxTurns * 2 / 3
}

// prepare writes the wrap-up hook into the out directory, which the
// container sees at p.Out.
func (claudeAdapter) prepare(_ context.Context, req RunRequest, p runner.Paths, _ *runner.Spec) (func(*Result), []string, error) {
	at := wrapUpAt(req)
	if at == 0 {
		return func(*Result) {}, nil, nil
	}
	note := path.Join(p.Out, ProgressFile)
	soon := fmt.Sprintf("The factory: you have about %d turns left of %d. Stop starting new work. Finish the step you are on, then commit what you have in each git repository you changed: inside that repository run `git add -A && git commit -m 'wip: <what>'` (a work-in-progress commit is fine; when the work folder holds several repositories, it is not one itself, so do this in each one you changed). Then write %s: a short note of what is done, what is left, and anything the next run should know. If the task is already done, finish it normally and write the verdict.", req.MaxTurns-at, req.MaxTurns, note)
	last := fmt.Sprintf("The factory: you are about to run out of turns. Commit your work now: in each git repository you changed, run `git add -A && git commit -m 'wip'` inside it. Then write %s with what is done and what is left. Do nothing else.", note)
	hook := func(msg string) string {
		b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": msg}})
		return string(b)
	}
	count := path.Join(p.Out, ".factory-turns")
	script := fmt.Sprintf(`#!/bin/sh
# Written by the factory: counts the builder's tool calls and, near its
# turn cap, tells it to wrap up.
cat >/dev/null
n=$(cat %[1]s 2>/dev/null)
case "$n" in ''|*[!0-9]*) n=0 ;; esac
n=$((n + 1))
echo "$n" > %[1]s
if [ "$n" -eq %[2]d ]; then
cat <<'FACTORY_EOF'
%[3]s
FACTORY_EOF
elif [ "$n" -eq %[4]d ]; then
cat <<'FACTORY_EOF'
%[5]s
FACTORY_EOF
fi
exit 0
`, shellQuote(count), at, hook(soon), max(at+1, req.MaxTurns-3), hook(last))
	if err := os.WriteFile(filepath.Join(req.OutDir, wrapUpScript), []byte(script), 0o755); err != nil {
		return nil, nil, err
	}
	return func(*Result) {}, nil, nil
}

// shellQuote quotes s for sh: a local run's paths may hold spaces or
// quotes.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (claudeAdapter) userLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	return append(b, '\n')
}

// observe: a replayed message is a user line with text rather than tool
// results; a result line ends the run.
func (claudeAdapter) observe(line []byte) (echo string, ok, done, stopped bool) {
	var l struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil {
		return "", false, false, false
	}
	switch l.Type {
	case "user":
		if l.Message == nil {
			return "", false, false, false
		}
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil {
			return s, true, false, false
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(l.Message.Content, &blocks)
		var text []string
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return "", false, false, false
			}
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		return strings.Join(text, ""), len(text) > 0, false, false
	case "result":
		return "", false, true, stopReason(l.Subtype) != ""
	}
	return "", false, false, false
}

// stopReason names the cap a result's subtype says the run hit, or "".
func stopReason(subtype string) string {
	switch {
	case subtype == "error_max_turns":
		return "turns"
	case strings.HasPrefix(subtype, "error_max_budget"):
		return "budget"
	}
	return ""
}

type claudeLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Subtype   string `json:"subtype"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Result           string          `json:"result"`
	IsError          bool            `json:"is_error"`
	TotalCostUSD     *float64        `json:"total_cost_usd"`
	NumTurns         int             `json:"num_turns"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	RateLimit        *struct {
		Status      string   `json:"status"`        // allowed, allowed_warning, rejected
		ResetsAt    int64    `json:"resetsAt"`      // Unix seconds
		Type        string   `json:"rateLimitType"` // five_hour, seven_day, ...
		Utilization *float64 `json:"utilization"`   // 0 to 1
	} `json:"rate_limit_info"`
	Usage *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (claudeAdapter) parse(line []byte, res *Result, emit func(Event)) {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	if l.SessionID != "" {
		res.SessionID = l.SessionID
	}
	switch l.Type {
	case "assistant":
		if l.Message == nil {
			return
		}
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				b, _ := json.Marshal(c.Text)
				emit(Event{Kind: "text", Payload: b})
			case "tool_use":
				b, _ := json.Marshal(c.Name)
				emit(Event{Kind: "tool_call", Payload: b})
			}
		}
	case "user":
		emit(Event{Kind: "tool_result", Payload: line})
	case "rate_limit_event":
		// Subscriptions only, and not in Claude Code's documented stream:
		// read it when it's there.
		if rl := l.RateLimit; rl != nil && rl.Type != "" {
			lim := Limit{Window: windowName(rl.Type)}
			switch {
			case rl.Utilization != nil && *rl.Utilization <= 1:
				lim.UsedPercent = *rl.Utilization * 100
			case rl.Utilization != nil:
				lim.UsedPercent = *rl.Utilization
			case rl.Status == "rejected":
				lim.UsedPercent = 100
			default:
				return // no reading
			}
			if rl.Status == "rejected" {
				lim.UsedPercent = 100
			}
			if rl.ResetsAt > 0 {
				lim.ResetsAt = time.Unix(rl.ResetsAt, 0).UTC()
			}
			res.setLimit(lim)
		}
	case "result":
		res.Final = l.Result
		if len(l.StructuredOutput) > 0 && string(l.StructuredOutput) != "null" {
			res.Final = string(l.StructuredOutput)
		}
		res.Turns = l.NumTurns
		if l.TotalCostUSD != nil {
			res.CostUSD = *l.TotalCostUSD
			res.CostKnown = true
		}
		if l.Usage != nil {
			res.TokensIn = l.Usage.InputTokens + l.Usage.CacheReadInputTokens + l.Usage.CacheCreationInputTokens
			res.TokensOut = l.Usage.OutputTokens
		}
		res.Stopped = stopReason(l.Subtype)
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			res.IsError = true
			res.Error = "claude: " + l.Subtype
			if l.Result != "" {
				res.Error += ": " + l.Result
			}
		}
		emit(Event{Kind: "usage", Payload: line})
		emit(Event{Kind: "final", Payload: line})
	}
}

// windowName gives a limit window a short name: 5h, weekly or the CLI's.
func windowName(t string) string {
	switch t {
	case "five_hour":
		return "5h"
	case "seven_day", "weekly":
		return "weekly"
	}
	return t
}
