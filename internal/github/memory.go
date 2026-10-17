package github

import (
	"context"
	"fmt"
	"sync"
)

// Memory is an in-memory GitHub for `factory eval` and `factory compare`:
// the pipeline's issue, label, comment and PR calls land here instead of on
// github.com.
type Memory struct {
	mu       sync.Mutex
	issues   map[int]*Issue
	prs      map[string]*PR
	next     int
	Comments map[int][]string // PR and issue comments, by number
}

// NewMemory returns an empty in-memory GitHub.
func NewMemory() *Memory {
	return &Memory{issues: map[int]*Issue{}, prs: map[string]*PR{}, next: 1000}
}

// AddIssue adds an issue.
func (m *Memory) AddIssue(n int, title, body string, labels ...string) {
	m.issues[n] = &Issue{Number: n, Title: title, Body: body, Labels: labels}
}

func (m *Memory) ListIssues(_ context.Context, label string) ([]Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Issue
	for _, is := range m.issues {
		for _, l := range is.Labels {
			if l == label {
				out = append(out, *is)
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) GetIssue(_ context.Context, n int) (*Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.issues[n]
	if !ok {
		return nil, fmt.Errorf("no issue %d", n)
	}
	c := *is
	return &c, nil
}

func (m *Memory) AddLabels(_ context.Context, n int, ls ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if is, ok := m.issues[n]; ok {
		is.Labels = append(is.Labels, ls...)
	}
	return nil
}

func (m *Memory) RemoveLabel(_ context.Context, n int, l string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if is, ok := m.issues[n]; ok {
		var keep []string
		for _, x := range is.Labels {
			if x != l {
				keep = append(keep, x)
			}
		}
		is.Labels = keep
	}
	return nil
}

func (m *Memory) Comment(_ context.Context, n int, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Comments == nil {
		m.Comments = map[int][]string{}
	}
	m.Comments[n] = append(m.Comments[n], body)
	return nil
}

func (m *Memory) FindPR(_ context.Context, branch string) (*PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prs[branch], nil
}

func (m *Memory) GetPR(_ context.Context, n int) (*PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pr := range m.prs {
		if pr.Number == n {
			return pr, nil
		}
	}
	return nil, fmt.Errorf("no PR %d", n)
}

func (m *Memory) CreatePR(_ context.Context, _, _, head, _ string, draft bool) (*PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	pr := &PR{Number: m.next, URL: fmt.Sprintf("eval://pr/%d", m.next), State: "open", Draft: draft}
	m.prs[head] = pr
	return pr, nil
}

func (m *Memory) UpdatePRBody(ctx context.Context, n int, body string) error {
	return m.Comment(ctx, n, body)
}

func (m *Memory) CreateIssue(_ context.Context, title, body string, labels []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.issues[m.next] = &Issue{Number: m.next, Title: title, Body: body, Labels: labels}
	return m.next, nil
}
