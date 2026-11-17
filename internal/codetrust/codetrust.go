// Package codetrust recognises Berth's own signed binaries in running processes: berthd uses it
// for the berth CLI and berth-native, and berth uses it for berthd. A file is pinned by the
// cdhash of a private copy that satisfies the signing requirement, and a process counts as that
// file only when the kernel reports the same cdhash, a valid signature and the hardened runtime.
package codetrust

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"berth/internal/core/ports"
)

// Berth's team and the identifiers its binaries are signed with.
const (
	TeamID          = "XPJ7XA42J4"
	CLISigningID    = "com.wesleybldc.berth.cli"
	HelperSigningID = "com.wesleybldc.berth.native"
	DaemonSigningID = "com.wesleybldc.berth.daemon"
	ProxySigningID  = "com.wesleybldc.berth.proxy"
)

// RepinInterval is the least time between two reads of the same file, so a caller cannot make
// the checker run codesign on every request.
const RepinInterval = 10 * time.Second

// Requirement pins Apple's certificate chain, the team and the identifier, which a self
// signed or renamed copy cannot satisfy.
func Requirement(identifier string) string {
	return `anchor apple generic and certificate leaf[subject.OU] = "` + TeamID + `" and identifier "` + identifier + `"`
}

// Pinner returns the cdhash of a trusted Berth file, or false when the file must not be trusted.
type Pinner func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error)

// SignedFiles checks a private copy of a file against a requirement and reads its cdhash
// (execid.Identity implements it).
type SignedFiles interface {
	PinSigned(ctx context.Context, path, requirement string) (ports.CDHash, bool, error)
}

// SignedPinner is the release rule: a private copy of the file must satisfy Requirement.
func SignedPinner(x SignedFiles) Pinner {
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, Requirement(identifier))
	}
}

// File is one Berth binary, pinned by cdhash.
type File struct {
	path       string
	identifier string
	pin        Pinner
	code       ports.CodeInspector
	clock      ports.Clock
	log        *slog.Logger

	mu       sync.Mutex
	cdhash   ports.CDHash
	ok       bool
	pinnedAt time.Time
	pinned   bool
}

// New pins the file at path now.
func New(ctx context.Context, path, identifier string, pin Pinner, code ports.CodeInspector, clock ports.Clock, log *slog.Logger) *File {
	f := &File{path: path, identifier: identifier, pin: pin, code: code, clock: clock, log: log}
	f.repin(ctx)
	return f
}

// repin reads the file again, for example after make build replaced it, at most once per
// RepinInterval.
func (f *File) repin(ctx context.Context) {
	now := f.clock.Now()
	f.mu.Lock()
	if f.pinned && now.Sub(f.pinnedAt) < RepinInterval {
		f.mu.Unlock()
		return
	}
	f.pinned, f.pinnedAt = true, now
	f.mu.Unlock()
	h, ok, err := f.pin(ctx, f.path, f.identifier)
	if err != nil || !ok {
		f.log.Warn("not trusting a Berth binary", "path", f.path, "identifier", f.identifier, "err", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdhash, f.ok = h, ok && err == nil
}

func (f *File) matches(live ports.LiveCode) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok && live.Valid() && live.Hardened() && live.CDHash == f.cdhash
}

// Runs reports whether peer is running this file right now.
func (f *File) Runs(ctx context.Context, peer ports.Peer) bool {
	live, err := f.code.Live(ctx, peer)
	if err != nil {
		f.log.Warn("cannot read a peer's code identity", "pid", peer.PID, "err", err)
		return false
	}
	if f.matches(live) {
		return true
	}
	// The file may have been rebuilt since it was pinned; reading it again is safe because the
	// pin rule checks the file itself.
	f.repin(ctx)
	return f.matches(live)
}
