//go:build darwin

package procscan

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// WatchPID returns a channel closed when the process exits, from a kqueue NOTE_EXIT filter
// registered before it returns, so an exit right after the call is never missed. A process that
// is already gone fails the registration. A kqueue error closes the channel too: a session whose
// proxy cannot be watched is treated as lost rather than trusted.
func WatchPID(ctx context.Context, pid int) (<-chan struct{}, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		defer func() { _ = unix.Close(kq) }()
		got := make([]unix.Kevent_t, 1)
		// A short wait lets the goroutine notice ctx without a second descriptor to wake it.
		wait := unix.NsecToTimespec(int64(time.Second))
		for ctx.Err() == nil {
			n, err := unix.Kevent(kq, nil, got, &wait)
			if n > 0 || (err != nil && !errors.Is(err, unix.EINTR)) {
				close(exited)
				return
			}
		}
	}()
	return exited, nil
}
