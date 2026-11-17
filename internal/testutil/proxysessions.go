package testutil

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"berth/internal/core/ports"
)

// ProxySessions is an in memory ports.ProxySessionRecords.
type ProxySessions struct {
	mu       sync.Mutex
	sessions map[string]ports.ProxySessionRecord
	order    []string
}

func NewProxySessions() *ProxySessions {
	return &ProxySessions{sessions: map[string]ports.ProxySessionRecord{}}
}

func (p *ProxySessions) AddProxySession(_ context.Context, r ports.ProxySessionRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sessions[r.ID]; ok {
		return fmt.Errorf("fake proxy sessions: session %s exists", r.ID)
	}
	r.Grants = slices.Clone(r.Grants)
	r.EndedAt, r.Injected, r.Refused = time.Time{}, 0, 0
	p.sessions[r.ID] = r
	p.order = append(p.order, r.ID)
	return nil
}

func (p *ProxySessions) ProxySession(_ context.Context, id string) (ports.ProxySessionRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.sessions[id]
	r.Grants = slices.Clone(r.Grants)
	return r, ok, nil
}

func (p *ProxySessions) ProxySessionsForGrant(_ context.Context, grantID int64) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		r := p.sessions[id]
		if r.EndedAt.IsZero() && slices.Contains(r.Grants, grantID) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (p *ProxySessions) ProxySessionsForAgentSession(_ context.Context, agentSession string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		r := p.sessions[id]
		if r.EndedAt.IsZero() && agentSession != "" && r.AgentSession == agentSession {
			out = append(out, id)
		}
	}
	return out, nil
}

func (p *ProxySessions) OpenProxySessions(_ context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		if p.sessions[id].EndedAt.IsZero() {
			out = append(out, id)
		}
	}
	return out, nil
}

func (p *ProxySessions) EndProxySession(_ context.Context, id string, injected, refused int64, at time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.sessions[id]
	if !ok {
		return fmt.Errorf("fake proxy sessions: no session %s", id)
	}
	if !r.EndedAt.IsZero() {
		return fmt.Errorf("fake proxy sessions: session %s already ended", id)
	}
	r.EndedAt, r.Injected, r.Refused = at, injected, refused
	p.sessions[id] = r
	return nil
}

var _ ports.ProxySessionRecords = (*ProxySessions)(nil)
