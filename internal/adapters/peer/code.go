//go:build darwin

package peer

import (
	"context"
	"encoding/binary"
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
)

// From <sys/codesign.h>.
const (
	csOpsStatus = 0
	csOpsCDHash = 5
	// The pid sits at index 5 of the audit token's eight 32-bit words.
	auditTokenPIDWord = 5
)

// Code implements ports.CodeInspector with csops_audittoken, so the answer is about the exact
// process instance that connected: a pid reused by another process, or a process that has
// since exec'd another image, no longer matches its token and fails.
type Code struct{}

func NewCode() *Code { return &Code{} }

var _ ports.CodeInspector = (*Code)(nil)

func (*Code) Live(_ context.Context, p ports.Peer) (ports.LiveCode, error) {
	if p.AuditToken == ([32]byte{}) {
		return ports.LiveCode{}, errors.New("no audit token for this peer")
	}
	pid := int(binary.LittleEndian.Uint32(p.AuditToken[auditTokenPIDWord*4:]))
	if pid != p.PID {
		return ports.LiveCode{}, errors.New("audit token names another process")
	}
	var live ports.LiveCode
	var status [4]byte
	if err := csops(pid, csOpsStatus, status[:], &p.AuditToken); err != nil {
		return ports.LiveCode{}, err
	}
	if err := csops(pid, csOpsCDHash, live.CDHash[:], &p.AuditToken); err != nil {
		return ports.LiveCode{}, err
	}
	live.Status = binary.NativeEndian.Uint32(status[:])
	return live, nil
}

func csops(pid, op int, buf []byte, token *[32]byte) error {
	// x/sys/unix has the syscall number but no wrapper; buf and token outlive the call, and
	// len(buf) bounds the kernel's write.
	//nolint:gosec,staticcheck // see above
	_, _, errno := unix.Syscall6(unix.SYS_CSOPS_AUDITTOKEN, uintptr(pid), uintptr(op), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(token)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
