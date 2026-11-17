// Package cli holds the berth command logic, so cmd/berth stays a thin entry point and tests can
// drive every command against an in-process server.
package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/client"
	"berth/internal/mask"
)

// Exit codes. berth run, once its command has started, exits with the command's own code instead.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitDenied      = 3
	ExitFailed      = 4
	ExitNoGrant     = 5
	ExitStaleGrant  = 6
	ExitBusy        = 7
	ExitProxyFailed = 8
	connectTimeout  = 5 * time.Second
	statusTimeout   = 5 * time.Second
	addReason       = "added by you from the terminal"
	maskReplacement = mask.Replacement
)

const usage = `usage:
  berth status                                  show the daemon's state
  berth debug state <entity> [-n N]             show an entity's state and recent transitions
  berth secret request <name> --why "<reason>"  ask the person to store a secret; prints only "saved <name>"
  berth secret add <name>                       store a secret yourself through the native dialog
  berth secret ls [--json]                      list secrets and their consumers, never values
  berth secret grant <name> --exe <path> [--args <glob>] [--pass-env NAME ...] [--allow-interpreter]
                                                let one executable use a secret, after Touch ID
  berth secret proxy <name> --host <host> --header <Header> [--port N] [--prefix S] [--suffix S]
                     [--path <glob>] [--method M ...] [--placeholder-env NAME]
                                                let berth-proxy write a secret into one header of
                                                HTTPS requests to one host, after Touch ID; for
                                                example --header Authorization --prefix "Bearer "
  berth secret revoke <grant id>                revoke one grant and stop proxies using it
  berth secret rm <name>                        delete a secret after Touch ID and revoke its grants
  berth secret rotate <name>                    replace a secret's value after Touch ID; grants stay
  berth run --env KEY=<name> [--env ...] -- <command> [args...]
                                                run a command with secrets in its environment,
                                                masked as **** in its output; the command gets
                                                HOME, PATH, TERM, LANG, LC_*, USER, LOGNAME, SHELL,
                                                TMPDIR, the secrets, and only the variables its
                                                grant lists with --pass-env
  berth run --proxy <name> [--proxy ...] -- <command> [args...]
                                                run a command through berth-proxy, which adds each
                                                secret to requests to its granted host; the command
                                                never holds a value and gets your environment with
                                                HTTPS_PROXY and the CA variables set by Berth and
                                                NO_PROXY, HTTP_PROXY and ALL_PROXY removed;
                                                --proxy and --env cannot be combined
  berth ps [--all] [--json]                     list every Chromium family browser on this Mac,
                                                what drives it and who started it; --all adds
                                                browsers that stopped since berthd started
  berth inspect <session id> [-n N]             show one browser's processes, owner chain and
                                                recent transitions
  berth stop <session id> [--force]             stop a browser: SIGTERM, then SIGKILL after 5 s;
                                                one you or an app started needs --force, and
                                                another user's is never stopped
  berth leftovers [--stop [--force]]           list, or stop, browsers whose owner exited
  berth top                                     a live full screen view of every browser session:
                                                inspect, stop (after a confirmation) and filter
  berth agent session new [--name N] [--ttl D]  issue an agent session and print its token once;
                                                default ttl 12h, at most 168h
  berth agent session ls [--json]               list agent sessions, never tokens
  berth agent session revoke <id>               end an agent session; its token is refused
  berth browser launch <profile> [--headless] [--executable <path>] [--token-file <path>]
                                                start Chromium over a private pipe on a Berth
                                                profile; prints its CDP proxy endpoint and the
                                                token, which goes in the X-Berth-Token header
  berth browser ls [--all] [--json]             list the browsers Berth launched
  berth browser close <browser id>              close one: Browser.close, then SIGKILL after 5 s

agent mode: with BERTH_AGENT_SOCKET set, berth talks to that agent socket and sends the token
read from the file BERTH_AGENT_TOKEN_FILE names (only its owner may read it); the token is never
an argument

exit codes:
  0  success
  1  error: berthd unreachable or it refused the request (the message says why)
  2  usage error
  3  denied by the person
  4  the store or grant failed
  5  run: no grant lets this command use the secret; the command did not start
  6  run: the command changed since it was granted; the command did not start
  7  the secret is busy with another request waiting for the person; try again later
  8  run --proxy: berth-proxy did not start or answered badly; the command did not start
  Once its command starts, berth run exits with the command's own exit code.
`

// ValueReader turns a single use ticket into the secret's value. The caller owns the returned
// slice and zeroes it; the value must never be converted to a string.
type ValueReader func(ctx context.Context, ticket []byte) ([]byte, error)

