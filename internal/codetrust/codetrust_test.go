package codetrust_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

type files struct {
	ok   bool
	hash ports.CDHash
}

func (f files) PinSigned(_ context.Context, _, requirement string) (ports.CDHash, bool, error) {
	return f.hash, f.ok && requirement == codetrust.Requirement(codetrust.DaemonSigningID), nil
}

func TestSignedPinnerNeedsTheRequirement(t *testing.T) {
	h := ports.CDHash{1}
	if got, ok, err := codetrust.SignedPinner(files{ok: true, hash: h})(t.Context(), "/b/berthd", codetrust.DaemonSigningID); err != nil || !ok || got != h {
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
	pin := func(context.Context, string, string) (ports.CDHash, bool, error) {
		pins++
		return pinned, true, nil
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

func TestExecutableCopyRunsOnlyACheckedPrivateCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "berth-native")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o700); err != nil { //nolint:gosec // a fixture
		t.Fatal(err)
	}
	var checked string
	accept := func(_ context.Context, path, _ string) (ports.CDHash, bool, error) {
		checked = path
		return ports.CDHash{}, true, nil
	}
	exe, cleanup, err := codetrust.ExecutableCopy(t.Context(), src, codetrust.HelperSigningID, accept)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(exe)
	if err != nil || exe == src || checked != exe || info.Mode().Perm() != 0o700 {
		t.Fatalf("copy %s (checked %s): %v %v", exe, checked, info.Mode(), err)
	}
	if dirInfo, _ := os.Stat(filepath.Dir(exe)); dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", dirInfo.Mode())
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(exe)); !os.IsNotExist(err) {
		t.Fatalf("folder left behind: %v", err)
	}
	refuse := func(context.Context, string, string) (ports.CDHash, bool, error) { return ports.CDHash{}, false, nil }
	if _, _, err := codetrust.ExecutableCopy(t.Context(), src, codetrust.HelperSigningID, refuse); err == nil {
		t.Fatal("a file failing the check was accepted")
	}
}
