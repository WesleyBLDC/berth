// Package native is the Go side of the berth-native helper protocol (docs/plans/e2-terminal-secrets.md).
// It starts the helper once per request with a scrubbed environment and never sees a value except
// through Read, which hands the bytes straight to its caller.
package native

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
	"berth/internal/strictjson"
)

// DefaultHelper is where the signed berth-native helper is installed.
const DefaultHelper = "/Applications/Berth.app/Contents/MacOS/berth-native"

const (
	// maxOutcome bounds a store or confirm reply; a real one is a few dozen bytes.
	maxOutcome = 64 << 10
	// maxValue bounds a secret read, so a misbehaving helper cannot exhaust memory.
	maxValue = 1 << 20
	// maxStderr is how much of the helper's stderr (diagnostics, never a value) an error keeps.
	maxStderr = 512
	// waitDelay lets a killed helper's pipes close instead of blocking Wait forever.
	waitDelay   = 2 * time.Second
	readRefused = 3
)

// Approver implements ports.Approver by running the helper's store, confirm, rotate and remove
// commands.
type Approver struct {
	helper string
}

// New returns an Approver that runs the helper at path; ctx deadlines bound each run.
func New(helper string) *Approver { return &Approver{helper: helper} }

var _ ports.Approver = (*Approver)(nil)

type process struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

type requester struct {
	PID        int       `json:"pid"`
	Executable string    `json:"executable"`
	Tree       []process `json:"tree"`
	WorkingDir string    `json:"working_dir"`
}

type storeRequest struct {
	Name      string    `json:"name"`
	Reason    string    `json:"reason"`
	Requester requester `json:"requester"`
}

type consumer struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	TeamID    string `json:"team_id"`
	SigningID string `json:"signing_id"`
	Signature string `json:"signature"`
}

type confirmRequest struct {
	Action      string    `json:"action"`
	Secret      string    `json:"secret"`
	Consumer    consumer  `json:"consumer"`
	ArgsGlob    string    `json:"args_glob"`
	Interpreter bool      `json:"interpreter"`
	PassEnv     []string  `json:"pass_env"`
	Requester   requester `json:"requester"`
}

type proxyTarget struct {
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	HeaderName     string   `json:"header_name"`
	ValuePrefix    string   `json:"value_prefix"`
	ValueSuffix    string   `json:"value_suffix"`
	PathGlob       string   `json:"path_glob"`
	Methods        []string `json:"methods"`
	PlaceholderEnv string   `json:"placeholder_env"`
}

type proxyConfirmRequest struct {
	Action    string      `json:"action"`
	Secret    string      `json:"secret"`
	Target    proxyTarget `json:"target"`
	Requester requester   `json:"requester"`
}

type outcome struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

func toRequester(r ports.Requester) requester {
	tree := make([]process, 0, len(r.Tree))
	for _, p := range r.Tree {
		tree = append(tree, process{PID: p.PID, Executable: p.Executable})
	}
	return requester{PID: r.PID, Executable: r.Executable, Tree: tree, WorkingDir: r.WorkingDir}
}

func (a *Approver) StoreSecret(ctx context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	req := storeRequest{Name: p.Name, Reason: p.Reason, Requester: toRequester(p.Requester)}
	out, err := a.ask(ctx, "store", req)
	return result("store", out, err, ports.StoreFailed, ports.StoreStored, ports.StoreDenied)
}

func (a *Approver) ConfirmPower(ctx context.Context, p ports.PowerPrompt) (ports.PowerOutcome, error) {
	out, err := a.ask(ctx, "confirm", confirmLine(p))
	return result("confirm", out, err, ports.PowerFailed, ports.PowerApproved, ports.PowerDenied)
}

