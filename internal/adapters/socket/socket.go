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
	"strings"
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

// ListenShared creates a socket another user can reach, mode 0660 with group gid, and serves
// only peers with user ID uid. berthd never creates the socket's folder: berth agent setup makes
// it, owned by the daemon's user with group gid and mode 0710, inside a root owned parent, so no
// other user can squat or replace any folder on the path. Every level of the path is checked
// before the bind and again after it.
func ListenShared(ctx context.Context, path string, uid, gid uint32, peer PeerUID, log *slog.Logger) (net.Listener, error) {
	dir := filepath.Dir(path)
	before, err := verifyFolder(dir, gid)
	if err != nil {
		return nil, err
	}
	ln, err := bind(ctx, path, 0o660, int(gid))
	if err != nil {
		return nil, err
	}
	// Go cannot bind relative to the checked descriptor, so the path is walked again: the folder
	// it reaches must be the same one. Every level is owned by root or this user and writable by
	// no one else, so only root could change a level between the two walks.
	after, err := verifyFolder(dir, gid)
	if err == nil && after != before {
		err = fmt.Errorf("%w: %s changed while berthd bound the socket", ErrUnsafeFolder, dir)
	}
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &listener{Listener: ln, uid: uid, peer: peer, log: log}, nil
}

// ErrUnsafeFolder means a folder or symlink on the socket's path belongs to, or can be changed
// by, someone other than root and this user, or the socket folder is not set up as expected.
var ErrUnsafeFolder = errors.New("unsafe socket folder")

// ErrNotSetUp means the agent socket's folder does not exist yet.
var ErrNotSetUp = errors.New("the agent socket folder is missing; run berth agent setup")

type folderID struct{ dev, ino uint64 }

// verifyFolder walks dir from / one component at a time, opening each level with O_NOFOLLOW so
// the checked folder is the one used. A symlink is followed only when root owns it (/tmp, /var);
// every level must belong to root or this user, and only a root owned sticky folder may be
// writable by others. The last level must be this user's, with group gid and mode 0710.
func verifyFolder(dir string, gid uint32) (folderID, error) {
	if !filepath.IsAbs(dir) {
		return folderID{}, fmt.Errorf("%w: %s is not an absolute path", ErrUnsafeFolder, dir)
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return folderID{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	st, err := checkLevel(fd, "/")
	if err != nil {
		return folderID{}, err
	}
	pending := components(dir)
	for links := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		next, err := unix.Openat(fd, name, flags, 0)
		switch {
		// O_NOFOLLOW with O_DIRECTORY answers ENOTDIR for a symlink on macOS, ELOOP elsewhere.
		case (errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)) && isSymlink(fd, name):
			if links++; links > 32 {
				return folderID{}, fmt.Errorf("%w: too many symlinks in %s", ErrUnsafeFolder, dir)
			}
			target, err := rootSymlink(fd, name)
			if err != nil {
				return folderID{}, fmt.Errorf("%w: %s in %s", err, name, dir)
			}
			if filepath.IsAbs(target) {
				root, err := unix.Open("/", flags, 0)
				if err != nil {
					return folderID{}, err
				}
				_ = unix.Close(fd)
				fd = root
			}
			pending = append(components(target), pending...)
			continue
		case errors.Is(err, unix.ENOENT):
			return folderID{}, fmt.Errorf("%w (%s)", ErrNotSetUp, dir)
		case err != nil:
			return folderID{}, fmt.Errorf("open %s in %s: %w", name, dir, err)
		}
		_ = unix.Close(fd)
		fd = next
		if st, err = checkLevel(fd, name); err != nil {
			return folderID{}, err
		}
	}
	switch {
	case st.Uid != uint32(os.Getuid()): //nolint:gosec // user IDs fit in 32 bits
		return folderID{}, fmt.Errorf("%w: %s belongs to user %d; run berth agent setup", ErrUnsafeFolder, dir, st.Uid)
	case st.Gid != gid || st.Mode&0o7777 != 0o710:
		return folderID{}, fmt.Errorf("%w: %s must have group %d and mode 0710; run berth agent setup", ErrUnsafeFolder, dir, gid)
	}
	return folderID{dev: uint64(st.Dev), ino: st.Ino}, nil //nolint:gosec // device numbers are not negative
}

func checkLevel(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	self := uint32(os.Getuid()) //nolint:gosec // user IDs fit in 32 bits
	sticky := st.Mode&unix.S_ISVTX != 0
	switch {
	case st.Uid != self && st.Uid != 0:
		return st, fmt.Errorf("%w: %s belongs to user %d", ErrUnsafeFolder, name, st.Uid)
	case st.Mode&0o022 != 0 && (st.Uid != 0 || !sticky):
		return st, fmt.Errorf("%w: others can write %s", ErrUnsafeFolder, name)
	}
	return st, nil
}

func isSymlink(fd int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// rootSymlink reads the symlink name in the folder fd, refusing one root does not own.
func rootSymlink(fd int, name string) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	if st.Uid != 0 {
		return "", fmt.Errorf("%w: symlink owned by user %d", ErrUnsafeFolder, st.Uid)
	}
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(fd, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func components(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
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

// Conn is an accepted connection with the facts the kernel gave about its peer, read before a
// single byte was.
type Conn struct {
	*net.UnixConn
	UID        uint32
	PID        int
	AuditToken [32]byte
}

// Accept drops connections from any other user before a single byte is read, and any whose
// peer facts the kernel would not give, so no handler ever sees a peer it cannot name.
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
		pid, err := KernelPeerPID(uc)
		var token [32]byte
		if err == nil {
			token, err = KernelPeerToken(uc)
		}
		if err != nil {
			l.log.Warn("refused socket peer without process facts", "peer_uid", got, "err", err)
			_ = conn.Close()
			continue
		}
		return &Conn{UnixConn: uc, UID: got, PID: pid, AuditToken: token}, nil
	}
}
