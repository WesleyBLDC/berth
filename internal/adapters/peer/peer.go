//go:build darwin

// Package peer learns about a socket peer from the macOS kernel: its executable, working
// directory and parent chain. Nothing here trusts what the peer says about itself.
package peer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	maxDepth          = 64
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
	seen := map[int]bool{pid: true}
	for cur := pid; cur != launchdPID && len(r.Tree) < maxDepth; {
		if err := ctx.Err(); err != nil {
			return ports.Requester{}, err
		}
		p, ok := visibleParent(cur)
		if !ok || seen[p.PID] {
			break
		}
		seen[p.PID] = true
		r.Tree = append(r.Tree, p)
		cur = p.PID
	}
	return r, nil
}

// visibleParent is false when the parent has exited or the kernel will not describe it, which
// ends the chain rather than failing the whole lookup.
func visibleParent(pid int) (ports.Process, bool) {
	ppid, err := parent(pid)
	if err != nil || ppid <= 0 {
		return ports.Process{}, false
	}
	exe, err := executable(ppid)
	if err != nil {
		return ports.Process{}, false
	}
	return ports.Process{PID: ppid, Executable: exe}, true
}

func parent(pid int) (int, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	// An exited pid comes back as an empty record rather than an error.
	if int(k.Proc.P_pid) != pid {
		return 0, unix.ESRCH
	}
	return int(k.Eproc.Ppid), nil
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
