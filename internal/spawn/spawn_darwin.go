//go:build darwin

// Package spawn starts a child process suspended, so berth run can read the code identity the
// kernel gave it before it runs a single instruction, then resume it or kill it.
//
//nolint:gosec // every libc call hands pointers over as uintptr, inside the call expression as unsafe.Pointer rule 4 requires
package spawn

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
)

// From <spawn.h>.
const (
	setSigmask     = 0x0008
	startSuspended = 0x0080
	cloexecDefault = 0x4000
)

// From <sys/codesign.h>.
const (
	csOpsStatus = 0
	csOpsCDHash = 5
)

// StartSuspended runs path with argv (argv[0] included) and env through posix_spawn with
// POSIX_SPAWN_START_SUSPENDED: the kernel loads the image and stops before its first instruction.
// The child gets exactly files as its stdin, stdout and stderr and no other descriptor, and an
// empty signal mask. Each env entry is KEY=VALUE; StartSuspended copies them and zeroes its copies
// before returning, and the caller zeroes its own.
func StartSuspended(path string, argv []string, env [][]byte, files [3]*os.File) (int, error) {
	cpath, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	cargv := make([]*byte, 0, len(argv)+1)
	for _, a := range argv {
		p, err := syscall.BytePtrFromString(a)
		if err != nil {
			return 0, err
		}
		cargv = append(cargv, p)
	}
	cargv = append(cargv, nil)
	copies := make([][]byte, 0, len(env))
	defer func() {
		for _, c := range copies {
			clear(c)
		}
	}()
	cenv := make([]*byte, 0, len(env)+1)
	for _, e := range env {
		c := make([]byte, len(e)+1)
		copy(c, e)
		copies = append(copies, c)
		cenv = append(cenv, &c[0])
	}
	cenv = append(cenv, nil)

	var attr, actions uintptr
	if err := check(syscall6(attrInitAddr, uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0)); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_init: %w", err)
	}
	defer func() { _ = check(syscall6(attrDestroyAddr, uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0)) }()
	if err := check(syscall6(attrSetFlagsAddr, uintptr(unsafe.Pointer(&attr)), setSigmask|startSuspended|cloexecDefault, 0, 0, 0, 0)); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_setflags: %w", err)
	}
	var mask uint32
	if err := check(syscall6(attrSetSigmaskAddr, uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&mask)), 0, 0, 0, 0)); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_setsigmask: %w", err)
	}
	if err := check(syscall6(actionsInitAddr, uintptr(unsafe.Pointer(&actions)), 0, 0, 0, 0, 0)); err != nil {
		return 0, fmt.Errorf("posix_spawn_file_actions_init: %w", err)
	}
	defer func() { _ = check(syscall6(actionsDestroyAddr, uintptr(unsafe.Pointer(&actions)), 0, 0, 0, 0, 0)) }()
	for target, f := range files {
		fd := f.Fd()
		// With POSIX_SPAWN_CLOEXEC_DEFAULT a descriptor already at its target is closed unless
		// marked inherited; dup2 onto itself would not clear the flag.
		if fd == uintptr(target) {
			err = check(syscall6(actionsInheritAddr, uintptr(unsafe.Pointer(&actions)), fd, 0, 0, 0, 0))
		} else {
			err = check(syscall6(actionsDup2Addr, uintptr(unsafe.Pointer(&actions)), fd, uintptr(target), 0, 0, 0))
		}
		if err != nil {
			return 0, fmt.Errorf("posix_spawn file action for fd %d: %w", target, err)
		}
	}
	var pid int32
	err = check(syscall6(posixSpawnAddr, uintptr(unsafe.Pointer(&pid)), uintptr(unsafe.Pointer(cpath)), uintptr(unsafe.Pointer(&actions)), uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&cargv[0])), uintptr(unsafe.Pointer(&cenv[0]))))
	runtime.KeepAlive(cpath)
	runtime.KeepAlive(cargv)
	runtime.KeepAlive(cenv)
	runtime.KeepAlive(copies)
	runtime.KeepAlive(files)
	if err != nil {
		return 0, fmt.Errorf("posix_spawn %s: %w", path, err)
	}
	return int(pid), nil
}

// Live reads the code identity of a child of this process. A child not yet waited for keeps
// its pid, so the pid cannot name another process here.
func Live(pid int) (ports.LiveCode, error) {
	var live ports.LiveCode
	var status uint32
	if err := csops(pid, csOpsStatus, unsafe.Pointer(&status), unsafe.Sizeof(status)); err != nil {
		return live, fmt.Errorf("csops status: %w", err)
	}
	if err := csops(pid, csOpsCDHash, unsafe.Pointer(&live.CDHash[0]), uintptr(len(live.CDHash))); err != nil {
		return live, fmt.Errorf("csops cdhash: %w", err)
	}
	live.Status = status
	return live, nil
}

func csops(pid, op int, buf unsafe.Pointer, size uintptr) error {
	//nolint:gosec,staticcheck // x/sys/unix has the syscall number but no wrapper; buf outlives the call and size bounds the kernel's write
	_, _, errno := unix.Syscall6(unix.SYS_CSOPS, uintptr(pid), uintptr(op), uintptr(buf), size, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Resume lets a child StartSuspended made run its first instruction.
func Resume(pid int) error { return syscall.Kill(pid, syscall.SIGCONT) }
