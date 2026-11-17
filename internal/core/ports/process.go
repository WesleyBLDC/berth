package ports

import (
	"context"
	"errors"
	"time"
)

// ProcessRef names one process instance. A pid alone is reused after the process exits, so every
// question about a process carries the start time the kernel reported when it was listed.
type ProcessRef struct {
	PID   int
	Start time.Time
}

// Same reports whether two refs name the same process instance.
func (r ProcessRef) Same(o ProcessRef) bool { return r.PID == o.PID && r.Start.Equal(o.Start) }

// ProcessInfo is what the kernel tells any user about any process.
type ProcessInfo struct {
	ProcessRef
	PPID int
	UID  uint32
	// Executable is the kernel's path for the process's text vnode, never argv[0]; empty when
	// the kernel gave none.
	Executable string
}

var (
	// ErrOpaqueProcess means the kernel refuses to describe the process to this user, as it
	// refuses another user's arguments.
	ErrOpaqueProcess = errors.New("the kernel will not describe this process")
	// ErrProcessGone means the process exited, or its pid now names a process started later.
	ErrProcessGone = errors.New("process is gone")
)

// Signal names the signals the core may send to a process it did not start.
type Signal string

const (
	SignalTerm Signal = "TERM"
	SignalKill Signal = "KILL"
)

// ProcessSource lists processes and acts on one instance at a time. Every method that takes a
// ProcessRef answers ErrProcessGone rather than describe or signal a later process on that pid.
type ProcessSource interface {
	// List returns every process the kernel lists, as one snapshot, without arguments.
	List(ctx context.Context) ([]ProcessInfo, error)
	// Args returns the process's arguments, argv[0] first, or ErrOpaqueProcess.
	Args(ctx context.Context, p ProcessRef) ([]string, error)
	// CPUTime is the user plus system time the process has used, or ErrOpaqueProcess.
	CPUTime(ctx context.Context, p ProcessRef) (time.Duration, error)
	// WatchExit returns a channel closed when the process exits, registered before it returns
	// so an exit right after the call is never missed.
	WatchExit(ctx context.Context, p ProcessRef) (<-chan struct{}, error)
	// Signal sends sig after checking the pid still names this process instance.
	Signal(ctx context.Context, p ProcessRef, sig Signal) error
}

// EntityLister finds every entity of one machine kind, for boot recovery of a machine whose
// entities have no table of their own (browser sessions).
type EntityLister interface {
	EntitiesOfKind(ctx context.Context, kind string) ([]EntityState, error)
}

// ProcessLineage reads one process and its parents from the kernel's process table, which the
// kernel shows for every user.
type ProcessLineage interface {
	// Lineage returns pid's process first, then each parent, ending with launchd or with the
	// last parent still running; ErrProcessGone when pid itself is gone.
	Lineage(ctx context.Context, pid int) ([]ProcessInfo, error)
}
