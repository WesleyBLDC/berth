// Package cdppipe launches Chromium with --remote-debugging-pipe and speaks CDP over that pipe:
// one JSON object per message, ended by a NUL byte, Chrome reading on fd 3 and writing on fd 4.
// No debugging port ever exists, and nothing but the browser's own process holds the pipe.
package cdppipe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"berth/internal/adapters/procscan"
	"berth/internal/core/ports"
)

const (
	// FirstOwnID starts Berth's own id range on the pipe; the proxy refuses agent ids from here
	// on, so a reply can always be told apart.
	FirstOwnID = 1_000_000_000
	// maxMessage bounds one message, so a runaway page cannot make berthd buffer without limit.
	maxMessage = 64 << 20
)

// killGroup sends SIGKILL to a process group; tests watch it.
var killGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }

// Driver starts browsers. It holds no state of its own.
type Driver struct{}

var _ ports.BrowserDriver = Driver{}

// Args are the browser's whole command line after the executable; there is no way to add to it.
func Args(l ports.BrowserLaunch) []string {
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + l.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
	}
	if l.Headless {
		args = append(args, "--headless=new")
	}
	return append(args, "about:blank")
}

// Launch starts the browser in its own process group with the pipe on fds 3 and 4. The
// browser outlives ctx; Kill or the pipe closing ends it.
func (Driver) Launch(ctx context.Context, l ports.BrowserLaunch) (ports.BrowserConn, error) {
	if l.Executable == "" || l.ProfileDir == "" {
		return nil, errors.New("a browser launch needs an executable and a profile folder")
	}
	if err := PrepareProfile(l.ProfileDir); err != nil {
		return nil, fmt.Errorf("prepare profile: %w", err)
	}
	chromeReads, toChrome, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChrome, chromeWrites, err := os.Pipe()
	if err != nil {
		_ = chromeReads.Close()
		_ = toChrome.Close()
		return nil, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), l.Executable, Args(l)...) //nolint:gosec // the executable was checked to be a Chromium family browser
	cmd.ExtraFiles = []*os.File{chromeReads, chromeWrites}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The browser gets none of berthd's environment beyond what it needs to start.
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + os.TempDir()}
	cmd.Dir = l.ProfileDir
	startErr := cmd.Start()
	_ = chromeReads.Close()
	_ = chromeWrites.Close()
	if startErr != nil {
		_ = toChrome.Close()
		_ = fromChrome.Close()
		return nil, fmt.Errorf("start browser: %w", startErr)
	}
	ref, err := procscan.Ref(cmd.Process.Pid)
	if err != nil {
		// It exited at once; the reaper below still runs so nothing is left behind.
		ref = ports.ProcessRef{PID: cmd.Process.Pid}
	}
	return newConn(ctx, cmd, ref, toChrome, fromChrome), nil
}

type reply struct {
	result []byte
	err    error
}

type conn struct {
	cmd  *exec.Cmd
	ref  ports.ProcessRef
	done chan struct{}

	reapMu sync.Mutex
	reaped bool

	writeMu sync.Mutex
	w       *os.File

	mu      sync.Mutex
	next    int64
	pending map[int64]chan reply
	// attaching names Berth's own calls whose reply carries a session Berth now owns.
	attaching map[int64]bool
	own       map[string]bool
	sink      func([]byte)
	gone      bool
}

func newConn(ctx context.Context, cmd *exec.Cmd, ref ports.ProcessRef, w, r *os.File) *conn {
	c := &conn{
		cmd: cmd, ref: ref, done: make(chan struct{}), w: w, next: FirstOwnID,
		pending: map[int64]chan reply{}, attaching: map[int64]bool{}, own: map[string]bool{},
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		c.read(r)
	}()
	go func() {
		// Wait for the exit without reaping, then reap under reapMu: while the leader is an
		// unreaped zombie its pid, and so the group id Kill signals, cannot name anything else.
		if exited, err := procscan.WatchPID(context.WithoutCancel(ctx), cmd.Process.Pid); err == nil {
			<-exited
		}
		c.reapMu.Lock()
		_ = cmd.Wait()
		c.reaped = true
		c.reapMu.Unlock()
		<-readDone
		_ = w.Close()
		close(c.done)
	}()
	return c
}

func (c *conn) Process() ports.ProcessRef { return c.ref }
func (c *conn) Done() <-chan struct{}     { return c.done }

