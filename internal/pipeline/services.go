package pipeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
)

// ServiceWait is how long a job's services may take to become ready.
var ServiceWait = 2 * time.Minute

// StartServices starts the repos' services for a job, so its builder and
// checks reach them by name, and returns what stops them. Job 0 is the
// readiness check. A workspace job passes every member: their services
// share one network, and where two repos name a service the same, the
// first repo's wins. With no services, or a runner that can't run them,
// it does nothing.
func (p *Pipeline) StartServices(ctx context.Context, jobID int64, repos ...string) (stop func(), err error) {
	svcs := map[string]config.Service{}
	for _, repo := range repos {
		for n, s := range p.repoConf(repo).Services {
			if _, ok := svcs[n]; !ok {
				svcs[n] = s
			}
		}
	}
	if len(svcs) == 0 {
		return func() {}, nil
	}
	sr, ok := p.Runner.(runner.ServiceRunner)
	if !ok {
		// Config only allows services with the docker runner; this is a
		// pipeline wired up some other way, so say what it's missing.
		p.log().Warn("services not started: the runner can't run them", "repos", repos)
		return func() {}, nil
	}
	names := make([]string, 0, len(svcs))
	for n := range svcs {
		names = append(names, n)
	}
	sort.Strings(names)
	var specs []runner.Service
	for _, n := range names {
		s := svcs[n]
		specs = append(specs, runner.Service{Name: n, Image: s.Image, Env: s.Env, Ready: s.Ready})
	}
	job := p.containerName(jobID)
	net, err := sr.StartServices(ctx, job, specs, ServiceWait)
	if err != nil {
		return func() {}, fmt.Errorf("starting services: %w", err)
	}
	p.svcMu.Lock()
	if p.svcNets == nil {
		p.svcNets = map[int64]string{}
	}
	p.svcNets[jobID] = net
	p.svcMu.Unlock()
	return func() {
		p.svcMu.Lock()
		delete(p.svcNets, jobID)
		p.svcMu.Unlock()
		p.warn(sr.StopServices(context.Background(), job), "StopServices")
	}, nil
}

// servicesOf is a job's services network, or "".
func (p *Pipeline) servicesOf(jobID int64) string {
	p.svcMu.Lock()
	defer p.svcMu.Unlock()
	return p.svcNets[jobID]
}
