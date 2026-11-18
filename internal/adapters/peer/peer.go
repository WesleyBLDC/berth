//go:build darwin

// Package peer learns about a socket peer from the macOS kernel: its executable, working
// directory and parent chain. Nothing here trusts what the peer says about itself.
package peer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
)

// From <sys/proc_info.h>; x/sys/unix has the syscall number but not libproc's wrappers.
const (
	procInfoCallPIDInfo   = 2
	procPIDPathInfo       = 11
	procPIDVnodePathInfo  = 9
	maxPathLen            = 1024
	procPIDPathInfoMaxLen = 4 * maxPathLen
	// struct vnode_info (vinfo_stat 136, vi_type 4, vi_pad 4, fsid_t 8) precedes the path.
	vnodeInfoSize     = 152
	vnodePathInfoSize = 2 * (vnodeInfoSize + maxPathLen)
	launchdPID        = 1
	// maxDepth bounds the parent walk; process trees are far shallower.
	maxDepth         = 64
	processSysctlPID = "kern.proc.pid"
)

// Facts implements ports.PeerFacts.
type Facts struct{}

func New() *Facts { return &Facts{} }

var _ ports.PeerFacts = (*Facts)(nil)

// Facts returns the peer's executable and working directory, and its ancestors from the parent
// up to launchd. The chain stops early at the first ancestor the kernel will not describe.
func (*Facts) Facts(ctx context.Context, pid int) (ports.Requester, error) {
	if pid <= 0 {
		return ports.Requester{}, fmt.Errorf("invalid pid %d", pid)
	}
	exe, err := executable(pid)
	if err != nil {
		return ports.Requester{}, fmt.Errorf("executable of process %d: %w", pid, err)
	}
	cwd, err := workingDir(pid)
	if err != nil {
		return ports.Requester{}, fmt.Errorf("working directory of process %d: %w", pid, err)
	}
	r := ports.Requester{PID: pid, Executable: exe, WorkingDir: cwd}
	// Lineage fails only for a cancelled ctx or a peer that exited since its path was read, which
	// has no parents left to describe. The chain stops at the first ancestor whose path the
	// kernel will not give.
	lineage, _ := Lineage(ctx, pid)
	if err := ctx.Err(); err != nil {
		return ports.Requester{}, err
	}
	for _, p := range lineage[min(1, len(lineage)):] {
		exe, ok := visibleExecutable(p.PID)
		if !ok {
			break
		}
		r.Tree = append(r.Tree, ports.Process{PID: p.PID, Executable: exe})
	}
	return r, nil
}

// visibleExecutable is false when the process has exited or the kernel will not describe it,
// which ends the chain rather than failing the whole lookup.
func visibleExecutable(pid int) (string, bool) {
	exe, err := executable(pid)
	return exe, err == nil
}

// Process reads one process's kernel record. It is ports.ErrProcessGone when pid names no
// process; any other failure says nothing about the process, so it must not end what the process
// stands for.
func Process(pid int) (ports.ProcessInfo, error) {
	if pid <= 0 {
		return ports.ProcessInfo{}, ports.ErrProcessGone
	}
	k, err := kinfoProc(processSysctlPID, pid)
	switch {
	// The kernel answers an exited pid with an empty record, which x/sys reports as EIO, its
	// error for a record of the wrong size.
	case errors.Is(err, unix.ESRCH), errors.Is(err, unix.EIO):
		return ports.ProcessInfo{}, ports.ErrProcessGone
	case err != nil:
		return ports.ProcessInfo{}, fmt.Errorf("%s %d: %w", processSysctlPID, pid, err)
	case int(k.Proc.P_pid) != pid:
		return ports.ProcessInfo{}, ports.ErrProcessGone
	}
	return ports.ProcessInfo{
		ProcessRef: ports.ProcessRef{PID: pid, Start: Start(k)},
		PPID:       int(k.Eproc.Ppid),
		UID:        k.Eproc.Ucred.Uid,
	}, nil
}

// kinfoProc is unix.SysctlKinfoProc, replaced in tests.
var kinfoProc = unix.SysctlKinfoProc

// Start is the process's start time in its kernel record, which with its pid names one process.
func Start(k *unix.KinfoProc) time.Time {
	return time.Unix(k.Proc.P_starttime.Sec, int64(k.Proc.P_starttime.Usec)*1000)
}

// Lineage is the process and its ancestors, read with kern.proc.pid one at a time.
func Lineage(ctx context.Context, pid int) ([]ports.ProcessInfo, error) {
	return walkLineage(ctx, pid, func(cur int) (ports.ProcessInfo, bool) {
		p, err := Process(cur)
		return p, err == nil
	})
}

// walkLineage follows parent pids through lookup. Each level is read at a different moment, so
// a parent can exit between reading its child and reading it: a parent already gone ends the
// lineage, and a pid that a later process took meanwhile is caught by its start time, since a
// real parent never starts after its child. Either way the lineage only gets shorter, never
// longer with a process the caller does not descend from.
func walkLineage(ctx context.Context, pid int, lookup func(int) (ports.ProcessInfo, bool)) ([]ports.ProcessInfo, error) {
	var out []ports.ProcessInfo
	for cur := pid; cur > 0 && len(out) < maxDepth; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ok := lookup(cur)
		if !ok || (len(out) > 0 && p.Start.After(out[len(out)-1].Start)) {
			break
		}
		out = append(out, p)
		if cur == launchdPID || p.PPID == cur {
			break
		}
		cur = p.PPID
	}
	if len(out) == 0 {
		return nil, ports.ErrProcessGone
	}
	return out, nil
}

// executable is proc_pidpath: the kernel's path for the process's text vnode, not argv[0].
func executable(pid int) (string, error) {
	buf := make([]byte, procPIDPathInfoMaxLen)
	if err := procInfo(pid, procPIDPathInfo, buf); err != nil {
		return "", err
	}
	return cString(buf)
}

// workingDir is proc_pidinfo(PROC_PIDVNODEPATHINFO): the path of pvi_cdir.
func workingDir(pid int) (string, error) {
	buf := make([]byte, vnodePathInfoSize)
	if err := procInfo(pid, procPIDVnodePathInfo, buf); err != nil {
		return "", err
	}
	return cString(buf[vnodeInfoSize : vnodeInfoSize+maxPathLen])
}

func procInfo(pid, flavor int, buf []byte) error {
	// x/sys/unix has no libproc wrapper and go.mod is frozen, so this is the raw proc_info
	// syscall libproc itself makes; buf outlives the call and its length bounds the kernel's write.
	//nolint:gosec,staticcheck // see above
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), uintptr(flavor), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return errno
	}
	return nil
}

func cString(b []byte) (string, error) {
	n := bytes.IndexByte(b, 0)
	if n <= 0 {
		return "", errors.New("kernel returned no path")
	}
	return string(b[:n]), nil
}

// Executable is proc_pidpath for any pid, for adapters that list processes rather than peers.
func Executable(pid int) (string, error) { return executable(pid) }

// PIDInfo is one proc_pidinfo call of the given flavor into buf.
func PIDInfo(pid, flavor int, buf []byte) error { return procInfo(pid, flavor, buf) }
