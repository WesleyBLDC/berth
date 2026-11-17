package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/envpolicy"
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
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 || len(bindings) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
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
	// The helper is checked before AuthorizeUse, so a refused helper never spends a ticket.
	if env.PrepareRead != nil {
		cleanup, err := env.PrepareRead(ctx)
		if err != nil {
			fmt.Fprintf(env.Stderr, "berth run will not start this berth-native: %v; run make native and use the bin/ folder it built\n", err)
			return ExitError
		}
		defer cleanup()
	}
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
	for i, b := range bindings {
		resp, err := c.Secrets.AuthorizeUse(ctx, &berthv1.AuthorizeUseRequest{RequestId: requestID(), Name: b.name, ExePath: exe, Argv: argv})
		if err != nil {
			return reportError(env.Stderr, err, b.name, exe)
		}
		switch resp.GetOutcome() {
		case berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED:
			tickets[i] = resp.GetTicket()
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
	return startChild(ctx, exe, argv, envpolicy.Child(os.Environ(), passEnv), bindings, values, env)
}

// startChild runs exe with childEnv plus the secrets; childEnv comes from envpolicy, never the
// whole environment berth was started with.
func startChild(ctx context.Context, exe string, argv, childEnv []string, bindings envFlags, values [][]byte, env Env) int {
	stdout := newMasker(env.Stdout, values)
	stderr := newMasker(env.Stderr, values)
	defer func() { _ = stdout.Close(); _ = stderr.Close() }()

	cmd := exec.CommandContext(ctx, exe, argv...) //nolint:gosec // running the person's chosen command is what berth run is for
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, stdout, stderr
	cmd.Env = slices.Clone(childEnv)
	for i, b := range bindings {
		cmd.Env = append(cmd.Env, envEntry(b.key, values[i]))
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	err := cmd.Start()
	cmd.Env = nil
	for _, v := range values {
		clear(v)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "cannot start %s: %v\n", exe, err)
		return ExitError
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-signals:
				if forwarded(s) {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	return exitCode(cmd.Wait(), env)
}

// forwarded reports whether berth passes a signal on to its child. The child shares berth's
// process group (it stays in the terminal's foreground group so it can read the terminal), so
// a terminal Ctrl-C already reaches it; forwarding SIGINT too would deliver it twice. berth
// still catches SIGINT so it outlives the child and reports the child's exit.
func forwarded(s os.Signal) bool { return s == syscall.SIGTERM }

// envEntry is the one place a secret value becomes a Go string: exec.Cmd.Env is a []string and
// the standard library offers no byte-slice way to hand a child its environment. The string
// cannot be zeroed, so the caller drops its reference as soon as the child has started.
func envEntry(key string, value []byte) string {
	return key + "=" + string(value)
}

func exitCode(err error, env Env) int {
	if err == nil {
		return ExitOK
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exitErr.ExitCode()
}
