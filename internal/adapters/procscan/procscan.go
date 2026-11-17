//go:build darwin

// Package procscan implements ports.ProcessSource with the macOS kernel: the process table from
// kern.proc.all, paths from proc_pidpath, arguments from kern.procargs2, CPU time from
// proc_pidinfo, exits from kqueue. The kernel refuses another user's arguments, so those
// processes are opaque here by design.
package procscan

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	"berth/internal/adapters/peer"
	"berth/internal/core/ports"
)

// From <sys/proc_info.h>: PROC_PIDTASKINFO fills struct proc_taskinfo, whose pti_total_user
// and pti_total_system, in mach time units, sit at bytes 16 and 24 of its 96.
const (
	procPIDTaskInfo  = 4
	taskInfoSize     = 96
	taskInfoUser     = 16
	taskInfoSystem   = 24
	nanosPerSecond   = 1_000_000_000
	procargsSysctl   = "kern.procargs2"
	processesSysctl  = "kern.proc.all"
	processSysctlPID = "kern.proc.pid"
)

type Source struct {
	// tbFrequency is mach time units per second, so CPU ticks convert to durations.
	tbFrequency uint64
}

var _ ports.ProcessSource = (*Source)(nil)

func New() (*Source, error) {
	f, err := unix.SysctlUint64("hw.tbfrequency")
	if err != nil || f == 0 {
		return nil, fmt.Errorf("hw.tbfrequency: %w", err)
	}
	return &Source{tbFrequency: f}, nil
}

func (*Source) List(ctx context.Context) ([]ports.ProcessInfo, error) {
	ks, err := unix.SysctlKinfoProcSlice(processesSysctl)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", processesSysctl, err)
	}
	out := make([]ports.ProcessInfo, 0, len(ks))
	for i := range ks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		k := &ks[i]
		pid := int(k.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		// A process that exits between the table and this call just has no path.
		exe, _ := peer.Executable(pid)
		out = append(out, ports.ProcessInfo{
			ProcessRef: ports.ProcessRef{PID: pid, Start: start(k)},
			PPID:       int(k.Eproc.Ppid),
			UID:        k.Eproc.Ucred.Uid,
			Executable: exe,
		})
	}
	return out, nil
}

func (*Source) Args(_ context.Context, p ports.ProcessRef) ([]string, error) {
	if err := current(p); err != nil {
		return nil, err
	}
	b, err := unix.SysctlRaw(procargsSysctl, p.PID)
	if err != nil {
		// The kernel answers EINVAL for another user's process, and for a zombie.
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) {
			return nil, ports.ErrOpaqueProcess
		}
		return nil, err
	}
	args, err := parseProcargs(b)
	// The buffer also holds the process's environment, which can hold secrets.
	clear(b)
	if err != nil {
		return nil, err
	}
	// The pid may have been reused while the arguments were read.
	if err := current(p); err != nil {
		return nil, err
	}
	return args, nil
}

func (s *Source) CPUTime(_ context.Context, p ports.ProcessRef) (time.Duration, error) {
	if err := current(p); err != nil {
		return 0, err
	}
	buf := make([]byte, taskInfoSize)
	if err := peer.PIDInfo(p.PID, procPIDTaskInfo, buf); err != nil {
		if errors.Is(err, unix.EPERM) {
			return 0, ports.ErrOpaqueProcess
		}
		if errors.Is(err, unix.ESRCH) {
			return 0, ports.ErrProcessGone
		}
		return 0, err
	}
	ticks := binary.NativeEndian.Uint64(buf[taskInfoUser:]) + binary.NativeEndian.Uint64(buf[taskInfoSystem:])
	secs, rem := ticks/s.tbFrequency, ticks%s.tbFrequency
	return time.Duration(secs)*time.Second + time.Duration(rem*nanosPerSecond/s.tbFrequency), nil //nolint:gosec // CPU time of one process fits
}

func (*Source) WatchExit(ctx context.Context, p ports.ProcessRef) (<-chan struct{}, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	exited, err := WatchPID(watchCtx, p.PID)
	if err != nil {
		cancel()
		return nil, ports.ErrProcessGone
	}
	// Checked after the watch is registered, so a pid reused before the watch is caught here
	// and one reused after it fires the watch. A refused watch ends at once, kqueue and all.
	if err := current(p); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		select {
		case <-exited:
		case <-watchCtx.Done():
		}
		cancel()
	}()
	return exited, nil
}

func (*Source) Signal(_ context.Context, p ports.ProcessRef, sig ports.Signal) error {
	var s unix.Signal
	switch sig {
	case ports.SignalTerm:
		s = unix.SIGTERM
	case ports.SignalKill:
		s = unix.SIGKILL
	default:
		return fmt.Errorf("unknown signal %q", sig)
	}
	if err := current(p); err != nil {
		return err
	}
	if err := unix.Kill(p.PID, s); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return ports.ErrProcessGone
		}
		return err
	}
	return nil
}

var _ ports.ProcessLineage = (*Source)(nil)

// maxLineage bounds the parent walk; process trees are far shallower.
const maxLineage = 64

// Lineage walks parents with kern.proc.pid one at a time.
func (*Source) Lineage(ctx context.Context, pid int) ([]ports.ProcessInfo, error) {
	return walkLineage(ctx, pid, func(cur int) (ports.ProcessInfo, bool) {
		k, err := unix.SysctlKinfoProc(processSysctlPID, cur)
		// An exited pid comes back as an empty record rather than an error.
		if err != nil || int(k.Proc.P_pid) != cur {
			return ports.ProcessInfo{}, false
		}
		return ports.ProcessInfo{
			ProcessRef: ports.ProcessRef{PID: cur, Start: start(k)},
			PPID:       int(k.Eproc.Ppid),
			UID:        k.Eproc.Ucred.Uid,
		}, true
	})
}

// walkLineage follows parent pids through lookup. Each level is read at a different moment, so
// a parent can exit between reading its child and reading it: a parent already gone ends the
// lineage, and a pid that a later process took meanwhile is caught by its start time, since a
// real parent never starts after its child. Either way the lineage only gets shorter, never
// longer with a process the caller does not descend from.
func walkLineage(ctx context.Context, pid int, lookup func(int) (ports.ProcessInfo, bool)) ([]ports.ProcessInfo, error) {
	var out []ports.ProcessInfo
	for cur := pid; cur > 0 && len(out) < maxLineage; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ok := lookup(cur)
		if !ok || (len(out) > 0 && p.Start.After(out[len(out)-1].Start)) {
			break
		}
		out = append(out, p)
		if cur == 1 || p.PPID == cur {
			break
		}
		cur = p.PPID
	}
	if len(out) == 0 {
		return nil, ports.ErrProcessGone
	}
	return out, nil
}

// current is nil only when p.PID still names the process instance p started at p.Start.
func current(p ports.ProcessRef) error {
	if p.PID <= 0 {
		return ports.ErrProcessGone
	}
	k, err := unix.SysctlKinfoProc(processSysctlPID, p.PID)
	// An exited pid comes back as an empty record rather than an error.
	if err != nil || int(k.Proc.P_pid) != p.PID || !start(k).Equal(p.Start) {
		return ports.ErrProcessGone
	}
	return nil
}

func start(k *unix.KinfoProc) time.Time {
	return time.Unix(k.Proc.P_starttime.Sec, int64(k.Proc.P_starttime.Usec)*1000)
}
