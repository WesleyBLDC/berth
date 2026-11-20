package codetrust_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

type files struct {
	ok   bool
	hash ports.CDHash
}

func (f files) PinSigned(_ context.Context, _, requirement string) ([]ports.CDHash, bool, error) {
	return []ports.CDHash{f.hash}, f.ok && requirement == codetrust.Requirement(codetrust.DaemonSigningID), nil
}

func TestSignedPinnerNeedsTheRequirement(t *testing.T) {
	h := ports.CDHash{1}
	if got, ok, err := codetrust.SignedPinner(files{ok: true, hash: h})(t.Context(), "/b/berthd", codetrust.DaemonSigningID); err != nil || !ok || len(got) != 1 || got[0] != h {
		t.Fatalf("signed: %x %v %v", got, ok, err)
	}
	if _, ok, _ := codetrust.SignedPinner(files{hash: h})(t.Context(), "/b/berthd", codetrust.DaemonSigningID); ok {
		t.Fatal("a file failing the requirement was pinned")
	}
	want := `anchor apple generic and certificate leaf[subject.OU] = "XPJ7XA42J4" and identifier "com.wesleybldc.berth.daemon"`
	if got := codetrust.Requirement(codetrust.DaemonSigningID); got != want {
		t.Fatalf("requirement %s", got)
	}
}

type live struct{ code ports.LiveCode }

func (l *live) Live(context.Context, ports.Peer) (ports.LiveCode, error) {
	if l.code == (ports.LiveCode{}) {
		return ports.LiveCode{}, errors.New("gone")
	}
	return l.code, nil
}

func TestRunsNeedsMatchingHardenedCodeAndRepinsAtMostEveryInterval(t *testing.T) {
	clock := testutil.NewClock()
	pinned := ports.CDHash{1}
	var pins int
	pin := func(context.Context, string, string) ([]ports.CDHash, bool, error) {
		pins++
		return []ports.CDHash{pinned}, true, nil
	}
	peer := &live{code: ports.LiveCode{CDHash: ports.CDHash{1}, Status: ports.CSValid | ports.CSRuntime}}
	f := codetrust.New(t.Context(), "/b/berthd", codetrust.DaemonSigningID, pin, peer, clock, slog.New(slog.DiscardHandler))
	p := ports.Peer{PID: 7}
	if !f.Runs(t.Context(), p) {
		t.Fatal("matching hardened code was refused")
	}
	for _, status := range []uint32{ports.CSValid, ports.CSRuntime} {
		peer.code.Status = status
		if f.Runs(t.Context(), p) {
			t.Fatalf("status %#x was trusted", status)
		}
	}
	peer.code = ports.LiveCode{CDHash: ports.CDHash{2}, Status: ports.CSValid | ports.CSRuntime}
	for range 5 {
		if f.Runs(t.Context(), p) {
			t.Fatal("another cdhash was trusted")
		}
	}
	// Only the pin at start: every mismatch inside the interval is throttled.
	if pins != 1 {
		t.Fatalf("%d pins", pins)
	}
	pinned = ports.CDHash{2}
	if f.Runs(t.Context(), p) {
		t.Fatal("re-pinned before the interval passed")
	}
	clock.Advance(codetrust.RepinInterval + time.Second)
	if !f.Runs(t.Context(), p) {
		t.Fatal("the rebuilt file was not pinned again")
	}
	peer.code = ports.LiveCode{}
	if f.Runs(t.Context(), p) {
		t.Fatal("a process the kernel could not describe was trusted")
	}
}

// A deferred file reads nothing until the first Runs, then trusts the code it pinned.
func TestDeferredPinsOnTheFirstRuns(t *testing.T) {
	var pins []string
	pin := func(_ context.Context, path, identifier string) ([]ports.CDHash, bool, error) {
		pins = append(pins, path+" "+identifier)
		return []ports.CDHash{{3}}, true, nil
	}
	peer := &live{code: ports.LiveCode{CDHash: ports.CDHash{3}, Status: ports.CSValid | ports.CSRuntime}}
	f := codetrust.Deferred("/A/Contents/MacOS/Berth", codetrust.AppSigningID, pin, peer, testutil.NewClock(), slog.New(slog.DiscardHandler))
	if len(pins) != 0 {
		t.Fatalf("pinned before any call: %v", pins)
	}
	if !f.Runs(t.Context(), ports.Peer{PID: 9}) {
		t.Fatal("the pinned app was refused")
	}
	if want := []string{"/A/Contents/MacOS/Berth com.wesleybldc.berth"}; len(pins) != 1 || pins[0] != want[0] {
		t.Fatalf("pins %v, want %v", pins, want)
	}
}

