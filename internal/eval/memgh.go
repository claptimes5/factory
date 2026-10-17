package eval

import (
	"context"
	"fmt"
	"sync"

	"github.com/claptimes5/factory/internal/github"
)

// memGH is an in-memory GitHub for one eval case: the pipeline's issue,
// label, comment and PR calls land here instead of on github.com.
type memGH struct {
	mu     sync.Mutex
	issues map[int]*github.Issue
	prs    map[string]*github.PR
	next   int
}

func newMemGH() *memGH {
	return &memGH{issues: map[int]*github.Issue{}, prs: map[string]*github.PR{}, next: 1000}
}

func (m *memGH) addIssue(n int, title, body string, labels ...string) {
	m.issues[n] = &github.Issue{Number: n, Title: title, Body: body, Labels: labels}
}

func (m *memGH) ListIssues(_ context.Context, label string) ([]github.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []github.Issue
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

func (m *memGH) GetIssue(_ context.Context, n int) (*github.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.issues[n]
	if !ok {
		return nil, fmt.Errorf("no issue %d", n)
	}
	c := *is
	return &c, nil
}

func (m *memGH) AddLabels(_ context.Context, n int, ls ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if is, ok := m.issues[n]; ok {
		is.Labels = append(is.Labels, ls...)
	}
	return nil
}

func (m *memGH) RemoveLabel(_ context.Context, n int, l string) error {
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

func (m *memGH) Comment(context.Context, int, string) error { return nil }

func (m *memGH) FindPR(_ context.Context, branch string) (*github.PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prs[branch], nil
}

func (m *memGH) GetPR(_ context.Context, n int) (*github.PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pr := range m.prs {
		if pr.Number == n {
			return pr, nil
		}
	}
	return nil, fmt.Errorf("no PR %d", n)
}

func (m *memGH) CreatePR(_ context.Context, _, _, head, _ string, draft bool) (*github.PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	pr := &github.PR{Number: m.next, URL: fmt.Sprintf("eval://pr/%d", m.next), State: "open", Draft: draft}
	m.prs[head] = pr
	return pr, nil
}

func (m *memGH) UpdatePRBody(context.Context, int, string) error { return nil }

func (m *memGH) CreateIssue(_ context.Context, title, body string, labels []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.issues[m.next] = &github.Issue{Number: m.next, Title: title, Body: body, Labels: labels}
	return m.next, nil
}
