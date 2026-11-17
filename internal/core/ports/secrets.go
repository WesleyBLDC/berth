package ports

import "context"

// Executable is what berthd verified about a file on disk. Grants pin these facts.
type Executable struct {
	// Path is absolute with every symlink resolved.
	Path   string
	SHA256 [32]byte
	// TeamID is set only for SignatureTeam; SigningID is empty when the file is not validly signed.
	TeamID    string
	SigningID string
	Signature Signature
	// Shebang is true when the file starts with #!, so it runs under an interpreter.
	Shebang bool
	// CDHash is what the kernel reports for a process started from the file: the file's own code
	// directory hash, or for a #! script its interpreter's. Zero when there is none to read.
	CDHash CDHash
}

// Signed reports whether the code signature named a team.
func (e Executable) Signed() bool { return e.TeamID != "" }

// Signature is what kind of code signature berthd verified on a file; the values are stored data
// in grants.signature, where an empty value marks a grant made before the kind was read.
type Signature string

const (
	SignatureUnsigned Signature = "unsigned"
	SignatureAdHoc    Signature = "adhoc"
	// SignatureApple is code Apple signed itself (the requirement "anchor apple"), as macOS
	// platform binaries are; they carry no team.
	SignatureApple Signature = "apple"
	SignatureTeam  Signature = "team"
	// SignatureOther is a valid signature from a certificate that is neither Apple's nor a team's.
	SignatureOther Signature = "other"
)

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

const (
	PowerGrant PowerAction = "grant"
	// PowerProxyGrant confirms a proxy grant; the prompt's Proxy says where the value may go.
	PowerProxyGrant PowerAction = "proxy"
)

// PowerOutcome is the result of a Touch ID confirmation.
type PowerOutcome string

const (
	PowerApproved PowerOutcome = "approved"
	PowerDenied   PowerOutcome = "denied"
	PowerFailed   PowerOutcome = "failed"
)

// PowerPrompt is everything a Touch ID confirmation may show, all verified by berthd.
type PowerPrompt struct {
	Action PowerAction
	Secret string
	// Consumer is set for PowerGrant, Proxy for PowerProxyGrant.
	Consumer Executable
	Proxy    ProxyTarget
	ArgsGlob string
	// Interpreter warns that the consumer runs other code, so it could print the secret.
	Interpreter bool
	// PassEnv names the environment variables the consumer will also receive.
	PassEnv   []string
	Requester Requester
}

// RemoveOutcome is the only thing berth-native reports back about a remove. A Keychain item
// that was already gone counts as removed.
type RemoveOutcome string

const (
	RemoveRemoved RemoveOutcome = "removed"
	RemoveDenied  RemoveOutcome = "denied"
	RemoveFailed  RemoveOutcome = "failed"
)

// LifecyclePrompt is everything the rotate dialog or the remove Touch ID sheet may show, all
// verified by berthd.
type LifecyclePrompt struct {
	Name      string
	Requester Requester
}

// Approver asks the person, through berth-native. A non nil error comes with the Failed
// outcome and carries detail for logs only; it never contains a value.
type Approver interface {
	StoreSecret(ctx context.Context, p StorePrompt) (StoreOutcome, error)
	ConfirmPower(ctx context.Context, p PowerPrompt) (PowerOutcome, error)
	// RotateSecret shows the store dialog in replace mode, then Touch ID, and updates the item.
	// berth-native redeems ticket, an action ticket bound to berthd's pid, before any prompt.
	RotateSecret(ctx context.Context, p LifecyclePrompt, ticket Ticket) (StoreOutcome, error)
	// RemoveSecret asks Touch ID and deletes the item, after redeeming ticket like RotateSecret.
	RemoveSecret(ctx context.Context, p LifecyclePrompt, ticket Ticket) (RemoveOutcome, error)
}

// Ticket is a single use ticket: one secret read, or one rotate or remove by berth-native.
type Ticket [32]byte

// TicketAction is what a redeemed ticket lets berth-native do.
type TicketAction string

const (
	TicketUse    TicketAction = "use"
	TicketRemove TicketAction = "remove"
	TicketRotate TicketAction = "rotate"
)

// TicketSource makes tickets; the real one reads crypto/rand, the fake is deterministic.
type TicketSource interface {
	NewTicket(ctx context.Context) (Ticket, error)
}