// Kill signals the group only while its leader is unreaped, so a reused pid never names it.
func (c *conn) Kill() error {
	c.reapMu.Lock()
	defer c.reapMu.Unlock()
	if c.reaped {
		return nil
	}
	if err := killGroup(c.cmd.Process.Pid); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (c *conn) Sink(f func([]byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sink = f
}

func (c *conn) OwnSession(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.own[id]
}

func (c *conn) Send(raw []byte) error {
	if bytes.IndexByte(raw, 0) >= 0 {
		return errors.New("a CDP message cannot hold a NUL byte")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(append(raw[:len(raw):len(raw)], 0)); err != nil {
		return ports.ErrBrowserGone
	}
	return nil
}

type outgoing struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

func (c *conn) Call(ctx context.Context, sessionID, method string, params []byte) ([]byte, error) {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		return nil, ports.ErrBrowserGone
	}
	id := c.next
	c.next++
	c.pending[id] = ch
	if method == "Target.attachToTarget" || method == "Target.attachToBrowserTarget" {
		c.attaching[id] = true
	}
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.attaching, id)
		c.mu.Unlock()
	}
	raw, err := json.Marshal(outgoing{ID: id, Method: method, Params: params, SessionID: sessionID})
	if err != nil {
		forget()
		return nil, err
	}
	if err := c.Send(raw); err != nil {
		forget()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

type incoming struct {
	ID        *int64          `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *cdpError       `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// splitNUL splits the pipe's stream at each NUL byte.
func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (c *conn) read(r *os.File) {
	defer func() {
		_ = r.Close()
		c.mu.Lock()
		c.gone = true
		for id, ch := range c.pending {
			ch <- reply{err: ports.ErrBrowserGone}
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessage)
	sc.Split(splitNUL)
	for sc.Scan() {
		c.route(sc.Bytes())
	}
	if sc.Err() != nil {
		// An oversized or unreadable stream ends the browser rather than desynchronise the pipe.
		_ = c.Kill()
	}
}

// route hands a reply to Berth's own call, drops anything about Berth's own sessions, and gives
// everything else to the sink.
func (c *conn) route(raw []byte) {
	var m incoming
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	c.mu.Lock()
	if m.ID != nil && *m.ID >= FirstOwnID {
		ch, ok := c.pending[*m.ID]
		attach := c.attaching[*m.ID]
		delete(c.pending, *m.ID)
		delete(c.attaching, *m.ID)
		if attach && m.Error == nil {
			var res struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(m.Result, &res) == nil && res.SessionID != "" {
				c.own[res.SessionID] = true
			}
		}
		c.mu.Unlock()
		if ok {
			if m.Error != nil {
				ch <- reply{err: fmt.Errorf("CDP error %d: %s", m.Error.Code, m.Error.Message)}
			} else {
				ch <- reply{result: m.Result}
			}
		}
		return
	}
	if m.Method == "Target.detachedFromTarget" {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) == nil && c.own[p.SessionID] {
			delete(c.own, p.SessionID)
			c.mu.Unlock()
			return
		}
	}
	if c.own[m.SessionID] {
		c.mu.Unlock()
		return
	}
	sink := c.sink
	c.mu.Unlock()
	if sink != nil {
		// The scanner reuses its buffer, so the sink gets its own copy.
		sink(bytes.Clone(raw))
	}
}

// PrepareProfile points the profile's downloads at its own Downloads folder, with no prompt, so a
// download never lands in the person's ~/Downloads; agents cannot change it, since the proxy
// refuses setDownloadBehavior. It keeps every other preference Chrome wrote.
func PrepareProfile(dir string) error {
	downloads := filepath.Join(dir, "Downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "Default", "Preferences")
	prefs := map[string]any{}
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the profile folder is Berth's own
		// Unreadable preferences are replaced; Chrome would reset them anyway.
		_ = json.Unmarshal(b, &prefs)
		if prefs == nil {
			prefs = map[string]any{}
		}
	}
	section := func(name string) map[string]any {
		m, _ := prefs[name].(map[string]any)
		if m == nil {
			m = map[string]any{}
			prefs[name] = m
		}
		return m
	}
	download := section("download")
	download["default_directory"] = downloads
	download["prompt_for_download"] = false
	download["directory_upgrade"] = true
	section("savefile")["default_directory"] = downloads
	b, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	tmp := path + ".berth"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
