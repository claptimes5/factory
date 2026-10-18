package harness

import (
	"encoding/json"
	"fmt"
	"path"
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
	if req.Fast {
		args = append(args, "--settings", `{"fastMode":true}`)
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	_ = p
	return args, nil
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
