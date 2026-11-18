package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/logging"
	"berth/internal/proxyctl"
	"berth/internal/strictjson"
	"berth/internal/token"
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
	// ExitNoGrant: berthd holds no active proxy grant for every name; nothing was served.
	ExitNoGrant = proxyctl.ExitNoGrant
)

const (
	// Its starter kills the proxy proxyctl.StopTimeout after closing its stdin, so closing and
	// ending fit in that.
	endTimeout = 2 * time.Second
	closeWait  = 2 * time.Second
	maxRequest = 64 << 10
)

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
	r := &runner{d: d, log: newLog(d.Stderr)}

	in := bufio.NewReaderSize(io.LimitReader(d.Stdin, maxRequest), 4096)
	req, err := readRequest(in)
	if err != nil {
		return r.fail(ExitRefused, "bad request on stdin", err), nil
	}
	cl, session, grants, code := r.authorize(ctx, req.Secrets)
	defer zeroAll(grants)
	if code != ExitOK {
		return code, grants
	}
	files, code := r.prepareFiles(grants)
	defer files.remove()
	if code != ExitOK {
		return code, grants
	}
	return r.serve(ctx, cl, session, req.Mode, grants, files), grants
}

// runner is one berth-proxy run's dependencies and log.
type runner struct {
	d   Deps
	log *slog.Logger
}

// fail prints why the run ends to stderr and returns its exit code.
func (r *runner) fail(code int, msg string, err error) int {
	if err != nil {
		msg += ": " + err.Error()
	}
	fmt.Fprintln(r.d.Stderr, "berth-proxy:", msg)
	return code
}

// authorize asks the signed berthd for a session on names and reads each granted value into
// locked memory. It returns the grants made so far even when it fails, for the caller to zero.
func (r *runner) authorize(ctx context.Context, names []string) (berthv1connect.SecretsServiceClient, string, []*Grant, int) {
	cl, err := r.d.Connect(ctx)
	if err != nil {
		return nil, "", nil, r.fail(ExitError, "cannot reach the signed berthd", err)
	}
	resp, err := cl.AuthorizeProxy(ctx, &berthv1.AuthorizeProxyRequest{RequestId: rand.Text(), Names: names})
	if err != nil {
		return nil, "", nil, r.fail(ExitError, "berthd refused to authorize the proxy", err)
	}
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		return nil, "", nil, r.fail(ExitNoGrant, "no active proxy grant for every name; grant them with berth secret proxy", nil)
	}
	grants, err := buildGrants(names, resp.GetTickets(), r.d.AllowLocal)
	if err != nil {
		return nil, "", nil, r.fail(ExitRefused, "berthd sent a target berth-proxy refuses", err)
	}
	tickets := resp.GetTickets()
	for i, g := range grants {
		raw, err := r.d.Read(ctx, tickets[i].GetTicket())
		if err != nil {
			return nil, "", grants, r.fail(ExitError, "cannot read secret "+g.Name, err)
		}
		if g.value, err = lockValue(raw); err != nil {
			return nil, "", grants, r.fail(ExitRefused, "secret "+g.Name+" refused", err)
		}
		if !g.value.Locked() {
			r.log.Warn("could not lock a secret in memory; it may reach swap", "name", g.Name)
		}
	}
	return cl, resp.GetSessionId(), grants, ExitOK
}

// trustFiles is the run's CA and the folder holding ca.pem and bundle.pem.
type trustFiles struct {
	ca                      *CA
	dir, caFile, bundleFile string
}

func (f trustFiles) remove() {
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	}
}

// prepareFiles makes the run's CA for the granted hosts and writes the files a child trusts it by.
func (r *runner) prepareFiles(grants []*Grant) (trustFiles, int) {
	hosts := make([]string, 0, len(grants))
	for _, g := range grants {
		hosts = append(hosts, g.Host)
	}
	ca, err := NewCA(hosts, time.Now())
	if err != nil {
		return trustFiles{}, r.fail(ExitError, "cannot make the run CA", err)
	}
	f := trustFiles{ca: ca}
	f.dir, f.caFile, f.bundleFile, err = proxyctl.WriteTrustFiles(r.d.TempDir, r.d.SystemBundle, ca.PEM())
	if err != nil {
		return f, r.fail(ExitError, "cannot write the trust files", err)
	}
	return f, ExitOK
}

