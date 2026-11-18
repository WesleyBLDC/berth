// Package codetrust recognises Berth's own signed binaries in running processes: berthd uses it
// for the berth CLI, berth-native, berth-proxy and Berth.app, and berth and berth-proxy use it for
// berthd. A file is pinned by the cdhash of each architecture slice whose code satisfies the
// signing requirement, and a process counts as that file only when the kernel reports one of
// those cdhashes, a valid signature and the hardened runtime.
package codetrust

import (
	"context"
	"log/slog"
	"slices"
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
	AppSigningID    = "com.wesleybldc.berth"
)

// RepinInterval is the least time between two reads of the same file, so a caller cannot make
// the checker run codesign on every request.
const RepinInterval = 10 * time.Second

// Requirement pins Apple's certificate chain, the team and the identifier, which a self
// signed or renamed copy cannot satisfy.
func Requirement(identifier string) string {
	return `anchor apple generic and certificate leaf[subject.OU] = "` + TeamID + `" and identifier "` + identifier + `"`
}

// Pinner returns the cdhashes of a trusted Berth file, one per architecture slice, or false when
// the file must not be trusted.
type Pinner func(ctx context.Context, path, identifier string) ([]ports.CDHash, bool, error)

// SignedFiles returns the cdhash of every slice of a file whose code satisfies a requirement
// (execid.Identity implements it).
type SignedFiles interface {
	PinSigned(ctx context.Context, path, requirement string) ([]ports.CDHash, bool, error)
}

// SignedPinner is the release rule: each pinned slice's code must satisfy Requirement.
func SignedPinner(x SignedFiles) Pinner {
	return func(ctx context.Context, path, identifier string) ([]ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, Requirement(identifier))
	}
}

// File is one Berth binary, pinned by the cdhash of each of its architecture slices.
type File struct {
	path       string
	identifier string
	pin        Pinner
	code       ports.CodeInspector
	clock      ports.Clock
	log        *slog.Logger

	mu       sync.Mutex
	cdhashes []ports.CDHash
	ok       bool
	pinnedAt time.Time
	pinned   bool
	// inFlight is closed when the pin under way finishes; nil when none is.
	inFlight chan struct{}
}

// pinTimeout bounds one pin, which runs on its own context so a caller that gives up does not
// fail it for the callers waiting on it.
const pinTimeout = time.Minute

// New pins the file at path now.
func New(ctx context.Context, path, identifier string, pin Pinner, code ports.CodeInspector, clock ports.Clock, log *slog.Logger) *File {
	f := Deferred(path, identifier, pin, code, clock, log)
	f.repin(ctx)
	return f
}

// Deferred is New without the first read: the file is pinned on the first Runs, for a binary
// only some methods need, so a berthd with no Berth.app beside it never reads a file for one.
func Deferred(path, identifier string, pin Pinner, code ports.CodeInspector, clock ports.Clock, log *slog.Logger) *File {
	return &File{path: path, identifier: identifier, pin: pin, code: code, clock: clock, log: log}
}

// repin reads the file again, for example after make build replaced it, at most once per
// RepinInterval counted from the end of the last pin. Callers that arrive while a pin runs wait
// for it instead of starting another.
func (f *File) repin(ctx context.Context) {
	f.mu.Lock()
	if wait := f.inFlight; wait != nil {
		f.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	if f.pinned && f.clock.Now().Sub(f.pinnedAt) < RepinInterval {
		f.mu.Unlock()
		return
	}
	done := make(chan struct{})
	f.inFlight = done
	f.mu.Unlock()
	pinCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pinTimeout)
	h, ok, err := f.pin(pinCtx, f.path, f.identifier)
	cancel()
	if err != nil || !ok {
		f.log.Warn("not trusting a Berth binary", "path", f.path, "identifier", f.identifier, "err", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdhashes, f.ok = h, ok && err == nil
	f.pinned, f.pinnedAt, f.inFlight = true, f.clock.Now(), nil
	close(done)
}

func (f *File) matches(live ports.LiveCode) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok && live.Valid() && live.Hardened() && slices.Contains(f.cdhashes, live.CDHash)
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
