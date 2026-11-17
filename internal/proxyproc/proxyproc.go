// Package proxyproc starts berth-proxy and speaks its two JSON lines. The berth command uses it
// in owner mode, and berthd uses it to start the proxy of an agent session as its own user.
package proxyproc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"berth/internal/proxyctl"
)

// Process is a started berth-proxy. Stdin carries the request and stays open for the run;
// closing it tells the proxy to end its session and exit.
type Process struct {
	// PID is 0 when the process is not a real one (tests).
	PID    int
	Stdin  io.WriteCloser
	Stdout io.Reader
	Wait   func() error
	Kill   func() error
}

var credentialHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

const maxControlLine = 64 << 10

// Start runs the berth-proxy at path with only the variables it accepts (proxyctl.Env), in its own process
// group, so a terminal Ctrl-C reaches neither it nor its session before its owner closes stdin.
func Start(ctx context.Context, path string, stderr io.Writer) (*Process, error) {
	cmd := exec.CommandContext(ctx, path) //nolint:gosec // path is the berth-proxy beside the caller
	cmd.Env = keepEnv(os.Environ())
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// An os.Pipe, not StdoutPipe: Wait would close a StdoutPipe while the caller still drains it.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("cannot start %s: %w", path, err)
	}
	return &Process{
		PID:    cmd.Process.Pid,
		Stdin:  stdin,
		Stdout: r,
		Wait: func() error {
			defer func() { _ = r.Close() }()
			return cmd.Wait()
		},
		Kill: cmd.Process.Kill,
	}, nil
}

func keepEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(proxyctl.Env, k) {
			out = append(out, kv)
		}
	}
	return out
}

// Handshake sends the request for names in mode (proxyctl.ModeOwner or ModeAgent) and waits up
// to timeout for the control line, which the proxy prints only once berthd let its session serve.
func Handshake(p *Process, names []string, mode string, timeout time.Duration) (proxyctl.Control, error) {
	req, err := json.Marshal(proxyctl.Request{Secrets: names, Mode: mode})
	if err != nil {
		return proxyctl.Control{}, err
	}
	if _, err := p.Stdin.Write(append(req, '\n')); err != nil {
		return proxyctl.Control{}, fmt.Errorf("cannot send it the request: %w", err)
	}
	type lineResult struct {
		line []byte
		err  error
	}
	got := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReaderSize(p.Stdout, maxControlLine).ReadSlice('\n')
		got <- lineResult{slices.Clone(line), err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-got:
		switch {
		case errors.Is(r.err, bufio.ErrBufferFull):
			return proxyctl.Control{}, errors.New("its control line is too long")
		case r.err != nil:
			return proxyctl.Control{}, errors.New("it exited before it was ready")
		}
		return Parse(r.line)
	case <-timer.C:
		return proxyctl.Control{}, fmt.Errorf("it was not ready within %s", timeout)
	}
}

// Parse decodes a control line strictly and never echoes it, since it holds the run's credential.
func Parse(line []byte) (proxyctl.Control, error) {
	var ctl proxyctl.Control
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ctl); err != nil {
		return proxyctl.Control{}, errors.New("its control line is malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return proxyctl.Control{}, errors.New("its control line is malformed")
	}
	return ctl, Check(ctl)
}

// Check refuses control facts a child must not be given, whoever reported them.
func Check(ctl proxyctl.Control) error {
	switch {
	case ctl.Port < 1 || ctl.Port > 65535:
		return errors.New("its control line has no valid port")
	case !credentialHex.MatchString(ctl.Credential):
		return errors.New("its control line has no valid credential")
	case !filepath.IsAbs(ctl.CAFile) || !filepath.IsAbs(ctl.BundleFile):
		return errors.New("its control line has no absolute CA and bundle paths")
	}
	for k, v := range ctl.Placeholders {
		if !proxyctl.PlaceholderPattern.MatchString(k) || v == "" || strings.ContainsRune(v, 0) {
			return fmt.Errorf("its control line has an invalid placeholder %s", k)
		}
	}
	return nil
}

// Stop closes the proxy's stdin, which ends its session, and kills it if it has not exited
// within timeout; now kills it at once. It reports whether the timeout killed it.
func Stop(p *Process, timeout time.Duration, now bool) (killed bool) {
	_ = p.Stdin.Close()
	if now {
		_ = p.Kill()
	}
	done := make(chan struct{})
	go func() {
		// The proxy's exit status changes nothing for its owner.
		_ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(timeout):
		_ = p.Kill()
		<-done
		return true
	}
}
