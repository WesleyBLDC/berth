// Package socket serves the API on a Unix socket only the daemon's own user can use.
package socket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// PeerUID reports the user ID of the process on the other end of a Unix socket.
type PeerUID func(*net.UnixConn) (uint32, error)

// KernelPeerUID asks the kernel (LOCAL_PEERCRED), so the peer cannot lie about it.
func KernelPeerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// ErrAlreadyRunning means another daemon answers on the socket.
var ErrAlreadyRunning = errors.New("another berthd is already serving this socket")

// Listen creates the socket at path with mode 0600 inside a 0700 folder. A stale socket file
// from a crashed daemon is removed; a live one is an error.
func Listen(ctx context.Context, path string, uid uint32, peer PeerUID, log *slog.Logger) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // a folder needs the execute bit; 0700 is owner only
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		d := net.Dialer{Timeout: 300 * time.Millisecond}
		if c, err := d.DialContext(ctx, "unix", path); err == nil {
			_ = c.Close()
			return nil, ErrAlreadyRunning
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &listener{Listener: ln, uid: uid, peer: peer, log: log}, nil
}

type listener struct {
	net.Listener
	uid  uint32
	peer PeerUID
	log  *slog.Logger
}

// Accept drops connections from any other user before a single byte is read.
func (l *listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		got, err := l.peer(uc)
		if err != nil || got != l.uid {
			l.log.Warn("refused socket peer", "peer_uid", got, "err", err)
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}
