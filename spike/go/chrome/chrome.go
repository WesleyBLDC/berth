// Package chrome launches Chrome over --remote-debugging-pipe, so no TCP debugging port exists.
package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
)

const DefaultPath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

type Message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

type Options struct {
	Path        string
	UserDataDir string
	Headless    bool
	ExtraArgs   []string
}

// Browser is one Chrome process and its pipe. Raw messages from Chrome arrive on Incoming.
type Browser struct {
	cmd      *exec.Cmd
	toChrome *os.File
	Incoming chan []byte
	writeMu  sync.Mutex
	done     chan struct{}

	nextID  atomic.Int64
	pending sync.Map // int64 -> chan Message
	events  chan Message
}

func Launch(ctx context.Context, opts Options) (*Browser, error) {
	if opts.Path == "" {
		opts.Path = DefaultPath
	}
	if opts.UserDataDir == "" {
		return nil, errors.New("a dedicated user data dir is required; Chrome refuses debugging on the default profile")
	}
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + opts.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
	}
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, "about:blank")

	// Chrome reads commands on fd 3 and writes replies on fd 4.
	chromeReads, toChrome, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChrome, chromeWrites, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, opts.Path, args...)
	cmd.ExtraFiles = []*os.File{chromeReads, chromeWrites}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	chromeReads.Close()
	chromeWrites.Close()

	b := &Browser{
		cmd:      cmd,
		toChrome: toChrome,
		Incoming: make(chan []byte, 256),
		done:     make(chan struct{}),
		events:   make(chan Message, 256),
	}
	b.nextID.Store(1_000_000_000)
	go b.read(fromChrome)
	return b, nil
}

func (b *Browser) read(r *os.File) {
	defer close(b.done)
	defer close(b.Incoming)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, c := range data {
			if c == 0 {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		var m Message
		if json.Unmarshal(raw, &m) == nil && m.ID >= 1_000_000_000 {
			if ch, ok := b.pending.LoadAndDelete(m.ID); ok {
				ch.(chan Message) <- m
				continue
			}
		}
		b.Incoming <- raw
	}
}

// Write sends one raw CDP message to Chrome.
func (b *Browser) Write(raw []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.toChrome.Write(append(raw, 0)); err != nil {
		return err
	}
	return nil
}

// Call sends a command on Berth's own id range and waits for its reply.
func (b *Browser) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	id := b.nextID.Add(1)
	ch := make(chan Message, 1)
	b.pending.Store(id, ch)
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(Message{ID: id, Method: method, Params: p, SessionID: sessionID})
	if err := b.Write(raw); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if len(m.Error) > 0 {
			return nil, fmt.Errorf("%s: %s", method, m.Error)
		}
		return m.Result, nil
	case <-ctx.Done():
		b.pending.Delete(id)
		return nil, ctx.Err()
	case <-b.done:
		return nil, errors.New("chrome exited")
	}
}

func (b *Browser) Done() <-chan struct{} { return b.done }

// Close stops the whole process group so no helper process is left behind.
func (b *Browser) Close() {
	if b.cmd.Process != nil {
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGTERM)
	}
	_ = b.cmd.Wait()
}

func (b *Browser) PID() int { return b.cmd.Process.Pid }