// confirmLine is the confirm request for the prompt's action; a proxy grant names its target
// instead of a consumer executable.
func confirmLine(p ports.PowerPrompt) any {
	if p.Action == ports.PowerProxyGrant {
		t := p.Proxy
		return proxyConfirmRequest{
			Action: string(p.Action),
			Secret: p.Secret,
			Target: proxyTarget{
				Host:           t.Host,
				Port:           t.Port,
				HeaderName:     t.HeaderName,
				ValuePrefix:    t.ValuePrefix,
				ValueSuffix:    t.ValueSuffix,
				PathGlob:       t.PathGlob,
				Methods:        append([]string{}, t.Methods...),
				PlaceholderEnv: t.PlaceholderEnv,
			},
			Requester: toRequester(p.Requester),
		}
	}
	return confirmRequest{
		Action: string(p.Action),
		Secret: p.Secret,
		Consumer: consumer{
			Path:      p.Consumer.Path,
			SHA256:    hex.EncodeToString(p.Consumer.SHA256[:]),
			TeamID:    p.Consumer.TeamID,
			SigningID: p.Consumer.SigningID,
			Signature: string(p.Consumer.Signature),
		},
		ArgsGlob:    p.ArgsGlob,
		Interpreter: p.Interpreter,
		PassEnv:     append([]string{}, p.PassEnv...),
		Requester:   toRequester(p.Requester),
	}
}

// ask runs one command and returns its parsed reply; an error means the helper crashed, exited
// non-zero, timed out or replied with something that is not one outcome line.
func (a *Approver) ask(ctx context.Context, command string, req any) (outcome, error) {
	line, err := json.Marshal(req)
	if err != nil {
		return outcome{}, fmt.Errorf("native %s: encode request: %w", command, err)
	}
	cmd := helperCommand(ctx, a.helper, command)
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxOutcome}
	cmd.Stderr = &limitedWriter{w: &stderr, n: maxStderr}
	if err := cmd.Run(); err != nil {
		return outcome{}, runError(ctx, command, err, stderr.Bytes())
	}
	out, err := parseOutcome(stdout.Bytes())
	if err != nil {
		return outcome{}, fmt.Errorf("native %s: %w", command, err)
	}
	return out, nil
}

// result turns one command's reply into its outcome: failed always comes with an error, and an
// outcome the command does not define counts as failed.
func result[O ~string](command string, out outcome, err error, failed O, fine ...O) (O, error) {
	if err != nil {
		return failed, err
	}
	switch o := O(out.Outcome); {
	case o == failed:
		return o, out.failure(command)
	case slices.Contains(fine, o):
		return o, nil
	}
	return failed, fmt.Errorf("native %s: unknown outcome %q", command, out.Outcome)
}

// failure is the error that goes with a failed outcome, so the caller always has log detail.
func (o outcome) failure(command string) error {
	if o.Error == "" {
		return fmt.Errorf("native %s failed", command)
	}
	return fmt.Errorf("native %s failed: %s", command, o.Error)
}

// parseOutcome accepts exactly one JSON object on one line, with or without its newline.
func parseOutcome(b []byte) (outcome, error) {
	b = bytes.TrimSuffix(b, []byte("\n"))
	if len(b) == 0 {
		return outcome{}, errors.New("empty reply")
	}
	if bytes.IndexByte(b, '\n') >= 0 {
		return outcome{}, errors.New("reply is more than one line")
	}
	var out outcome
	if err := strictjson.Decode(b, &out); err != nil {
		return outcome{}, errors.New("malformed reply")
	}
	return out, nil
}

// Reader is the Read berth and berth-proxy hand their runs: it reads the value with the ticket
// berthd sent, through the helper path names. Which file that is does not decide who gets the
// value: berth-native writes it only over a socket whose peer the kernel shows is its parent,
// running the signed caller.
func Reader(path func() (string, error)) func(ctx context.Context, ticket []byte) ([]byte, error) {
	return func(ctx context.Context, ticket []byte) ([]byte, error) {
		var t ports.Ticket
		if len(ticket) != len(t) {
			return nil, errors.New("berthd sent a ticket of the wrong length")
		}
		copy(t[:], ticket)
		helper, err := path()
		if err != nil {
			return nil, err
		}
		return Read(ctx, helper, t)
	}
}

