package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/envpolicy"
	"berth/internal/core/ports"
	"berth/internal/mask"
	"berth/internal/spawn"
)

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type envBinding struct{ key, name string }

type envFlags []envBinding

func (e *envFlags) String() string { return "" }

func (e *envFlags) Set(s string) error {
	key, name, ok := strings.Cut(s, "=")
	if !ok || !envKey.MatchString(key) || name == "" {
		return fmt.Errorf("want KEY=<secret name>, got %q", s)
	}
	for _, b := range *e {
		if b.key == key {
			return fmt.Errorf("%s is set twice", key)
		}
	}
	*e = append(*e, envBinding{key: key, name: name})
	return nil
}

func runCommand(ctx context.Context, args []string, env Env) int {
	fs := newFlags("run", env)
	var bindings envFlags
	fs.Var(&bindings, "env", "KEY=<secret name>; repeat for more secrets")
	var proxies proxyNames
	fs.Var(&proxies, "proxy", "a secret berth-proxy writes into requests to its granted host; repeat for more")
	// --env and --proxy together would put a value in a child that also holds the proxy's
	// credential; E2b keeps the two modes apart.
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 || (len(bindings) == 0) == (len(proxies) == 0) {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	if len(proxies) > 0 {
		exe, err := resolveExecutable(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return ExitError
		}
		return runProxied(ctx, proxies, exe, fs.Args()[1:], env)
	}
	if env.Read == nil {
		fmt.Fprintln(env.Stderr, errNoReader)
		return ExitError
	}
	exe, err := resolveExecutable(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	argv := fs.Args()[1:]
	if env.ConnectTrusted == nil {
		fmt.Fprintln(env.Stderr, "berth run needs a verified connection to berthd")
		return ExitError
	}
	c, ok := connectWith(ctx, env, env.ConnectTrusted)
	if !ok {
		return ExitError
	}

	// Every grant is checked before any value is read, so a refusal never leaves a value behind.
	tickets := make([][]byte, len(bindings))
	// A variable passes only when every grant used here lists it.
	var passEnv []string
	var pinned []byte
	for i, b := range bindings {
		resp, err := c.Secrets.AuthorizeUse(ctx, &berthv1.AuthorizeUseRequest{RequestId: requestID(), Name: b.name, ExePath: exe, Argv: argv})
		if err != nil {
			return reportError(env.Stderr, err, b.name, exe)
		}
		switch resp.GetOutcome() {
		case berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED:
			tickets[i] = resp.GetTicket()
			// Every grant here pins the same file, so they pin one image; one that pins another,
			// or none, cannot be checked against the child.
			if len(resp.GetCdhash()) != len(ports.CDHash{}) || (i > 0 && !bytes.Equal(resp.GetCdhash(), pinned)) {
				fmt.Fprintf(env.Stderr, "berthd pinned no single code identity for %s; ask the person to run: berth secret grant %s --exe %s\n",
					exe, b.name, exe)
				return ExitStaleGrant
			}
			pinned = resp.GetCdhash()
			if i == 0 {
				passEnv = resp.GetPassEnv()
			} else {
				passEnv = slices.DeleteFunc(passEnv, func(n string) bool { return !slices.Contains(resp.GetPassEnv(), n) })
			}
			continue
		case berthv1.UseOutcome_USE_OUTCOME_NO_GRANT:
			fmt.Fprintf(env.Stderr, "no grant lets %s use %s with these arguments; ask the person to run: berth secret grant %s --exe %s\n",
				exe, b.name, b.name, exe)
			return ExitNoGrant
		case berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT:
			fmt.Fprintf(env.Stderr, "%s changed since it was granted %s; ask the person to run: berth secret grant %s --exe %s\n",
				exe, b.name, b.name, exe)
			fmt.Fprintln(env.Stderr, "grants made before this version of berth go stale at their first use and need one fresh berth secret grant")
			return ExitStaleGrant
		case berthv1.UseOutcome_USE_OUTCOME_UNSPECIFIED:
		}
		fmt.Fprintf(env.Stderr, "berthd gave no outcome for %s\n", b.name)
		return ExitError
	}

	values := make([][]byte, 0, len(bindings))
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	for i, b := range bindings {
		v, err := env.Read(ctx, tickets[i])
		if err != nil {
			fmt.Fprintf(env.Stderr, "cannot read secret %s: %v\n", b.name, err)
			return ExitError
		}
		values = append(values, v)
	}
	var want ports.CDHash
	copy(want[:], pinned)
	return startVerified(ctx, exe, argv, envpolicy.Child(os.Environ(), passEnv), bindings, values, want, env)
}

// startVerified starts exe suspended with childEnv plus the secrets, and lets it run only if the
// kernel reports the cdhash berthd pinned when the person granted it: a file swapped after
// AuthorizeUse hashed it is killed before its first instruction. The secrets stay []byte all the
// way into the child's environment, and childEnv comes from envpolicy, never berth's own.
func startVerified(ctx context.Context, exe string, argv, childEnv []string, bindings envFlags, values [][]byte, want ports.CDHash, env Env) int {
	// A C environment ends each entry at a NUL, so such a value would reach the child cut short.
	for i, v := range values {
		if bytes.IndexByte(v, 0) >= 0 {
			fmt.Fprintf(env.Stderr, "secret %s contains a NUL byte, which cannot go into an environment variable; nothing started\n", bindings[i].name)
			return ExitError
		}
	}
	stdout, stderr := mask.NewWriter(env.Stdout, values), mask.NewWriter(env.Stderr, values)
	defer func() { _ = stdout.Close(); _ = stderr.Close() }()
	entries := make([][]byte, 0, len(childEnv)+len(bindings))
	for _, e := range childEnv {
		entries = append(entries, []byte(e))
	}
	for i, b := range bindings {
		entries = append(entries, append([]byte(b.key+"="), values[i]...))
	}
	wipe := func() {
		for _, e := range entries {
			clear(e)
		}
		for _, v := range values {
			clear(v)
		}
	}
	defer wipe()
	std, err := newStdio(env.Stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cannot start %s: %v\n", exe, err)
		return ExitError
	}
	defer std.close()
	if env.BeforeStart != nil {
		if err := env.BeforeStart(exe); err != nil {
			fmt.Fprintf(env.Stderr, "cannot start %s: %v\n", exe, err)
			return ExitError
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	pid, err := spawn.StartSuspended(exe, append([]string{exe}, argv...), entries, std.child)
	std.started()
	wipe()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cannot start %s: %v\n", exe, err)
		return ExitError
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cannot find %s after starting it: %v\n", exe, err)
		return ExitError
	}
	if live, err := childCode(pid); err != nil || live.CDHash != want || !live.Valid() {
		_ = proc.Kill()
		_, _ = proc.Wait()
		std.wait()
		fmt.Fprintf(env.Stderr, "%s is not the code that was granted; berth killed it before it ran. Ask the person to check the file, then run: berth secret grant %s --exe %s\n",
			exe, bindings[0].name, exe)
		return ExitStaleGrant
	}
	if err := spawn.Resume(pid); err != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
		std.wait()
		fmt.Fprintf(env.Stderr, "cannot resume %s: %v\n", exe, err)
		return ExitError
	}
	stop := forwardSignals(ctx, proc, signals)
	state, err := proc.Wait()
	stop()
	std.wait()
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	return exitStatus(state)
}

// childCode is spawn.Live, replaced in a test to show a child whose code signature is not valid.
var childCode = spawn.Live

// stdio gives a suspended child real descriptors: an *os.File is handed over as it is, and any
// other reader or writer gets a pipe and a copying goroutine, as exec.Cmd does.
type stdio struct {
	child [3]*os.File
	// mine are the pipe ends berth keeps; theirs are closed once the child holds its copies.
	mine, theirs []*os.File
	copies       []chan struct{}
}

func newStdio(in io.Reader, out, errOut io.Writer) (*stdio, error) {
	s := &stdio{}
	if f, ok := in.(*os.File); ok {
		s.child[0] = f
	} else {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.child[0], s.theirs, s.mine = r, append(s.theirs, r), append(s.mine, w)
		go func() { _, _ = io.Copy(w, in); _ = w.Close() }()
	}
	for i, dst := range []io.Writer{out, errOut} {
		r, w, err := os.Pipe()
		if err != nil {
			s.close()
			return nil, err
		}
		s.child[i+1], s.theirs, s.mine = w, append(s.theirs, w), append(s.mine, r)
		done := make(chan struct{})
		s.copies = append(s.copies, done)
		go func() { _, _ = io.Copy(dst, r); close(done) }()
	}
	return s, nil
}

// started closes berth's copies of the child's ends, so output ends when the child's does.
func (s *stdio) started() {
	for _, f := range s.theirs {
		_ = f.Close()
	}
	s.theirs = nil
}

// wait returns once the child's output has been copied.
func (s *stdio) wait() {
	s.started()
	for _, done := range s.copies {
		<-done
	}
}

func (s *stdio) close() {
	s.started()
	for _, f := range s.mine {
		_ = f.Close()
	}
}

// startChild runs a proxied child, which holds no secret, with childEnv.
func startChild(ctx context.Context, exe string, argv, childEnv []string, env Env) int {
	cmd := exec.CommandContext(ctx, exe, argv...) //nolint:gosec // running the person's chosen command is what berth run is for
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
	cmd.Env = slices.Clone(childEnv)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(env.Stderr, "cannot start %s: %v\n", exe, err)
		return ExitError
	}
	stop := forwardSignals(ctx, cmd.Process, signals)
	err := cmd.Wait()
	stop()
	return exitCode(err, env)
}

// forwardSignals passes SIGTERM on to the child until stop is called, and kills the child if
// ctx ends first.
func forwardSignals(ctx context.Context, p *os.Process, signals <-chan os.Signal) (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-signals:
				if forwarded(s) {
					_ = p.Signal(s)
				}
			case <-ctx.Done():
				_ = p.Kill()
				return
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// forwarded reports whether berth passes a signal on to its child. The child shares berth's
// process group (it stays in the terminal's foreground group so it can read the terminal), so
// a terminal Ctrl-C already reaches it; forwarding SIGINT too would deliver it twice. berth
// still catches SIGINT so it outlives the child and reports the child's exit.
func forwarded(s os.Signal) bool { return s == syscall.SIGTERM }

func exitCode(err error, env Env) int {
	if err == nil {
		return ExitOK
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	return exitStatus(exitErr.ProcessState)
}

func exitStatus(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}
