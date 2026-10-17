package harness

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"

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

func (claudeAdapter) command(req RunRequest, p runner.Paths) ([]string, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("claude: model is required")
	}
	args := []string{"claude", "-p", "--model", req.Model, "--output-format", "stream-json", "--verbose"}
	if req.ReadOnly {
		// Read-only stages review content the builder wrote, so ignore the
		// checkout's own settings, hooks and MCP servers.
		args = append(args, "--permission-mode", "dontAsk", "--setting-sources", "user", "--strict-mcp-config",
			"--allowedTools", "Read,Glob,Grep")
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
	_ = p
	return args, nil
}

type claudeLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
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
	Usage            *struct {
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
