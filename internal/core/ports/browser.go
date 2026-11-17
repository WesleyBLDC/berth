package ports

import (
	"context"
	"errors"
)

// BrowserLaunch is everything a launch may choose. There is no field for extra arguments or a
// debugging port: a managed browser speaks CDP only over its private pipe.
type BrowserLaunch struct {
	// Executable is the browser to run, already checked to be a Chromium family browser.
	Executable string
	// ProfileDir is the user data folder, which Berth owns.
	ProfileDir string
	Headless   bool
}

// ErrBrowserGone means the browser exited or its pipe closed before a call was answered.
var ErrBrowserGone = errors.New("browser is gone")

// BrowserConn is one running browser and its CDP pipe.
type BrowserConn interface {
	// Process is the browser's root process instance.
	Process() ProcessRef
	// Call sends method on Berth's own id range, to the browser or to sessionID, and returns the
	// raw result; a CDP error comes back as an error.
	Call(ctx context.Context, sessionID, method string, params []byte) ([]byte, error)
	// Send writes one message that is not Berth's own, already checked by the caller.
	Send(raw []byte) error
	// Sink receives every message from the browser that is not a reply to Berth's own calls, on
	// the pipe's reader goroutine; a nil sink drops them. Only one sink is set at a time.
	Sink(func(raw []byte))
	// OwnSession reports whether sessionID belongs to a target Berth attached to itself.
	OwnSession(sessionID string) bool
	// Done is closed once the pipe reached EOF and the process was reaped.
	Done() <-chan struct{}
	// Kill sends SIGKILL to the browser's whole process group.
	Kill() error
}

// BrowserDriver starts browsers Berth manages.
type BrowserDriver interface {
	Launch(ctx context.Context, l BrowserLaunch) (BrowserConn, error)
}
