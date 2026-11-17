package procscan

import (
	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
)

// Ref names the process instance pid names now, with the start time the kernel reports, for a
// process the caller just started.
func Ref(pid int) (ports.ProcessRef, error) {
	if pid <= 0 {
		return ports.ProcessRef{}, ports.ErrProcessGone
	}
	k, err := unix.SysctlKinfoProc(processSysctlPID, pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return ports.ProcessRef{}, ports.ErrProcessGone
	}
	return ports.ProcessRef{PID: pid, Start: start(k)}, nil
}
