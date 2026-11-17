//go:build darwin

package spawn

import (
	"syscall"
	_ "unsafe" // for go:linkname
)

//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_destroy posix_spawnattr_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setsigmask posix_spawnattr_setsigmask "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_init posix_spawn_file_actions_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_destroy posix_spawn_file_actions_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_adddup2 posix_spawn_file_actions_adddup2 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_addinherit_np posix_spawn_file_actions_addinherit_np "/usr/lib/libSystem.B.dylib"

// The addresses of the trampolines in libc_darwin.s.
var (
	posixSpawnAddr     uintptr
	attrInitAddr       uintptr
	attrDestroyAddr    uintptr
	attrSetFlagsAddr   uintptr
	attrSetSigmaskAddr uintptr
	actionsInitAddr    uintptr
	actionsDestroyAddr uintptr
	actionsDup2Addr    uintptr
	actionsInheritAddr uintptr
)

// syscall6 calls a libc function on the system stack, as golang.org/x/sys/unix does on darwin.
//
//go:linkname syscall6 syscall.syscall6
func syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// check reads a posix_spawn family result, which is the error number rather than errno; the
// return is a C int, so only the low 32 bits mean anything.
func check(r, _ uintptr, _ syscall.Errno) error {
	if code := r & 0xffffffff; code != 0 {
		return syscall.Errno(code)
	}
	return nil
}