// A universal binary is pinned by every slice, and a process running any of them is trusted.
func TestAnySliceOfAUniversalBinaryIsTrusted(t *testing.T) {
	pin := func(context.Context, string, string) ([]ports.CDHash, bool, error) {
		return []ports.CDHash{{1}, {2}}, true, nil
	}
	peer := &live{}
	f := codetrust.New(t.Context(), "/A/Contents/MacOS/Berth", codetrust.AppSigningID, pin, peer, testutil.NewClock(), slog.New(slog.DiscardHandler))
	for h, want := range map[ports.CDHash]bool{{1}: true, {2}: true, {3}: false} {
		peer.code = ports.LiveCode{CDHash: h, Status: ports.CSValid | ports.CSRuntime}
		if got := f.Runs(t.Context(), ports.Peer{PID: 9}); got != want {
			t.Errorf("slice %x trusted %v, want %v", h[:1], got, want)
		}
	}
}

// Callers that find no match while a pin runs wait for it rather than start their own, and the
// interval before the next pin counts from when a pin finished, so a slow pin is not repeated at
// once.
func TestRepinsAreSingleFlightAndSpacedFromTheirEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := testutil.NewClock()
		release := make(chan struct{})
		var pins atomic.Int32
		pin := func(context.Context, string, string) ([]ports.CDHash, bool, error) {
			if pins.Add(1) > 1 {
				return []ports.CDHash{{1}}, true, nil
			}
			<-release
			// The first pin takes longer than the interval.
			clock.Advance(codetrust.RepinInterval + time.Second)
			return []ports.CDHash{{1}}, true, nil
		}
		peer := &live{code: ports.LiveCode{CDHash: ports.CDHash{2}, Status: ports.CSValid | ports.CSRuntime}}
		f := codetrust.Deferred("/b/berthd", codetrust.DaemonSigningID, pin, peer, clock, slog.New(slog.DiscardHandler))
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { f.Runs(t.Context(), ports.Peer{PID: 7}) })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		if got := pins.Load(); got != 1 {
			t.Fatalf("%d pins for 8 concurrent mismatches, want 1", got)
		}
		f.Runs(t.Context(), ports.Peer{PID: 7})
		if got := pins.Load(); got != 1 {
			t.Fatalf("pinned again right after a slow pin finished: %d pins", got)
		}
	})
}

// A repin any process can trigger, by presenting code that does not match, keeps the last good pin
// when the check could not run, and drops it only when the file fails the check.
func TestAFailedRepinKeepsTheLastPinUntilTheFileFails(t *testing.T) {
	clock := testutil.NewClock()
	var result struct {
		ok  bool
		err error
	}
	result.ok = true
	pin := func(context.Context, string, string) ([]ports.CDHash, bool, error) {
		if result.err != nil || !result.ok {
			return nil, result.ok, result.err
		}
		return []ports.CDHash{{1}}, true, nil
	}
	genuine := &live{code: ports.LiveCode{CDHash: ports.CDHash{1}, Status: ports.CSValid | ports.CSRuntime}}
	other := ports.LiveCode{CDHash: ports.CDHash{9}, Status: ports.CSValid | ports.CSRuntime}
	f := codetrust.New(t.Context(), "/b/berthd", codetrust.DaemonSigningID, pin, genuine, clock, slog.New(slog.DiscardHandler))
	p := ports.Peer{PID: 7}

	result.err = errors.New("codesign: signal: killed")
	clock.Advance(codetrust.RepinInterval + time.Second)
	saved := genuine.code
	genuine.code = other
	if f.Runs(t.Context(), p) {
		t.Fatal("other code was trusted")
	}
	genuine.code = saved
	if !f.Runs(t.Context(), p) {
		t.Fatal("a repin that could not run untrusted the pinned code")
	}

	result.err, result.ok = nil, false
	clock.Advance(codetrust.RepinInterval + time.Second)
	genuine.code = other
	_ = f.Runs(t.Context(), p)
	genuine.code = saved
	if f.Runs(t.Context(), p) {
		t.Fatal("a file that failed the check stayed trusted")
	}
}
