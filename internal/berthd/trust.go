package berthd

import (
	"context"
	"log/slog"
	"sync"

	"berth/internal/core/ports"
)

// The signed Berth binaries berthd trusts with secrets: the berth CLI may receive use tickets,
// berth-native may redeem them.
const (
	TeamID          = "XPJ7XA42J4"
	CLISigningID    = "com.wesleybldc.berth.cli"
	HelperSigningID = "com.wesleybldc.berth.native"
)

// Requirement pins Apple's certificate chain, the team and the identifier, which a self
// signed or renamed copy cannot satisfy.
func Requirement(identifier string) string {
	return `anchor apple generic and certificate leaf[subject.OU] = "` + TeamID + `" and identifier "` + identifier + `"`
}

// Pinner returns the cdhash of a trusted Berth file, or false when the file must not be trusted.
type Pinner func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error)

// CodeFiles reads code signatures from files (execid.Identity implements it).
type CodeFiles interface {
	Satisfies(ctx context.Context, path, requirement string) (bool, error)
	CDHash(ctx context.Context, path string) (ports.CDHash, error)
}

// SignedPinner is the release rule: the file must satisfy Requirement before its cdhash is pinned.
func SignedPinner(x CodeFiles) Pinner {
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		ok, err := x.Satisfies(ctx, path, Requirement(identifier))
		if err != nil || !ok {
			return ports.CDHash{}, false, err
		}
		h, err := x.CDHash(ctx, path)
		return h, err == nil, err
	}
}

// trustedFile is one Berth binary beside berthd, pinned by cdhash. A running process is that
// binary only when the kernel reports the same cdhash, a valid signature and the hardened
// runtime, so a renamed copy, a modified file or a debugged process never passes.
type trustedFile struct {
	path       string
	identifier string
	pin        Pinner
	code       ports.CodeInspector
	log        *slog.Logger

	mu     sync.Mutex
	cdhash ports.CDHash
	ok     bool
}

// repin reads the file again, for example after make build replaced it.
func (f *trustedFile) repin(ctx context.Context) {
	h, ok, err := f.pin(ctx, f.path, f.identifier)
	if err != nil || !ok {
		f.log.Warn("not trusting a Berth binary", "path", f.path, "identifier", f.identifier, "err", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdhash, f.ok = h, ok && err == nil
}

func (f *trustedFile) matches(live ports.LiveCode) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok && live.Valid() && live.Hardened() && live.CDHash == f.cdhash
}

// runs reports whether peer is running this file right now.
func (f *trustedFile) runs(ctx context.Context, peer ports.Peer) bool {
	live, err := f.code.Live(ctx, peer)
	if err != nil {
		f.log.Warn("cannot read a peer's code identity", "pid", peer.PID, "err", err)
		return false
	}
	if f.matches(live) {
		return true
	}
	// The file may have been rebuilt since berthd pinned it; reading it again is safe because
	// the pin rule checks the file itself.
	f.repin(ctx)
	return f.matches(live)
}
