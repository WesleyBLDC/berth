package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/cli"
)

// Exit codes.
const (
	ExitOK = 0
	// ExitError: berthd, berth-native or the network failed.
	ExitError = 1
	// ExitRefused: the environment, the request or a grant was refused before anything was served.
	ExitRefused = 2
	// ExitStopped: berthd stopped the session, the watch stream broke, or the CA expired.
	ExitStopped = 3
)

const (
	// readyTimeout is how long berthd has to answer SERVING; berthd itself gives up after 30 seconds.
	readyTimeout = 30 * time.Second
	// berth kills the proxy 5 seconds after closing its stdin, so closing and ending fit in that.
	endTimeout = 2 * time.Second
	closeWait  = 2 * time.Second
	maxRequest = 64 << 10
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Deps is everything Run touches outside the process, so tests can replace all of it.
type Deps struct {
	Environ []string
	// ExtraEnv extends the allowed environment; only end to end builds set it.
	ExtraEnv []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	// Connect returns a client for the signed berthd only.
	Connect func(ctx context.Context) (berthv1connect.SecretsServiceClient, error)
	// Read turns a ticket into a value through berth-native; the caller zeroes the slice.
	Read func(ctx context.Context, ticket []byte) ([]byte, error)
	// Roots nil means the system verifier.
	Roots *x509.CertPool
	Dial  DialFunc
	// AllowLocal accepts IP literal and localhost grants; only end to end builds set it.
	AllowLocal bool
	// SystemBundle is the trust store bundle.pem starts with, /etc/ssl/cert.pem in release.
	SystemBundle string
	// TempDir is where the run's folder goes; empty means os.TempDir.
	TempDir string
}

// Run is berth-proxy from start to exit.
func Run(ctx context.Context, d Deps) int {
	code, _ := run(ctx, d)
	return code
}

// run returns the grants so tests can check their values were zeroed.
func run(ctx context.Context, d Deps) (int, []*Grant) {
	if err := CheckEnv(d.Environ, d.ExtraEnv); err != nil {
		fmt.Fprintln(d.Stderr, "berth-proxy:", err)
		return ExitRefused, nil
	}
	// A core file would hold the values.
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})
	log := slog.New(slog.NewTextHandler(d.Stderr, nil))
	fail := func(code int, msg string, err error) int {
		if err != nil {
			msg += ": " + err.Error()
		}
		fmt.Fprintln(d.Stderr, "berth-proxy:", msg)
		return code
	}

	in := bufio.NewReaderSize(io.LimitReader(d.Stdin, maxRequest), 4096)
	names, err := readRequest(in)
	if err != nil {
		return fail(ExitRefused, "bad request on stdin", err), nil
	}
	cl, err := d.Connect(ctx)
	if err != nil {
		return fail(ExitError, "cannot reach the signed berthd", err), nil
	}
	resp, err := cl.AuthorizeProxy(ctx, &berthv1.AuthorizeProxyRequest{RequestId: randomHex(16), Names: names})
	if err != nil {
		return fail(ExitError, "berthd refused to authorize the proxy", err), nil
	}
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		return fail(ExitRefused, "no active proxy grant for every name; grant them with berth secret grant --proxy", nil), nil
	}
	grants, err := buildGrants(names, resp.GetTickets(), d.AllowLocal)
	if err != nil {
		return fail(ExitRefused, "berthd sent a target berth-proxy refuses", err), nil
	}
	defer zeroAll(grants)
	tickets := resp.GetTickets()
	for i, g := range grants {
		raw, err := d.Read(ctx, tickets[i].GetTicket())
		if err != nil {
			return fail(ExitError, "cannot read secret "+g.Name, err), grants
		}
		if g.value, err = lockValue(raw); err != nil {
			return fail(ExitRefused, "secret "+g.Name+" refused", err), grants
		}
		if !g.value.Locked() {
			log.Warn("could not lock a secret in memory; it may reach swap", "secret", g.Name)
		}
	}

	hosts := make([]string, 0, len(grants))
	for _, g := range grants {
		hosts = append(hosts, g.Host)
	}
	ca, err := NewCA(hosts, time.Now())
	if err != nil {
		return fail(ExitError, "cannot make the run CA", err), grants
	}
	system, err := os.ReadFile(d.SystemBundle)
	if err != nil {
		return fail(ExitError, "cannot read the system trust bundle", err), grants
	}
	dir, err := os.MkdirTemp(d.TempDir, "berth-proxy-")
	if err != nil {
		return fail(ExitError, "cannot make the run folder", err), grants
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// berth accepts only absolute paths, and TMPDIR may be relative.
	if dir, err = filepath.Abs(dir); err != nil {
		return fail(ExitError, "cannot resolve the run folder", err), grants
	}
	caFile, bundleFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "bundle.pem")
	bundle := append(append(system, '\n'), ca.PEM()...)
	if err := os.WriteFile(caFile, ca.PEM(), 0o600); err != nil {
		return fail(ExitError, "cannot write ca.pem", err), grants
	}
	if err := os.WriteFile(bundleFile, bundle, 0o600); err != nil { //nolint:gosec // bundleFile is inside the folder MkdirTemp just made
		return fail(ExitError, "cannot write bundle.pem", err), grants
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fail(ExitError, "cannot listen on loopback", err), grants
	}
	credential := randomHex(16)
	srv := NewServer(ServerConfig{Grants: grants, CA: ca, Credential: credential, Roots: d.Roots, Dial: d.Dial, Log: log})
	defer srv.Close(closeWait)

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events, err := watch(watchCtx, cl, resp.GetSessionId())
	if err != nil {
		_ = ln.Close()
		return fail(ExitError, "berthd did not start the session", err), grants
	}
	select {
	case ev := <-events:
		if ev.err != nil || ev.kind != berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING {
			_ = ln.Close()
			return fail(ExitStopped, "berthd did not let the session serve", ev.err), grants
		}
	case <-time.After(readyTimeout):
		_ = ln.Close()
		return fail(ExitError, "berthd did not answer SERVING in time", nil), grants
	case <-ctx.Done():
		_ = ln.Close()
		return fail(ExitError, "interrupted", nil), grants
	}

	placeholders := map[string]string{}
	for _, g := range grants {
		if g.Placeholder != "" {
			placeholders[g.Placeholder] = Placeholder
		}
	}
	line, err := json.Marshal(cli.ProxyControl{
		Port:         ln.Addr().(*net.TCPAddr).Port,
		Credential:   credential,
		CAFile:       caFile,
		BundleFile:   bundleFile,
		Placeholders: placeholders,
	})
	if err != nil {
		_ = ln.Close()
		return fail(ExitError, "cannot encode the control line", err), grants
	}
	if _, err := d.Stdout.Write(append(line, '\n')); err != nil {
		_ = ln.Close()
		return fail(ExitError, "cannot write the control line", err), grants
	}
	go srv.Serve(ln) //nolint:contextcheck // the server's own context, cancelled by Close, bounds every dial

	stdinClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, d.Stdin); close(stdinClosed) }()
	expiry := time.NewTimer(time.Until(ca.NotAfter()))
	defer expiry.Stop()

	code, end := waitForEnd(ctx, events, stdinClosed, expiry.C, log)
	if !srv.Close(closeWait) {
		log.Warn("some connections did not close in time; zeroing anyway")
	}
	zeroAll(grants)
	_ = os.RemoveAll(dir)
	if end {
		injected, refused := srv.Counts()
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		_, err := cl.EndProxySession(endCtx, &berthv1.EndProxySessionRequest{
			RequestId: randomHex(16),
			SessionId: resp.GetSessionId(),
			Injected:  injected,
			Refused:   refused,
		})
		cancel()
		if err != nil && code == ExitOK {
			return fail(ExitError, "berthd did not record the end of the session", err), grants
		}
	}
	return code, grants
}

