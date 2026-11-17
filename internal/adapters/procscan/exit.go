//go:build darwin

package procscan

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

// wakeIdent names the EVFILT_USER event that ends a watch when its context ends.
const wakeIdent = 1

// WatchPID returns a channel closed when the process exits, from a kqueue NOTE_EXIT filter
// registered before it returns, so an exit right after the call is never missed. A process that
// is already gone fails the registration. A kqueue error closes the channel too: a session whose
// proxy cannot be watched is treated as lost rather than trusted. Ending ctx ends the watch at
// once and closes its kqueue, without closing the channel.
func WatchPID(ctx context.Context, pid int) (<-chan struct{}, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	changes := make([]unix.Kevent_t, 2)
	unix.SetKevent(&changes[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	changes[0].Fflags = unix.NOTE_EXIT
	unix.SetKevent(&changes[1], wakeIdent, unix.EVFILT_USER, unix.EV_ADD|unix.EV_CLEAR)
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	// mu keeps a wake from reaching the descriptor number after the watch closed it, when it may
	// already name another file.
	var mu sync.Mutex
	open := true
	stopWaking := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !open {
			return
		}
		var wake unix.Kevent_t
		unix.SetKevent(&wake, wakeIdent, unix.EVFILT_USER, 0)
		wake.Fflags = unix.NOTE_TRIGGER
		_, _ = unix.Kevent(kq, []unix.Kevent_t{wake}, nil, nil)
	})
	exited := make(chan struct{})
	go func() {
		defer func() {
			stopWaking()
			mu.Lock()
			open = false
			_ = unix.Close(kq)
			mu.Unlock()
		}()
		got := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(kq, nil, got, nil)
			switch {
			case errors.Is(err, unix.EINTR):
				continue
			case err != nil:
				close(exited)
			case n > 0 && got[0].Filter == unix.EVFILT_PROC:
				close(exited)
			case n > 0:
				// The wake: ctx ended, and the process may still run.
			default:
				continue
			}
			return
		}
	}()
	return exited, nil
}
