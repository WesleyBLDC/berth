package ports

import "context"

// Executable is what berthd verified about a file on disk. Grants pin these facts.
type Executable struct {
	// Path is absolute with every symlink resolved.
	Path   string
	SHA256 [32]byte
	// TeamID and SigningID are empty when the file is not validly signed.
	TeamID    string
	SigningID string
}

// Signed reports whether the code signature named a team.
func (e Executable) Signed() bool { return e.TeamID != "" }

// ExecIdentity identifies an executable by reading it, never by trusting a caller's claim.
type ExecIdentity interface {
	Identify(ctx context.Context, path string) (Executable, error)
}

// Process is one link of a requester's process tree.
type Process struct {
	PID        int
	Executable string
}

// Requester is what berthd learned about a socket peer from the kernel, not from the peer.
type Requester struct {
	PID        int
	Executable string
	// Tree runs from the peer's parent upward and ends at the harness (for example an agent
	// CLI or a terminal), or at the last ancestor visible when no harness is recognised.
	Tree       []Process
	WorkingDir string
}

// PeerFacts builds requester facts from the pid the socket reported for a peer.
type PeerFacts interface {
	Facts(ctx context.Context, pid int) (Requester, error)
}

// StoreOutcome is the only thing berth-native reports back about a store; never the value.
type StoreOutcome string

const (
	StoreStored StoreOutcome = "stored"
	StoreDenied StoreOutcome = "denied"
	StoreFailed StoreOutcome = "failed"
)

// StorePrompt is everything the store dialog may show. Reason is the requester's own claim,
// so the dialog labels it as such; every other field was verified by berthd.
type StorePrompt struct {
	Name      string
	Reason    string
	Requester Requester
}

// PowerAction names an action that needs Touch ID.
type PowerAction string

const PowerGrant PowerAction = "grant"

// PowerOutcome is the result of a Touch ID confirmation.
type PowerOutcome string

const (
	PowerApproved PowerOutcome = "approved"
	PowerDenied   PowerOutcome = "denied"
	PowerFailed   PowerOutcome = "failed"
)

// PowerPrompt is everything a Touch ID confirmation may show, all verified by berthd.
type PowerPrompt struct {
	Action    PowerAction
	Secret    string
	Consumer  Executable
	ArgsGlob  string
	Requester Requester
}

// Approver asks the person, through berth-native. A non nil error comes with the Failed
// outcome and carries detail for logs only; it never contains a value.
type Approver interface {
	StoreSecret(ctx context.Context, p StorePrompt) (StoreOutcome, error)
	ConfirmPower(ctx context.Context, p PowerPrompt) (PowerOutcome, error)
}

// Ticket is a single use ticket for one secret read.
type Ticket [32]byte

// TicketSource makes tickets; the real one reads crypto/rand, the fake is deterministic.
type TicketSource interface {
	NewTicket(ctx context.Context) (Ticket, error)
}
