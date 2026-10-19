package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The job page shows a run the way Claude Code does: the agent's own
// words as the thread, each tool call as one line ("Ran go test ./...",
// "Read parser/parser.go") that opens to its input and output, thinking
// tucked away, and how the run ended. entries turns one line of a run's
// log (Claude's or Codex's JSON stream, already scrubbed of secrets as it
// was written) into those pieces.

// APITranscriptEntry is one piece of a run's transcript.
type APITranscriptEntry struct {
	// Kind is init (the session started), text (the agent's words),
	// thinking, tool (a call), tool_result (its output), you (a message a
	// person sent it), outcome (how the run ended) or error.
	Kind string `json:"kind"`
	Text string `json:"text"`
	// A tool call's name, its id (which its result names too), what it
	// did in a line, and its whole input.
	Tool    string `json:"tool"`
	ToolID  string `json:"tool_id"`
	Summary string `json:"summary"`
	Input   string `json:"input"`
	// A tool result's output, and whether it was cut.
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	IsError   bool   `json:"is_error"`
	// init: the model and how many tools it has.
	Model string `json:"model"`
	Tools int    `json:"tools"`
	// outcome: the CLI's own word for it, turns and cost.
	Subtype string  `json:"subtype"`
	Turns   int     `json:"turns"`
	CostUSD float64 `json:"cost_usd"`
	// you: false when the agent had already finished.
	Delivered bool `json:"delivered"`
}

// maxOutput caps a tool's output in the stream; the raw log has it all.
const maxOutput = 20_000

func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return trimKeepLines(s, n), true
}

// trimKeepLines is trim without folding the lines.
func trimKeepLines(s string, n int) string {
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// entries structures one log line; nil for the bookkeeping no one reads.
func entries(line string) []APITranscriptEntry {
	var v map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &v) != nil {
		if s := strings.TrimSpace(line); s != "" {
			return []APITranscriptEntry{{Kind: "text", Text: s}}
		}
		return nil
	}
	var typ string
	_ = json.Unmarshal(v["type"], &typ)
	switch typ {
	case "factory_message":
		var m struct {
			Text      string `json:"text"`
			Delivered bool   `json:"delivered"`
		}
		_ = json.Unmarshal([]byte(line), &m)
		return []APITranscriptEntry{{Kind: "you", Text: m.Text, Delivered: m.Delivered}}
	case "system", "assistant", "user", "result":
		return claudeEntries(typ, line)
	}
	return codexEntries(typ, line)
}

