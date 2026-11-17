package socket_test

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
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

func TestKernelPeerPIDIsTheConnectingProcess(t *testing.T) {
	dir, err := os.MkdirTemp("", "berth-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "berthd.sock")
	ln, err := socket.Listen(t.Context(), path, uid(), socket.KernelPeerUID, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer func() { _ = conn.Close() }()
		pid, err := socket.KernelPeerPID(conn.(*net.UnixConn))
		if err != nil {
			pid = -1
		}
		got <- pid
	}()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if pid := <-got; pid != os.Getpid() {
		t.Fatalf("peer pid %d, want %d", pid, os.Getpid())
	}
}

func gid() uint32 { return uint32(os.Getgid()) } //nolint:gosec // group IDs fit in 32 bits

func sharedPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "Berth", "501", "agent.sock")
}

func serveShared(t *testing.T, path string, peer socket.PeerUID) {
	t.Helper()
	ln, err := socket.ListenShared(t.Context(), path, uid(), gid(), peer, slog.New(slog.DiscardHandler))
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
}

func TestSharedSocketServesTheAgentUIDWithGroupModes(t *testing.T) {
	path := sharedPath(t)
	serveShared(t, path, socket.KernelPeerUID)
	if got := read(t, path); got != "ok" {
		t.Fatalf("agent uid got %q", got)
	}
	for p, want := range map[string]os.FileMode{path: 0o660, filepath.Dir(path): 0o710} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if fi.Mode().Perm() != want || st.Gid != gid() {
			t.Errorf("%s mode %v group %d, want %v group %d", p, fi.Mode().Perm(), st.Gid, want, gid())
		}
	}
}

func TestSharedSocketRefusesAnotherUID(t *testing.T) {
	path := sharedPath(t)
	serveShared(t, path, func(*net.UnixConn) (uint32, error) { return uid() + 1, nil })
	if got := read(t, path); got != "" {
		t.Fatalf("another user was served %q", got)
	}
}

func TestSharedSocketRefusesAFolderOthersCouldReplace(t *testing.T) {
	listen := func(path string) error {
		ln, err := socket.ListenShared(t.Context(), path, uid(), gid(), socket.KernelPeerUID, slog.New(slog.DiscardHandler))
		if err == nil {
			_ = ln.Close()
		}
		return err
	}

	writable := sharedPath(t)
	parent := filepath.Dir(filepath.Dir(writable))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := listen(writable); !errors.Is(err, socket.ErrUnsafeFolder) {
		t.Errorf("parent writable by others: got %v, want ErrUnsafeFolder", err)
	}

	linked := sharedPath(t)
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(linked)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Dir(linked)); err != nil {
		t.Fatal(err)
	}
	if err := listen(linked); !errors.Is(err, socket.ErrUnsafeFolder) {
		t.Errorf("socket folder is a symlink: got %v, want ErrUnsafeFolder", err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() == 0o710 {
		t.Errorf("the symlink target's mode changed: %v %v", fi.Mode().Perm(), err)
	}
}
