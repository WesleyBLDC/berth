package testutil

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"berth/internal/core/ports"
)

// Approver is a scriptable ports.Approver. Queue outcomes before the call; with none queued
// it answers stored, approved and removed. Prompts records every prompt in order.
type Approver struct {
	mu           sync.Mutex
	stores       []ports.StoreOutcome
	powers       []ports.PowerOutcome
	rotates      []ports.StoreOutcome
	removes      []ports.RemoveOutcome
	storePrompts []ports.StorePrompt
	powerPrompts []ports.PowerPrompt
	lifecycle    []LifecycleCall
}

// LifecycleCall is one RotateSecret or RemoveSecret call as the fake saw it.
type LifecycleCall struct {
	Action ports.TicketAction
	Prompt ports.LifecyclePrompt
	Ticket ports.Ticket
}

func (a *Approver) QueueRotate(o ...ports.StoreOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rotates = append(a.rotates, o...)
}

func (a *Approver) QueueRemove(o ...ports.RemoveOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removes = append(a.removes, o...)
}

func (a *Approver) RotateSecret(_ context.Context, p ports.LifecyclePrompt, t ports.Ticket) (ports.StoreOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lifecycle = append(a.lifecycle, LifecycleCall{Action: ports.TicketRotate, Prompt: p, Ticket: t})
	o := ports.StoreStored
	if len(a.rotates) > 0 {
		o, a.rotates = a.rotates[0], a.rotates[1:]
	}
	if o == ports.StoreFailed {
		return o, errors.New("fake approver: rotate failed")
	}
	return o, nil
}

func (a *Approver) RemoveSecret(_ context.Context, p ports.LifecyclePrompt, t ports.Ticket) (ports.RemoveOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lifecycle = append(a.lifecycle, LifecycleCall{Action: ports.TicketRemove, Prompt: p, Ticket: t})
	o := ports.RemoveRemoved
	if len(a.removes) > 0 {
		o, a.removes = a.removes[0], a.removes[1:]
	}
	if o == ports.RemoveFailed {
		return o, errors.New("fake approver: remove failed")
	}
	return o, nil
}

func (a *Approver) LifecycleCalls() []LifecycleCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.lifecycle)
}

func (a *Approver) QueueStore(o ...ports.StoreOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stores = append(a.stores, o...)
}

func (a *Approver) QueuePower(o ...ports.PowerOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.powers = append(a.powers, o...)
}

func (a *Approver) StoreSecret(_ context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.storePrompts = append(a.storePrompts, p)
	o := ports.StoreStored
	if len(a.stores) > 0 {
		o, a.stores = a.stores[0], a.stores[1:]
	}
	if o == ports.StoreFailed {
		return o, errors.New("fake approver: store failed")
	}
	return o, nil
}

func (a *Approver) ConfirmPower(_ context.Context, p ports.PowerPrompt) (ports.PowerOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.powerPrompts = append(a.powerPrompts, p)
	o := ports.PowerApproved
	if len(a.powers) > 0 {
		o, a.powers = a.powers[0], a.powers[1:]
	}
	if o == ports.PowerFailed {
		return o, errors.New("fake approver: confirm failed")
	}
	return o, nil
}

func (a *Approver) StorePrompts() []ports.StorePrompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.storePrompts)
}

func (a *Approver) PowerPrompts() []ports.PowerPrompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.powerPrompts)
}

// ErrUnknownExecutable is what the fake ExecIdentity returns for a path it was not given.
var ErrUnknownExecutable = errors.New("fake exec identity: unknown executable")

// ExecIdentity is a ports.ExecIdentity backed by a map the test edits, so a test can change
// an executable's hash between calls.
type ExecIdentity struct {
	mu    sync.Mutex
	execs map[string]ports.Executable
}

func NewExecIdentity() *ExecIdentity { return &ExecIdentity{execs: map[string]ports.Executable{}} }

// Set registers the identity returned for path; Executable.Path is what the lookup resolves to.
func (x *ExecIdentity) Set(path string, e ports.Executable) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.execs[path] = e
}

func (x *ExecIdentity) Identify(_ context.Context, path string) (ports.Executable, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.execs[path]
	if !ok {
		return ports.Executable{}, fmt.Errorf("%w: %s", ErrUnknownExecutable, path)
	}
	return e, nil
}

// ErrUnknownPeer is what the fake PeerFacts returns for a pid it was not given.
var ErrUnknownPeer = errors.New("fake peer facts: unknown pid")

// PeerFacts is a ports.PeerFacts backed by a map the test edits.
type PeerFacts struct {
	mu    sync.Mutex
	peers map[int]ports.Requester
}

func NewPeerFacts() *PeerFacts { return &PeerFacts{peers: map[int]ports.Requester{}} }

func (p *PeerFacts) Set(r ports.Requester) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers[r.PID] = r
}

func (p *PeerFacts) Facts(_ context.Context, pid int) (ports.Requester, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.peers[pid]
	if !ok {
		return ports.Requester{}, fmt.Errorf("%w: %d", ErrUnknownPeer, pid)
	}
	return r, nil
}

// TicketSource hands out tickets whose every byte is a counter (1, 2, 3, ...), so tests can
// predict them. Fail makes the next NewTicket fail.
type TicketSource struct {
	mu   sync.Mutex
	next byte
	Fail error
}

func (s *TicketSource) NewTicket(_ context.Context) (ports.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail; err != nil {
		s.Fail = nil
		return ports.Ticket{}, err
	}
	s.next++
	var t ports.Ticket
	for i := range t {
		t[i] = s.next
	}
	return t, nil
}

var (
	_ ports.Approver     = (*Approver)(nil)
	_ ports.ExecIdentity = (*ExecIdentity)(nil)
	_ ports.PeerFacts    = (*PeerFacts)(nil)
	_ ports.TicketSource = (*TicketSource)(nil)
)
