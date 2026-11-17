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
	"unsafe"

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

// KernelPeerPID asks the kernel (LOCAL_PEERPID) which process connected, so handlers can learn
// about the caller without trusting anything it sends.
func KernelPeerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var pidErr error
	if err := raw.Control(func(fd uintptr) {
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if pidErr != nil {
		return 0, pidErr
	}
	return pid, nil
}

// KernelPeerToken asks the kernel (LOCAL_PEERTOKEN) for the connecting process's audit token,
// which names that process instance and cannot be reused by another.
func KernelPeerToken(c *net.UnixConn) ([32]byte, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return [32]byte{}, err
	}
	var token [32]byte
	var tokenErr error
	if err := raw.Control(func(fd uintptr) {
		size := uint32(len(token))
		// x/sys/unix has no byte-buffer getsockopt that keeps zero bytes; token and size outlive the call.
		//nolint:gosec,staticcheck // see above
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_LOCAL, unix.LOCAL_PEERTOKEN,
			uintptr(unsafe.Pointer(&token[0])), uintptr(unsafe.Pointer(&size)), 0)
		if errno != 0 {
			tokenErr = errno
		} else if size != uint32(len(token)) {
			tokenErr = fmt.Errorf("audit token is %d bytes", size)
		}
	}); err != nil {
		return [32]byte{}, err
	}
	return token, tokenErr
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
	ln, err := bind(ctx, path, 0o600, -1)
	if err != nil {
		return nil, err
	}
	return &listener{Listener: ln, uid: uid, peer: peer, log: log}, nil
}

// ListenShared creates a socket another user can reach: mode 0660 with group gid, inside a 0710
// folder with the same group, so members can connect but not list it. Only peers with user ID
// uid are served. The daemon's user must belong to gid. It refuses a folder another user owns
// or could replace, since berthd changes its mode and group and agents trust what answers there.
func ListenShared(ctx context.Context, path string, uid, gid uint32, peer PeerUID, log *slog.Logger) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // the parent only has to be traversable
		return nil, err
	}
	if err := checkAncestors(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := shareFolder(dir, gid); err != nil {
		return nil, err
	}
	ln, err := bind(ctx, path, 0o660, int(gid))
	if err != nil {
		return nil, err
	}
	return &listener{Listener: ln, uid: uid, peer: peer, log: log}, nil
}

// ErrUnsafeFolder means another user owns or could replace a folder on the socket's path.
var ErrUnsafeFolder = errors.New("unsafe socket folder")

// checkAncestors walks the real path of dir up to the root: each folder must belong to this user
// or root, and only a root owned folder with the sticky bit (like /tmp) may be writable by others.
func checkAncestors(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	self := uint32(os.Getuid()) //nolint:gosec // user IDs fit in 32 bits
	for p := real; ; p = filepath.Dir(p) {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		sticky := st.Mode&unix.S_ISVTX != 0
		switch {
		case st.Uid != self && st.Uid != 0:
			return fmt.Errorf("%w: %s belongs to user %d", ErrUnsafeFolder, p, st.Uid)
		case st.Mode&0o022 != 0 && (st.Uid != 0 || !sticky):
			return fmt.Errorf("%w: others can write %s", ErrUnsafeFolder, p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// shareFolder sets dir to 0710 with group gid through a descriptor opened without following
// symlinks, so the change lands on the folder that was checked and nowhere else.
func shareFolder(dir string, gid uint32) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrUnsafeFolder, dir, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Getuid()) { //nolint:gosec // user IDs fit in 32 bits
		return fmt.Errorf("%w: %s belongs to user %d", ErrUnsafeFolder, dir, st.Uid)
	}
	if err := unix.Fchown(fd, -1, int(gid)); err != nil {
		return fmt.Errorf("give %s to group %d: %w", dir, gid, err)
	}
	return unix.Fchmod(fd, 0o710)
}

// bind listens at path, removing a stale socket file but refusing a live one, then sets the
// socket's mode and, when gid is not -1, its group.
func bind(ctx context.Context, path string, mode os.FileMode, gid int) (net.Listener, error) {
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
	if gid != -1 {
		if err := os.Lchown(path, -1, gid); err != nil {
			_ = ln.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
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