// ValueFD is the file descriptor on which berth-native read writes the value: one end of a
// Unix socketpair whose other end berth holds, so the helper can ask the kernel who will
// receive the value before it writes. stdout stays free and carries nothing.
const ValueFD = 3

// Read runs `<helper> read` with the ticket and returns the value bytes, which the caller zeroes
// when done. The value arrives only over the socketpair on ValueFD; no error ever includes
// anything the helper wrote.
func Read(ctx context.Context, helper string, ticket ports.Ticket) ([]byte, error) {
	in := make([]byte, hex.EncodedLen(len(ticket))+1)
	hex.Encode(in, ticket[:])
	in[len(in)-1] = '\n'
	// Like os.Pipe: hold ForkLock until both ends are close-on-exec, so no concurrent exec
	// inherits either. ExtraFiles clears the flag on theirs for the helper alone.
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("native read: socketpair: %w", err)
	}
	mine := os.NewFile(uintptr(fds[0]), "berth-value")
	theirs := os.NewFile(uintptr(fds[1]), "berth-native-value")
	defer func() { _ = mine.Close() }()
	cmd := helperCommand(ctx, helper, "read")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: maxStderr}
	cmd.ExtraFiles = []*os.File{theirs} // becomes ValueFD in the helper
	if err := cmd.Start(); err != nil {
		_ = theirs.Close()
		return nil, fmt.Errorf("native read: start helper: %w", err)
	}
	// Only the helper may hold the other end, so reading reaches end of file when it exits.
	_ = theirs.Close()
	value, readErr := readAllZeroing(mine, maxValue)
	waitErr := cmd.Wait()
	switch {
	case waitErr != nil:
		clear(value)
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) && exit.ExitCode() == readRefused {
			return nil, errors.New("native read: helper refused the ticket")
		}
		return nil, runError(ctx, "read", waitErr, stderr.Bytes())
	case readErr != nil:
		clear(value)
		return nil, fmt.Errorf("native read: %w", readErr)
	case len(value) == 0:
		return nil, errors.New("native read: helper returned no value")
	}
	return value, nil
}

// readAllZeroing reads r to the end. When the buffer grows it zeroes the old one, so no stray
// copy of the value is left for the garbage collector.
func readAllZeroing(r io.Reader, limit int) ([]byte, error) {
	buf := make([]byte, 0, 512)
	for {
		if len(buf) == cap(buf) {
			if len(buf) >= limit {
				clear(buf)
				return nil, fmt.Errorf("value larger than %d bytes", limit)
			}
			bigger := make([]byte, len(buf), min(2*cap(buf), limit+1))
			copy(bigger, buf)
			clear(buf)
			buf = bigger
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			if len(buf) > limit {
				clear(buf)
				return nil, fmt.Errorf("value larger than %d bytes", limit)
			}
			return buf, nil
		}
		if err != nil {
			clear(buf)
			return nil, err
		}
	}
}

func helperCommand(ctx context.Context, helper, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, helper, command) //nolint:gosec // the helper path is Berth's own configuration, never a caller's
	cmd.Env = scrubbedEnv()
	cmd.WaitDelay = waitDelay
	return cmd
}

// scrubbedEnv passes only what the protocol allows, so nothing from the daemon's environment
// reaches the helper.
func scrubbedEnv() []string {
	var env []string
	for _, k := range []string{"HOME", "PATH", "BERTH_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func runError(ctx context.Context, command string, err error, stderr []byte) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("native %s: %w", command, ctxErr)
	}
	stderr = bytes.TrimSpace(stderr)
	if len(stderr) == 0 {
		return fmt.Errorf("native %s: %w", command, err)
	}
	return fmt.Errorf("native %s: %w: %s", command, err, stderr)
}

// limitedWriter keeps the first n bytes and discards the rest without failing the child.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	keep := min(len(p), l.n)
	if keep > 0 {
		if _, err := l.w.Write(p[:keep]); err != nil {
			return 0, err
		}
		l.n -= keep
	}
	return len(p), nil
}
