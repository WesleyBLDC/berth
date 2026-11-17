package testutil

import (
	"context"
	"errors"
	"sync"
	"time"

	"berth/internal/core/ports"
)

// Browsers is a scriptable ports.BrowserDriver: each launch gets a FakeBrowser that answers
// Browser.getVersion and exits on Browser.close or Kill, unless told otherwise.
type Browsers struct {
	mu sync.Mutex
	// LaunchErr fails every launch; Silent launches browsers that never answer over the pipe;
	// Stubborn ones ignore Browser.close.
	LaunchErr error
	Silent    bool
	Stubborn  bool
	// Hold, when set, makes each launch wait until it is closed or the launch's context ends.
	Hold     chan struct{}
	launched []*FakeBrowser
	nextPID  int
}

var _ ports.BrowserDriver = (*Browsers)(nil)

func (d *Browsers) Launch(ctx context.Context, l ports.BrowserLaunch) (ports.BrowserConn, error) {
	d.mu.Lock()
	hold := d.Hold
	d.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.LaunchErr != nil {
		return nil, d.LaunchErr
	}
	d.nextPID++
	b := &FakeBrowser{
		Launch: l, ref: ports.ProcessRef{PID: 5000 + d.nextPID, Start: time.Unix(1_800_000_000, 0)},
		silent: d.Silent, stubborn: d.Stubborn, done: make(chan struct{}),
	}
	d.launched = append(d.launched, b)
	return b, nil
}

// Launched returns every browser started so far, oldest first.
func (d *Browsers) Launched() []*FakeBrowser {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*FakeBrowser(nil), d.launched...)
}

// FakeBrowser is one launched fake browser.
type FakeBrowser struct {
	Launch   ports.BrowserLaunch
	ref      ports.ProcessRef
	silent   bool
	stubborn bool

	mu     sync.Mutex
	calls  []string
	sent   [][]byte
	sink   func([]byte)
	killed bool
	done   chan struct{}
	once   sync.Once
}

func (b *FakeBrowser) Process() ports.ProcessRef { return b.ref }
func (b *FakeBrowser) Done() <-chan struct{}     { return b.done }
func (b *FakeBrowser) OwnSession(string) bool    { return false }

// Exit ends the fake browser as if its process exited on its own.
func (b *FakeBrowser) Exit() { b.once.Do(func() { close(b.done) }) }

func (b *FakeBrowser) Call(ctx context.Context, _, method string, _ []byte) ([]byte, error) {
	b.mu.Lock()
	b.calls = append(b.calls, method)
	b.mu.Unlock()
	select {
	case <-b.done:
		return nil, ports.ErrBrowserGone
	default:
	}
	switch {
	case method == "Browser.getVersion" && !b.silent:
		return []byte(`{"product":"FakeChrome/1.0"}`), nil
	case method == "Browser.close" && !b.stubborn:
		b.Exit()
		return []byte(`{}`), nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, ports.ErrBrowserGone
	}
}

func (b *FakeBrowser) Send(raw []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
		return ports.ErrBrowserGone
	default:
	}
	b.sent = append(b.sent, append([]byte(nil), raw...))
	return nil
}

func (b *FakeBrowser) Sink(f func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sink = f
}

// Emit hands raw to the sink as if the browser sent it.
func (b *FakeBrowser) Emit(raw []byte) error {
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	if sink == nil {
		return errors.New("no sink")
	}
	sink(raw)
	return nil
}

func (b *FakeBrowser) Kill() error {
	b.mu.Lock()
	b.killed = true
	b.mu.Unlock()
	b.Exit()
	return nil
}

// Calls are the methods Berth called, in order.
func (b *FakeBrowser) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// Sent are the raw messages passed through Send, in order.
func (b *FakeBrowser) Sent() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]byte(nil), b.sent...)
}

func (b *FakeBrowser) Killed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.killed
}
