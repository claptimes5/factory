package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A readiness check is a job like any other: queued, one per repo at a
// time, run by the daemon, still there after a restart, and in the
// history with what it did. It runs no model. Its steps go to the job's
// progress file as it goes, and what it did to its result file.

// KindReadiness is a readiness check of a repo's base branch.
const KindReadiness = "readiness"

// ReadinessInput is a readiness check's job input.
type ReadinessInput struct {
	Repo string `json:"repo"`
	// Why says what started it when the factory did, by itself ("master
	// moved from 1a2b3c4 to 5d6e7f8"); "" when a person asked.
	Why string `json:"why,omitempty"`
}

// QueueReadiness queues a readiness check of repo, unless one is queued or
// running: then it returns false.
func (p *Pipeline) QueueReadiness(ctx context.Context, repo, why string) (bool, error) {
	b, _ := json.Marshal(ReadinessInput{Repo: repo, Why: why})
	_, ok, err := p.Store.EnqueueSoleFor(ctx, store.Job{Kind: KindReadiness, Input: string(b), Priority: 10}, repo)
	return ok, err
}

func (p *Pipeline) runReadiness(ctx context.Context, j *store.Job) error {
	var in ReadinessInput
	if err := json.Unmarshal([]byte(j.Input), &in); err != nil || in.Repo == "" {
		p.finish(ctx, j, agentRun{}, state.Failed, "", fmt.Sprintf("bad job input: %v", err))
		return nil
	}
	if p.Readiness == nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", "this daemon can't run a readiness check")
		return nil
	}
	dir := p.jobDir(j.ID)
	w, err := progressFile(dir)
	if err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		return nil
	}
	defer func() { _ = w.Close() }()
	res, err := p.Readiness(ctx, in.Repo, w)
	if err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		return nil
	}
	p.warn(os.WriteFile(filepath.Join(dir, ResultFile), []byte(res), 0o644), "save readiness result")
	p.finish(ctx, j, agentRun{}, state.Succeeded, "", "")
	return nil
}

// ProgressFile, in a job's folder, is a setup run's steps, one JSON line
// each; ResultFile is what it did.
const (
	ProgressFile = "progress.jsonl"
	ResultFile   = "result.txt"
)

// ProgressLine is one step of a setup run, and when it was said.
type ProgressLine struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// progressLog writes each line said to it as a ProgressLine.
type progressLog struct {
	mu sync.Mutex
	f  *os.File
}

func progressFile(dir string) (*progressLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ProgressFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &progressLog{f: f}, nil
}

func (l *progressLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		out, _ := json.Marshal(ProgressLine{At: store.Now(), Text: line})
		if _, err := l.f.Write(append(out, '\n')); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func (l *progressLog) Close() error { return l.f.Close() }

// ReadProgress reads a setup run's steps from its job folder, oldest first.
func ReadProgress(dir string) []ProgressLine {
	f, err := os.Open(filepath.Join(dir, ProgressFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ProgressLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l ProgressLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// ReadResult reads what a setup run did, from its job folder.
func ReadResult(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ResultFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var _ io.WriteCloser = (*progressLog)(nil)