// Env is everything a command touches outside the process, so tests can replace all of it.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Connect func(ctx context.Context) (*client.Client, error)
	// ConnectTrusted is Connect, refusing any server but the signed berthd; berth run uses it,
	// since it hands what berthd answers to berth-native.
	ConnectTrusted func(ctx context.Context) (*client.Client, error)
	// Read reads a value through berth-native (cmd/berth wires native.Read); tests leave it nil or
	// fake it, and berth run --env refuses without it.
	Read ValueReader
	// StartProxy starts berth-proxy for berth run --proxy, which refuses without it.
	StartProxy ProxyStarter
	// Agent is set when Connect reaches the agent socket; berth run --proxy then asks berthd to
	// start berth-proxy instead of starting it itself.
	Agent bool
	// BeforeStart runs just before berth run --env starts its child; only berth_e2e builds set
	// it, so the end to end tests can swap the file after berthd checked it.
	BeforeStart func(exe string) error
}

// Run executes one berth command and returns its exit code.
func Run(ctx context.Context, args []string, env Env) int {
	switch {
	case len(args) == 1 && args[0] == "status":
		return status(ctx, env)
	case len(args) >= 3 && args[0] == "debug" && args[1] == "state":
		return debugState(ctx, args[2], args[3:], env)
	case len(args) >= 2 && args[0] == "secret":
		switch args[1] {
		case "request":
			return secretRequest(ctx, args[2:], env)
		case "add":
			return secretAdd(ctx, args[2:], env)
		case "ls":
			return secretList(ctx, args[2:], env)
		case "grant":
			return secretGrant(ctx, args[2:], env)
		case "proxy":
			return secretProxy(ctx, args[2:], env)
		case "revoke":
			return secretRevoke(ctx, args[2:], env)
		case "rm":
			return secretRemove(ctx, args[2:], env)
		case "rotate":
			return secretRotate(ctx, args[2:], env)
		}
	case len(args) >= 1 && args[0] == "run":
		return runCommand(ctx, args[1:], env)
	case len(args) >= 1 && args[0] == "ps":
		return sessionList(ctx, args[1:], env)
	case len(args) >= 1 && args[0] == "inspect":
		return sessionInspect(ctx, args[1:], env)
	case len(args) >= 1 && args[0] == "stop":
		return sessionStop(ctx, args[1:], env)
	case len(args) >= 1 && args[0] == "leftovers":
		return sessionLeftovers(ctx, args[1:], env)
	case len(args) == 1 && args[0] == "top":
		return top(ctx, env)
	case len(args) >= 1 && args[0] == "agent":
		return agentCommand(ctx, args[1:], env)
	case len(args) >= 1 && args[0] == "browser":
		return browserCommand(ctx, args[1:], env)
	}
	fmt.Fprint(env.Stderr, usage)
	return ExitUsage
}

func connectClient(ctx context.Context, env Env) (*client.Client, bool) {
	return connectWith(ctx, env, env.Connect)
}

func connectWith(ctx context.Context, env Env, connect func(context.Context) (*client.Client, error)) (*client.Client, bool) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	c, err := connect(ctx)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return nil, false
	}
	return c, true
}

func status(ctx context.Context, env Env) int {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	s, err := c.Daemon.Status(ctx, &berthv1.StatusRequest{})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "state\t%s (v%d)\n", s.GetState(), s.GetStateVersion())
	fmt.Fprintf(w, "pid\t%d\n", s.GetPid())
	fmt.Fprintf(w, "version\t%s\n", s.GetDaemonVersion())
	fmt.Fprintf(w, "started\t%s\n", s.GetStartedAt().AsTime().Local().Format(time.DateTime))
	fmt.Fprintf(w, "socket\t%s\n", s.GetSocketPath())
	_ = w.Flush()
	return ExitOK
}

func debugState(ctx context.Context, entity string, args []string, env Env) int {
	fs := newFlags("debug state", env)
	limit := fs.Uint("n", 10, "how many transitions to show, at most 500")
	if err := fs.Parse(args); err != nil || *limit > 500 {
		return ExitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	s, err := c.Debug.GetState(ctx, &berthv1.GetStateRequest{Entity: entity, HistoryLimit: uint32(*limit)}) //nolint:gosec // bounded to 500 above
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "%s (%s): %s, version %d\n\n", s.GetEntity(), s.GetKind(), s.GetState(), s.GetVersion())
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "V\tWHEN\tEVENT\tFROM\tTO\tDETAIL")
	for _, t := range s.GetHistory() {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.GetVersion(), t.GetAt().AsTime().Local().Format(time.TimeOnly),
			t.GetEvent(), t.GetFromState(), t.GetToState(), t.GetDetail())
	}
	_ = w.Flush()
	return ExitOK
}

