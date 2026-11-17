package socket_test

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"berth/internal/adapters/socket"
)

func uid() uint32 { return uint32(os.Getuid()) } //nolint:gosec // user IDs fit in 32 bits

func serveEcho(t *testing.T, peer socket.PeerUID) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s", "berthd.sock")
	ln, err := socket.Listen(t.Context(), path, uid(), peer, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("ok"))
			_ = c.Close()
		}
	}()
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestSameUserIsServedAndModesAreTight(t *testing.T) {
	path := serveEcho(t, socket.KernelPeerUID)
	if got := read(t, path); got != "ok" {
		t.Fatalf("same user got %q", got)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}

func TestOtherUserIsRefused(t *testing.T) {
	path := serveEcho(t, func(*net.UnixConn) (uint32, error) { return uid() + 1, nil })
	if got := read(t, path); got != "" {
		t.Fatalf("another user was served %q", got)
	}
}

func TestSecondDaemonOnLiveSocketFails(t *testing.T) {
	path := serveEcho(t, socket.KernelPeerUID)
	if _, err := socket.Listen(t.Context(), path, uid(), socket.KernelPeerUID, slog.New(slog.DiscardHandler)); !errors.Is(err, socket.ErrAlreadyRunning) {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
}
