package testutil

import (
	"context"
	"slices"
	"sync"
	"time"

	"berth/internal/core/ports"
)

// FakeProcess is one row of a Processes table.
type FakeProcess struct {
	ports.ProcessInfo
	Args []string
	// Opaque makes Args and CPUTime answer ErrOpaqueProcess, as for another user's process.
	Opaque bool
	CPU    time.Duration
}

// Processes is a scriptable ports.ProcessSource: tests set the table and read the signals sent.
type Processes struct {
	mu      sync.Mutex
	table   []FakeProcess
	signals []SentSignal
}

type SentSignal struct {
	PID    int
	Signal ports.Signal
}

var _ ports.ProcessSource = (*Processes)(nil)

func (p *Processes) Set(table ...FakeProcess) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.table = table
}

func (p *Processes) Signals() []SentSignal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]SentSignal(nil), p.signals...)
}

func (p *Processes) find(ref ports.ProcessRef) (FakeProcess, error) {
	for _, f := range p.table {
		if f.Same(ref) {
			return f, nil
		}
	}
	return FakeProcess{}, ports.ErrProcessGone
}

func (p *Processes) List(context.Context) ([]ports.ProcessInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ports.ProcessInfo, 0, len(p.table))
	for _, f := range p.table {
		out = append(out, f.ProcessInfo)
	}
	return out, nil
}

func (p *Processes) Args(_ context.Context, ref ports.ProcessRef) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := p.find(ref)
	if err != nil {
		return nil, err
	}
	if f.Opaque {
		return nil, ports.ErrOpaqueProcess
	}
	return f.Args, nil
}

func (p *Processes) CPUTime(_ context.Context, ref ports.ProcessRef) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := p.find(ref)
	if err != nil {
		return 0, err
	}
	if f.Opaque {
		return 0, ports.ErrOpaqueProcess
	}
	return f.CPU, nil
}

// WatchExit never fires; tests drive exits through the table.
func (p *Processes) WatchExit(_ context.Context, ref ports.ProcessRef) (<-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.find(ref); err != nil {
		return nil, err
	}
	return make(chan struct{}), nil
}

func (p *Processes) Signal(_ context.Context, ref ports.ProcessRef, sig ports.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.find(ref); err != nil {
		return err
	}
	p.signals = append(p.signals, SentSignal{PID: ref.PID, Signal: sig})
	return nil
}

var _ ports.ProcessLineage = (*Processes)(nil)

// Lineage walks the table by parent pid, with procscan's rule for reused pids.
func (p *Processes) Lineage(_ context.Context, pid int) ([]ports.ProcessInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ports.ProcessInfo
	for cur := pid; len(out) < 64; {
		i := slices.IndexFunc(p.table, func(f FakeProcess) bool { return f.PID == cur })
		// A parent that started after its child is a later process on a reused pid.
		if i < 0 || (len(out) > 0 && p.table[i].Start.After(out[len(out)-1].Start)) {
			break
		}
		out = append(out, p.table[i].ProcessInfo)
		if cur == 1 {
			break
		}
		cur = p.table[i].PPID
	}
	if len(out) == 0 {
		return nil, ports.ErrProcessGone
	}
	return out, nil
}