// waitForEnd blocks until the run must end and says with what exit code, and whether berthd is
// there to record the end.
func waitForEnd(ctx context.Context, events <-chan event, stdinClosed <-chan struct{}, expiry <-chan time.Time, log *slog.Logger) (code int, end bool) {
	for {
		select {
		case <-stdinClosed:
			return ExitOK, true
		case ev := <-events:
			if ev.err == nil && ev.kind == berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING {
				continue
			}
			// STOP or a broken stream: fail closed. Only after a STOP is berthd there to hear the end.
			log.Info("session ended by berthd", "stopped", ev.err == nil)
			return ExitStopped, ev.err == nil
		case <-expiry:
			log.Info("the run CA expired")
			return ExitStopped, true
		case <-ctx.Done():
			return ExitError, true
		}
	}
}

func readRequest(in *bufio.Reader) ([]string, error) {
	line, err := in.ReadBytes('\n')
	if err != nil {
		return nil, errors.New(`expected one line {"secrets":[...]}`)
	}
	var req cli.ProxyRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, errors.New(`expected one line {"secrets":[...]}`)
	}
	if len(req.Secrets) == 0 || len(req.Secrets) > 16 {
		return nil, errors.New("name between 1 and 16 secrets")
	}
	seen := map[string]bool{}
	for _, n := range req.Secrets {
		if !namePattern.MatchString(n) || seen[n] {
			return nil, fmt.Errorf("bad or repeated secret name %q", n)
		}
		seen[n] = true
	}
	return req.Secrets, nil
}

// buildGrants pairs each requested name with exactly one ticket, in request order, and refuses
// two grants on one host and port, since only one header could win.
func buildGrants(names []string, tickets []*berthv1.ProxyTicket, allowLocal bool) ([]*Grant, error) {
	if len(tickets) != len(names) {
		return nil, errors.New("one ticket per name expected")
	}
	grants := make([]*Grant, 0, len(names))
	seen := map[string]bool{}
	for i, t := range tickets {
		if t.GetName() != names[i] || len(t.GetTicket()) != 32 {
			return nil, errors.New("tickets do not match the names asked for")
		}
		g, err := newGrant(t.GetName(), t.GetTarget(), allowLocal)
		if err != nil {
			return nil, err
		}
		if seen[g.authority()] {
			return nil, fmt.Errorf("two grants on %s", g.authority())
		}
		seen[g.authority()] = true
		grants = append(grants, g)
	}
	return grants, nil
}

type event struct {
	kind berthv1.ProxySessionEventKind
	err  error
}

// watch opens the session stream and delivers its events; a broken stream arrives as an error.
func watch(ctx context.Context, cl berthv1connect.SecretsServiceClient, session string) (<-chan event, error) {
	stream, err := cl.WatchProxySession(ctx, &berthv1.WatchProxySessionRequest{SessionId: session})
	if err != nil {
		return nil, err
	}
	out := make(chan event, 2)
	send := func(ev event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer func() { _ = stream.Close() }()
		for {
			msg, err := stream.Receive()
			if err != nil {
				send(event{err: fmt.Errorf("watch stream ended: %w", err)})
				return
			}
			kind := msg.GetEvent().GetKind()
			switch kind {
			case berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING:
				if !send(event{kind: kind}) {
					return
				}
			case berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP,
				berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_UNSPECIFIED:
				send(event{kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP})
				return
			}
		}
	}()
	return out, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