func claudeEntries(typ, line string) []APITranscriptEntry {
	var l struct {
		Subtype string   `json:"subtype"`
		Model   string   `json:"model"`
		Tools   []string `json:"tools"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Result       string   `json:"result"`
		IsError      bool     `json:"is_error"`
		NumTurns     int      `json:"num_turns"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
	}
	if json.Unmarshal([]byte(line), &l) != nil {
		return nil
	}
	switch typ {
	case "system":
		if l.Subtype == "init" {
			return []APITranscriptEntry{{Kind: "init", Model: l.Model, Tools: len(l.Tools)}}
		}
		return nil
	case "result":
		e := APITranscriptEntry{Kind: "outcome", Subtype: l.Subtype, Turns: l.NumTurns, IsError: l.IsError || l.Subtype != "success"}
		if l.TotalCostUSD != nil {
			e.CostUSD = *l.TotalCostUSD
		}
		e.Summary = outcomeText(l.Subtype, l.NumTurns, e.CostUSD)
		e.Text, _ = clip(l.Result, maxOutput)
		return []APITranscriptEntry{e}
	}
	if l.Message == nil {
		return nil
	}
	var text string
	if json.Unmarshal(l.Message.Content, &text) == nil {
		// A message on the input coming back: the factory logged it already.
		return nil
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		UseID    string          `json:"tool_use_id"`
		Content  json.RawMessage `json:"content"`
		IsError  bool            `json:"is_error"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}
	var out []APITranscriptEntry
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, APITranscriptEntry{Kind: "text", Text: b.Text})
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				out = append(out, APITranscriptEntry{Kind: "thinking", Text: b.Thinking})
			}
		case "tool_use":
			out = append(out, claudeTool(b.ID, b.Name, b.Input))
		case "tool_result":
			o, cut := clip(resultText(b.Content), maxOutput)
			out = append(out, APITranscriptEntry{Kind: "tool_result", ToolID: b.UseID, Output: o, Truncated: cut, IsError: b.IsError})
		}
	}
	return out
}

// resultText is a tool result's content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// claudeTool is a Claude tool call, with what it did in a line.
func claudeTool(id, name string, raw json.RawMessage) APITranscriptEntry {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	e := APITranscriptEntry{Kind: "tool", Tool: name, ToolID: id}
	switch name {
	case "Bash":
		e.Summary = "Ran " + firstLine(str("command"))
		e.Input = str("command")
		if d := str("description"); d != "" {
			e.Input = "# " + d + "\n" + e.Input
		}
	case "Read":
		e.Summary = "Read " + workPath(str("file_path"))
	case "Edit", "MultiEdit":
		e.Summary = "Edited " + workPath(str("file_path"))
	case "Write":
		e.Summary = "Wrote " + workPath(str("file_path"))
	case "NotebookEdit":
		e.Summary = "Edited " + workPath(str("notebook_path"))
	case "Glob":
		e.Summary = "Looked for files matching " + str("pattern")
	case "Grep":
		e.Summary = fmt.Sprintf("Searched for %q", str("pattern"))
		if p := str("path"); p != "" {
			e.Summary += " in " + workPath(p)
		}
	case "TodoWrite":
		e.Summary = "Updated its to-do list"
		e.Input = todos(in["todos"])
	case "Task", "Agent":
		e.Summary = "Handed a part to a subagent: " + str("description")
	case "WebFetch":
		e.Summary = "Fetched " + str("url")
	case "WebSearch":
		e.Summary = "Searched the web for " + str("query")
	default:
		if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__"); ok && strings.HasPrefix(name, "mcp__") {
			e.Summary = "Called " + server + "." + tool
		} else {
			e.Summary = "Used " + name
		}
	}
	if e.Input == "" && len(raw) > 0 && string(raw) != "null" {
		e.Input = pretty(raw)
	}
	e.Input, _ = clip(e.Input, maxOutput)
	return e
}

// workPath shows a path in the checkout from the checkout's root (the
// container's /work), and any other by its last parts.
func workPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/work/"); ok && rest != "" {
		return rest
	}
	return shortPath(p)
}

// todos writes a to-do list as lines.
func todos(v any) string {
	list, _ := v.([]any)
	var b strings.Builder
	for _, t := range list {
		m, _ := t.(map[string]any)
		mark := "[ ]"
		switch m["status"] {
		case "completed":
			mark = "[x]"
		case "in_progress":
			mark = "[~]"
		}
		fmt.Fprintf(&b, "%s %v\n", mark, m["content"])
	}
	return strings.TrimRight(b.String(), "\n")
}

func pretty(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// outcomeText says in plain words how a run ended.
func outcomeText(subtype string, turns int, cost float64) string {
	tail := ""
	if turns > 0 {
		tail = fmt.Sprintf(" after %d turns", turns)
	}
	if cost > 0 {
		tail += fmt.Sprintf(" ($%.2f)", cost)
	}
	switch {
	case subtype == "success":
		return "Finished" + tail + "."
	case subtype == "error_max_turns":
		return fmt.Sprintf("Stopped: it ran out of turns (%d) before it finished.", turns)
	case strings.HasPrefix(subtype, "error_max_budget"):
		return fmt.Sprintf("Stopped: it reached its budget ($%.2f) before it finished.", cost)
	case subtype == "error_during_execution":
		return "Ended with an error" + tail + "."
	}
	return "Ended (" + subtype + ")" + tail + "."
}

func codexEntries(typ, line string) []APITranscriptEntry {
	var l struct {
		ThreadID string `json:"thread_id"`
		Item     *struct {
			ID               string `json:"id"`
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			ExitCode         *int   `json:"exit_code"`
			Status           string `json:"status"`
			Changes          []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
			Server    string          `json:"server"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Result    json.RawMessage `json:"result"`
			Error     *struct {
				Message string `json:"message"`
			} `json:"error"`
			Query string `json:"query"`
			Items []struct {
				Text      string `json:"text"`
				Completed bool   `json:"completed"`
			} `json:"items"`
		} `json:"item"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(line), &l) != nil {
		return nil
	}
	switch typ {
	case "thread.started":
		return []APITranscriptEntry{{Kind: "init", Model: "codex"}}
	case "turn.failed":
		msg := "the turn failed"
		if l.Error != nil {
			msg = l.Error.Message
		}
		return []APITranscriptEntry{{Kind: "error", Text: msg}}
	case "error":
		return []APITranscriptEntry{{Kind: "error", Text: l.Message}}
	case "item.started", "item.updated", "item.completed":
	default:
		return nil
	}
	it := l.Item
	if it == nil {
		return nil
	}
	done := typ == "item.completed"
	tool := func(name, summary, input string) APITranscriptEntry {
		in, _ := clip(input, maxOutput)
		return APITranscriptEntry{Kind: "tool", Tool: name, ToolID: it.ID, Summary: summary, Input: in}
	}
	var call APITranscriptEntry
	result := APITranscriptEntry{Kind: "tool_result", ToolID: it.ID}
	switch it.Type {
	case "agent_message":
		if done && strings.TrimSpace(it.Text) != "" {
			return []APITranscriptEntry{{Kind: "text", Text: it.Text}}
		}
		return nil
	case "reasoning":
		if done && strings.TrimSpace(it.Text) != "" {
			return []APITranscriptEntry{{Kind: "thinking", Text: it.Text}}
		}
		return nil
	case "command_execution":
		call = tool("Bash", "Ran "+firstLine(unwrapShell(it.Command)), it.Command)
		result.Output, result.Truncated = clip(it.AggregatedOutput, maxOutput)
		result.IsError = it.Status == "failed" || it.ExitCode != nil && *it.ExitCode != 0
	case "file_change":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Kind+" "+c.Path)
		}
		sum := fmt.Sprintf("Edited %d files", len(it.Changes))
		if len(it.Changes) == 1 {
			sum = "Edited " + workPath(it.Changes[0].Path)
		}
		call = tool("Edit", sum, strings.Join(paths, "\n"))
		result.IsError = it.Status == "failed"
	case "mcp_tool_call":
		call = tool(it.Server+"."+it.Tool, "Called "+it.Server+"."+it.Tool, pretty(it.Arguments))
		if len(it.Result) > 0 && string(it.Result) != "null" {
			result.Output, result.Truncated = clip(pretty(it.Result), maxOutput)
		}
		if it.Error != nil {
			result.Output, result.IsError = it.Error.Message, true
		}
		result.IsError = result.IsError || it.Status == "failed"
	case "web_search":
		call = tool("WebSearch", "Searched the web for "+it.Query, it.Query)
	case "todo_list":
		var b strings.Builder
		for _, t := range it.Items {
			mark := "[ ]"
			if t.Completed {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, t.Text)
		}
		if !done {
			return []APITranscriptEntry{tool("TodoWrite", "Updated its to-do list", strings.TrimRight(b.String(), "\n"))}
		}
		return nil
	case "error":
		if done {
			return []APITranscriptEntry{{Kind: "error", Text: it.Text}}
		}
		return nil
	default:
		return nil
	}
	if typ == "item.started" {
		return []APITranscriptEntry{call}
	}
	if !done {
		return nil
	}
	// A completed item carries its call too, for a log without the start.
	result.Tool, result.Summary, result.Input = call.Tool, call.Summary, call.Input
	return []APITranscriptEntry{result}
}

// unwrapShell drops the `bash -lc '...'` Codex wraps its commands in.
func unwrapShell(cmd string) string {
	for _, p := range []string{"/bin/bash -lc ", "bash -lc ", "/bin/sh -c ", "sh -c "} {
		if rest, ok := strings.CutPrefix(cmd, p); ok {
			rest = strings.TrimSpace(rest)
			if len(rest) >= 2 && (rest[0] == '\'' || rest[0] == '"') && rest[len(rest)-1] == rest[0] {
				rest = rest[1 : len(rest)-1]
			}
			return rest
		}
	}
	return cmd
}