func newFlags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// parseNamed accepts the one positional name either before or after the flags.
func parseNamed(fs *flag.FlagSet, args []string) (string, bool) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
		return name, fs.NArg() == 1
	}
	return name, name != "" && fs.NArg() == 0
}

func requestID() string { return rand.Text() }

// reportError prints one plain line for a failed call and returns the exit code for it. target
// is the executable or host the call named.
func reportError(w io.Writer, err error, name, target string) int {
	var line string
	switch api.ReasonOf(err) {
	case berthv1.ErrorReason_ERROR_REASON_SECRET_EXISTS:
		line = fmt.Sprintf("secret %s already exists; use it with berth run, or grant a command with berth secret grant", name)
	case berthv1.ErrorReason_ERROR_REASON_SECRET_NOT_FOUND:
		line = fmt.Sprintf("no stored secret named %s; ask the person to store it with berth secret add %s", name, name)
	case berthv1.ErrorReason_ERROR_REASON_EXECUTABLE_NOT_FOUND:
		line = fmt.Sprintf("berthd cannot read the executable %s", target)
	case berthv1.ErrorReason_ERROR_REASON_REQUEST_ID_REUSED:
		line = "berthd saw this request id before with different input; try again"
	case berthv1.ErrorReason_ERROR_REASON_NOT_SERVING:
		line = "berthd is not serving right now; try again once berth status shows Serving"
	case berthv1.ErrorReason_ERROR_REASON_PROTOCOL_MISMATCH:
		line = "berth and berthd are different versions; restart berthd"
	case berthv1.ErrorReason_ERROR_REASON_TICKET_INVALID:
		line = "the use ticket was invalid, used or expired; run the command again"
	case berthv1.ErrorReason_ERROR_REASON_PEER_NOT_TRUSTED:
		line = "berthd gives secrets only to the signed berth binary; run make build and use bin/berth beside bin/berthd"
	case berthv1.ErrorReason_ERROR_REASON_INTERPRETER:
		line = fmt.Sprintf("%s is an interpreter, so anything it runs could print the secret; add --allow-interpreter only if that is what you want", target)
	case berthv1.ErrorReason_ERROR_REASON_BUSY:
		fmt.Fprintf(w, "secret %s is busy: another request waits for the person; try again when it finishes\n", name)
		return ExitBusy
	case berthv1.ErrorReason_ERROR_REASON_BAD_PROXY_TARGET:
		line = fmt.Sprintf("berthd will not proxy to %s: use a DNS host name, not an IP address or localhost", target)
	case berthv1.ErrorReason_ERROR_REASON_GRANT_NOT_FOUND:
		line = fmt.Sprintf("no grant has id %s; berth secret ls lists the grants", target)
	case berthv1.ErrorReason_ERROR_REASON_NOT_AGENT_METHOD:
		line = "this command is for the person, not for agents; agents reach berthd only through the agent socket's methods"
	case berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_FOUND:
		line = fmt.Sprintf("no browser session has id %s; berth ps lists them", target)
	case berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_STOPPABLE:
		line = fmt.Sprintf("berth will not stop session %s: it belongs to another user, or to the person or an app and needs --force", target)
	case berthv1.ErrorReason_ERROR_REASON_AGENT_TOKEN_INVALID:
		line = "berthd refused the agent token: it is missing, unknown, expired or revoked, or this process is outside the session's process tree; ask the person for a new session"
	case berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED:
		line = "this agent session has too many store requests waiting for the person, or made 10 in the last hour; wait, then try again"
	case berthv1.ErrorReason_ERROR_REASON_AGENT_SESSION_NOT_FOUND:
		line = fmt.Sprintf("no agent session has id %s; berth agent session ls lists them", target)
	case berthv1.ErrorReason_ERROR_REASON_PROFILE_BUSY:
		line = fmt.Sprintf("profile %s is in use by a live browser; berth browser ls lists them", target)
	case berthv1.ErrorReason_ERROR_REASON_BROWSER_NOT_FOUND:
		line = fmt.Sprintf("no managed browser has id %s; berth browser ls lists them", target)
	case berthv1.ErrorReason_ERROR_REASON_BROWSER_LAUNCH_FAILED:
		line = err.Error()
	case berthv1.ErrorReason_ERROR_REASON_UNSPECIFIED:
		line = err.Error()
	}
	if line == "" {
		line = err.Error()
	}
	fmt.Fprintln(w, line)
	return ExitError
}

var errNoReader = errors.New("berth run cannot read secrets: this build has no berth-native reader")
