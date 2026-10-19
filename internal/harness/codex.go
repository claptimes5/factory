package harness

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// NewCodex returns the Codex CLI adapter:
//
//	codex exec --json --model <m> --sandbox <read-only|danger-full-access> [--output-schema f] -
//
// The prompt is sent on stdin. Read-only stages run in Codex's read-only
// sandbox. A builder in a container (the docker runner) runs with
// danger-full-access, as Claude's runs with bypassPermissions: it installs
// dependencies, reaches the job's services and writes its verdict outside
// the checkout, and the container with its egress allowlist is the
// boundary. On the host (the local runner) a builder keeps Codex's
// workspace-write sandbox, with the out directory added so the verdict can
// be written. Codex reports tokens, not dollars, so the caller prices the
// usage (see config.CostOf).
func NewCodex(r runner.Runner) Harness {
	return &cliHarness{name: "codex", a: codexAdapter{}, runner: r}
}

type codexAdapter struct {
	plan *codexPlan // nil with an API key
}

func (a codexAdapter) skillsPath(p runner.Paths) string {
	if a.plan.withFile() {
		return path.Join(a.plan.home(p), "skills")
	}
	return path.Join(p.Home, ".codex", "skills")
}

func (a codexAdapter) command(req RunRequest, p runner.Paths) ([]string, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("codex: model is required")
	}
	sandbox := "read-only"
	switch {
	case req.ReadOnly:
	case req.Contained:
		sandbox = "danger-full-access"
	default:
		sandbox = "workspace-write"
	}
	args := []string{"codex", "exec", "--json", "--model", req.Model, "--sandbox", sandbox, "--skip-git-repo-check", "--cd", p.Work}
	if sandbox == "workspace-write" {
		args = append(args, "--add-dir", p.Out)
	}
	mcp, err := codexMCP(req.MCPConfig)
	if err != nil {
		return nil, err
	}
	args = append(args, mcp...)
	if req.Schema != "" {
		args = append(args, "--output-schema", path.Join(p.Out, "schema.json"))
	}
	if req.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	for _, im := range req.Images {
		args = append(args, "--image="+im) // one value each: a bare -i would take the - after it
	}
	args = append(args, "-")
	if req.CaptureLimits {
		// Codex writes its usage-limit windows only to its session file:
		// keep the last reading, and Codex's exit code.
		script := `codex "$@"; s=$?; f=$(ls -t "${CODEX_HOME:-$HOME/.codex}"/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -n 1); ` +
			`if [ -n "$f" ]; then grep -E '"rate_limits": ?\{' "$f" | tail -n 1 > ` + path.Join(p.Out, codexLimitsFile) + `; fi; exit $s`
		args = append([]string{"sh", "-c", script, "sh"}, args[1:]...)
	}
	return args, nil
}

const codexLimitsFile = "codex-limits.json"

// codexMCP turns the MCP servers of a `claude --mcp-config` JSON (the
// orchestrator's stdio bridge to the daemon) into Codex's -c overrides of
// mcp_servers. Values are TOML: a JSON string or array of strings is one.
func codexMCP(cfg string) ([]string, error) {
	if cfg == "" {
		return nil, nil
	}
	var c struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		return nil, fmt.Errorf("codex: mcp config: %w", err)
	}
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var args []string
	for _, n := range names {
		s := c.Servers[n]
		if !mcpName.MatchString(n) || s.Command == "" {
			return nil, fmt.Errorf("codex: mcp server %q: needs a plain name and a command", n)
		}
		cmd, _ := json.Marshal(s.Command)
		list, _ := json.Marshal(append([]string{}, s.Args...))
		// exec never asks, so a tool call needing approval would fail:
		// the daemon's tools are the only ones, and each checks its call.
		args = append(args, "-c", "mcp_servers."+n+".command="+string(cmd), "-c", "mcp_servers."+n+".args="+string(list),
			"-c", "mcp_servers."+n+`.default_tools_approval_mode="approve"`)
	}
	return args, nil
}

var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// limitsAfter reads the usage-limit windows the wrapper saved.
func (codexAdapter) limitsAfter(outDir string, res *Result) {
	// The job could have put anything there.
	b, err := readJobFile(outDir, codexLimitsFile)
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
		// An mcp_tool_call's server, tool, status and, when the call
		// couldn't be made, error.
		Server string `json:"server"`
		Tool   string `json:"tool"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
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
		case "mcp_tool_call":
			b, _ := json.Marshal(l.Item.Server + "." + l.Item.Tool)
			emit(Event{Kind: "tool_call", Payload: b})
			// A call Codex couldn't make (refused, or the server gone) fails
			// the run: Codex itself exits 0 and reports what it managed.
			if l.Item.Status == "failed" && l.Item.Error != nil && !res.IsError {
				res.IsError = true
				res.Error = fmt.Sprintf("codex: MCP tool call %s.%s failed: %s", l.Item.Server, l.Item.Tool, l.Item.Error.Message)
				emit(Event{Kind: "error", Payload: line})
			}
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
