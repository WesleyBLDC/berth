package ports

import "context"

// Peer is a socket caller as the kernel described the connection, never as the caller claims.
type Peer struct {
	PID int
	// AuditToken names this exact process instance; it stops matching once the process exits
	// or execs another image. All zero when the kernel gave none.
	AuditToken [32]byte
}

// CDHash identifies one signed image: the hash of its code directory, truncated to 20 bytes.
type CDHash [20]byte

// Code-signing status flags from <kern/cs_blobs.h>.
const (
	CSValid   uint32 = 0x00000001
	CSRuntime uint32 = 0x00010000
)

// LiveCode is what the kernel says about the image a process is running right now.
type LiveCode struct {
	CDHash CDHash
	Status uint32
}

// Valid and Hardened report the kernel's view: a valid signature and the hardened runtime,
// which refuses debuggers and injected libraries.
func (c LiveCode) Valid() bool    { return c.Status&CSValid != 0 }
func (c LiveCode) Hardened() bool { return c.Status&CSRuntime != 0 }

// CodeInspector reads a running process's code identity from the kernel by its audit token.
type CodeInspector interface {
	Live(ctx context.Context, p Peer) (LiveCode, error)
}