// serve listens on loopback, waits until berthd lets the session serve, prints the control line
// and proxies until the run must end, then zeroes the values and tells berthd the counts.
func (r *runner) serve(ctx context.Context, cl berthv1connect.SecretsServiceClient, session, mode string, grants []*Grant, files trustFiles) int {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return r.fail(ExitError, "cannot listen on loopback", err)
	}
	credential, err := token.Hex(16)
	if err != nil {
		_ = ln.Close()
		return r.fail(ExitError, "cannot make the proxy credential", err)
	}
	srv := NewServer(ServerConfig{
		Grants: grants, CA: files.ca, Credential: credential, Roots: r.d.Roots, Dial: r.d.Dial, Log: r.log,
		GrantedOnly: mode == proxyctl.ModeAgent,
	})
	defer srv.Close(closeWait)

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events, err := watch(watchCtx, cl, session)
	if err != nil {
		_ = ln.Close()
		return r.fail(ExitError, "berthd did not start the session", err)
	}
	select {
	case ev := <-events:
		if ev.err != nil || ev.kind != berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING {
			_ = ln.Close()
			return r.fail(ExitStopped, "berthd did not let the session serve", ev.err)
		}
	case <-time.After(proxyctl.ReadyTimeout):
		_ = ln.Close()
		return r.fail(ExitError, "berthd did not answer SERVING in time", nil)
	case <-ctx.Done():
		_ = ln.Close()
		return r.fail(ExitError, "interrupted", nil)
	}

	placeholders := map[string]string{}
	for _, g := range grants {
		if g.Placeholder != "" {
			placeholders[g.Placeholder] = Placeholder
		}
	}
	line, err := json.Marshal(proxyctl.Control{
		Port:         ln.Addr().(*net.TCPAddr).Port,
		Credential:   credential,
		CAFile:       files.caFile,
		BundleFile:   files.bundleFile,
		Placeholders: placeholders,
	})
	if err != nil {
		_ = ln.Close()
		return r.fail(ExitError, "cannot encode the control line", err)
	}
	if _, err := r.d.Stdout.Write(append(line, '\n')); err != nil {
		_ = ln.Close()
		return r.fail(ExitError, "cannot write the control line", err)
	}
	go srv.Serve(ln) //nolint:contextcheck // the server's own context, cancelled by Close, bounds every dial

	stdinClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r.d.Stdin); close(stdinClosed) }()
	expiry := time.NewTimer(time.Until(files.ca.NotAfter()))
	defer expiry.Stop()

	code, end := waitForEnd(ctx, events, stdinClosed, expiry.C, r.log)
	if !srv.Close(closeWait) {
		r.log.Warn("some connections did not close in time; zeroing anyway")
	}
	zeroAll(grants)
	files.remove()
	if end {
		injected, refused := srv.Counts()
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		_, err := cl.EndProxySession(endCtx, &berthv1.EndProxySessionRequest{
			RequestId: rand.Text(),
			SessionId: session,
			Injected:  injected,
			Refused:   refused,
		})
		cancel()
		if err != nil && code == ExitOK {
			return r.fail(ExitError, "berthd did not record the end of the session", err)
		}
	}
	return code
}

// newLog writes through the redacting handler berthd uses, so an attribute named like a secret
// never reaches stderr, which berthd copies into its own log for an agent session's proxy.
func newLog(w io.Writer) *slog.Logger {
	return slog.New(logging.NewRedacting(slog.NewTextHandler(w, nil)))
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
			log.Info("session ended by berthd", "stopped", ev.err == nil, "reason", ev.reason.String())
			return ExitStopped, ev.err == nil
		case <-expiry:
			log.Info("the run CA expired")
			return ExitStopped, true
		case <-ctx.Done():
			return ExitError, true
		}
	}
}

var errRequestLine = errors.New(`expected one line {"secrets":[...],"mode":"owner"|"agent"}`)

func readRequest(in *bufio.Reader) (proxyctl.Request, error) {
	line, err := in.ReadBytes('\n')
	if err != nil {
		return proxyctl.Request{}, errRequestLine
	}
	var req proxyctl.Request
	// Anything after the object is a second reading of the line, which the starter never wrote.
	if err := strictjson.Decode(line, &req); err != nil {
		return proxyctl.Request{}, errRequestLine
	}
	if req.Mode != proxyctl.ModeOwner && req.Mode != proxyctl.ModeAgent {
		return proxyctl.Request{}, errRequestLine
	}
	if len(req.Secrets) == 0 || len(req.Secrets) > proxyctl.MaxNames {
		return proxyctl.Request{}, fmt.Errorf("name between 1 and %d secrets", proxyctl.MaxNames)
	}
	seen := map[string]bool{}
	for _, n := range req.Secrets {
		if !proxyctl.NamePattern.MatchString(n) || seen[n] {
			return proxyctl.Request{}, fmt.Errorf("bad or repeated secret name %q", n)
		}
		seen[n] = true
	}
	return req, nil
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
	// reason is why berthd stopped the session, on STOP.
	reason berthv1.ProxyStopReason
	err    error
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
				send(event{kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP, reason: msg.GetEvent().GetStopReason()})
				return
			}
		}
	}()
	return out, nil
}
