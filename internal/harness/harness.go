// Package harness drives the stock agent CLIs (Claude Code and Codex)
// headless inside a runner and normalizes their JSON output. There is no
// custom agent loop here: each adapter builds a command line and parses.
package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// RunRequest is one agent run.
type RunRequest struct {
	Name      string // container name
	Stage     string // triage|implement|review|...
	Model     string
	Fast      bool // Claude only: fast mode
	Effort    string
	Prompt    string
	Workdir   string // host path of the checkout
	OutDir    string // host path; verdict.json is written here
	SkillsDir string // host path of the skill set, mounted read-only ("" for none)
	ReadOnly  bool
	Schema    string // JSON schema for the final verdict
	BudgetUSD float64
	MaxTurns  int
	Env       map[string]string // short-lived credentials only
	Timeout   time.Duration
	Image     string
	Mounts    []runner.Mount // extra mounts, e.g. the orchestrator's MCP socket
	MCPConfig string         // JSON for `claude --mcp-config` (Claude only)
	Tools     []string       // extra allowed tools for read-only runs, e.g. mcp__factory
	// SessionDir is a host directory that keeps the CLI's session files
	// across jobs (Claude only), and Resume a session in it to continue.
	SessionDir string
	Resume     string
	// Services is the job's services network, when it has services.
	Services string
	// Images are pictures for the agent to look at, as paths the process
	// sees: Codex attaches them to the prompt; Claude reads them with its
	// Read tool, so the prompt names them.
	Images []string
	// CaptureLimits asks the CLI for its usage-limit windows where it only
	// writes them to its own files (Codex). Only in a container: on the
	// host those files are the user's own sessions.
	CaptureLimits bool
}

// Limit is one usage-limit window the CLI reported, such as a
// subscription's five-hour or weekly allowance.
type Limit struct {
	Window      string // "5h", "weekly", or the CLI's own name
	UsedPercent float64
	ResetsAt    time.Time // zero when unknown
}

// Event is normalized from each CLI's JSON stream.
type Event struct {
	Kind    string // text|tool_call|tool_result|usage|final|error|raw
	Payload json.RawMessage
}

// Result is what a finished run reported.
type Result struct {
	Final     string // final message (verdict JSON for read-only stages)
	CostUSD   float64
	CostKnown bool // false when the CLI reports tokens only
	TokensIn  int64
	TokensOut int64
	// TokensCached is the part of TokensIn read from the prompt cache,
	// which is priced lower.
	TokensCached int64
	Turns        int
	IsError      bool
	Error        string
	ExitCode     int
	SessionID    string // the CLI's session, for a later Resume
	Limits       []Limit
	// SignedOut is set when the CLI couldn't sign in (Codex on a ChatGPT
	// plan): the run failed through no fault of the task.
	SignedOut bool
	// Redact are secrets the run held besides its env (a ChatGPT
	// sign-in's tokens), to keep out of its log.
	Redact []string
}

// setLimit replaces the reading for l's window.
func (r *Result) setLimit(l Limit) {
	for i := range r.Limits {
		if r.Limits[i].Window == l.Window {
			if l.ResetsAt.IsZero() {
				l.ResetsAt = r.Limits[i].ResetsAt
			}
			r.Limits[i] = l
			return
		}
	}
	r.Limits = append(r.Limits, l)
}

// Harness runs one agent CLI.
type Harness interface {
	Name() string
	Run(ctx context.Context, req RunRequest, events chan<- Event) (*Result, error)
}

// adapter is the per-CLI part: the command and a line parser.
type adapter interface {
	command(req RunRequest, p runner.Paths) ([]string, error)
	parse(line []byte, res *Result, emit func(Event))
	skillsPath(p runner.Paths) string
}

type cliHarness struct {
	name   string
	a      adapter
	runner runner.Runner
}

func (h *cliHarness) Name() string { return h.name }

func (h *cliHarness) OneAtATime() bool {
	g, ok := h.a.(Gated)
	return ok && g.OneAtATime()
}

func (h *cliHarness) Unavailable() string {
	if g, ok := h.a.(Gated); ok {
		return g.Unavailable()
	}
	return ""
}

// Recover cleans up after runs a crash cut short, where the CLI keeps
// files outside the job (Codex's ChatGPT sign-in).
func (h *cliHarness) Recover() error {
	if r, ok := h.a.(interface{ Recover() error }); ok {
		return r.Recover()
	}
	return nil
}

func (h *cliHarness) Run(ctx context.Context, req RunRequest, events chan<- Event) (*Result, error) {
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return nil, err
	}
	spec := runner.Spec{
		Name: req.Name, Image: req.Image, Workdir: req.Workdir, OutDir: req.OutDir,
		ReadOnly: req.ReadOnly, Env: req.Env, Timeout: req.Timeout, Services: req.Services,
	}
	spec.Mounts = append(spec.Mounts, req.Mounts...)
	paths := h.runner.Paths(spec)
	if req.SkillsDir != "" {
		spec.Mounts = append(spec.Mounts, runner.Mount{Host: req.SkillsDir, Container: h.a.skillsPath(paths), ReadOnly: true})
	}
	if sp, ok := h.a.(interface{ sessionsPath(runner.Paths) string }); ok && req.SessionDir != "" {
		if err := os.MkdirAll(req.SessionDir, 0o700); err != nil {
			return nil, err
		}
		spec.Mounts = append(spec.Mounts, runner.Mount{Host: req.SessionDir, Container: sp.sessionsPath(paths)})
	}
	if req.Schema != "" {
		if err := os.WriteFile(filepath.Join(req.OutDir, "schema.json"), []byte(req.Schema), 0o644); err != nil {
			return nil, err
		}
	}
	cmd, err := h.a.command(req, paths)
	if err != nil {
		return nil, err
	}
	spec.Cmd = cmd
	after := func(*Result) {}
	var redact []string
	if pa, ok := h.a.(interface {
		prepare(ctx context.Context, req RunRequest, p runner.Paths, spec *runner.Spec) (func(*Result), []string, error)
	}); ok {
		if after, redact, err = pa.prepare(ctx, req, paths, &spec); err != nil {
			return nil, err
		}
	}
	pr, pw := io.Pipe()
	spec.Stdin = stringsReader(req.Prompt)

	res := &Result{}
	emit := func(e Event) {
		if events != nil {
			select {
			case events <- e:
			case <-ctx.Done():
			}
		}
	}
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			raw := json.RawMessage(scrub(string(line), redact))
			emit(Event{Kind: "raw", Payload: raw})
			h.a.parse(line, res, emit)
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	code, runErr := h.runner.Run(ctx, spec, pw)
	_ = pw.Close()
	<-parsed
	res.ExitCode = code
	if la, ok := h.a.(interface {
		limitsAfter(outDir string, res *Result)
	}); ok && req.CaptureLimits {
		la.limitsAfter(req.OutDir, res)
	}
	after(res)
	if runErr != nil {
		return res, runErr
	}
	if code != 0 && !res.IsError {
		res.IsError = true
		if res.Error == "" {
			res.Error = fmt.Sprintf("%s exited with code %d", h.name, code)
		}
	}
	return res, nil
}

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func stringsReader(s string) io.Reader { return &stringReader{s: s} }
