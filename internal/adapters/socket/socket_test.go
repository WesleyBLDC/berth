package socket_test

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
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
		// The accepted connection carries the kernel's facts, and asking again agrees.
		sc := conn.(*socket.Conn)
		pid, err := socket.KernelPeerPID(sc.UnixConn)
		if err != nil || pid != sc.PID || sc.AuditToken == ([32]byte{}) {
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

// sharedPath makes the folders berth agent setup would: a parent only this user (standing in for
// root) can write, and the socket folder with this user's group and mode 0710.
func sharedPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockDir := filepath.Join(dir, "Berth", "501")
	if err := os.MkdirAll(sockDir, 0o755); err != nil { //nolint:gosec // setup makes the parent 0755
		t.Fatal(err)
	}
	if err := os.Chown(sockDir, -1, int(gid())); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sockDir, 0o710); err != nil { //nolint:gosec // the mode setup gives it
		t.Fatal(err)
	}
	return filepath.Join(sockDir, "agent.sock")
}

func listenShared(t *testing.T, path string) error {
	t.Helper()
	ln, err := socket.ListenShared(t.Context(), path, uid(), gid(), socket.KernelPeerUID, slog.New(slog.DiscardHandler))
	if err == nil {
		_ = ln.Close()
	}
	return err
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

func TestSharedSocketNeedsSetup(t *testing.T) {
	path := sharedPath(t)
	berth := filepath.Dir(filepath.Dir(path))
	if err := os.RemoveAll(berth); err != nil {
		t.Fatal(err)
	}
	err := listenShared(t, path)
	if !errors.Is(err, socket.ErrNotSetUp) || !strings.Contains(err.Error(), "run berth agent setup") {
		t.Fatalf("missing folders: got %v, want ErrNotSetUp naming berth agent setup", err)
	}
	if _, err := os.Stat(berth); !os.IsNotExist(err) {
		t.Fatalf("berthd created the folder setup owns: %v", err)
	}
}

func TestSharedSocketRefusesAFolderOthersCouldReplace(t *testing.T) {
	writable := sharedPath(t)
	if err := os.Chmod(filepath.Dir(filepath.Dir(writable)), 0o777); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := listenShared(t, writable); !errors.Is(err, socket.ErrUnsafeFolder) {
		t.Errorf("parent writable by others: got %v, want ErrUnsafeFolder", err)
	}

	loose := sharedPath(t)
	if err := os.Chmod(filepath.Dir(loose), 0o750); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := listenShared(t, loose); !errors.Is(err, socket.ErrUnsafeFolder) {
		t.Errorf("socket folder not 0710: got %v, want ErrUnsafeFolder", err)
	}

	// A symlink this user (not root) owns is refused as the socket folder and in the middle.
	for _, level := range []int{1, 2} {
		real := sharedPath(t)
		linked := sharedPath(t)
		link := linked
		for range level {
			link = filepath.Dir(link)
		}
		target := real
		for range level {
			target = filepath.Dir(target)
		}
		if err := os.RemoveAll(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := listenShared(t, linked); !errors.Is(err, socket.ErrUnsafeFolder) {
			t.Errorf("symlink %d levels up: got %v, want ErrUnsafeFolder", level, err)
		}
	}
}
