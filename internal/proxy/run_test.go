package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/cli"
	"berth/internal/proxyctl"
)

const sessionID = "0123456789abcdef0123456789abcdef"

type fakeDaemon struct {
	berthv1connect.UnimplementedSecretsServiceHandler
	target  *berthv1.ProxyTarget
	outcome berthv1.UseOutcome
	// control receives "stop" or "drop" for the watch stream.
	control chan string

	mu         sync.Mutex
	authorized int
	ended      []*berthv1.EndProxySessionRequest
}

func newFakeDaemon(tgt *berthv1.ProxyTarget) *fakeDaemon {
	return &fakeDaemon{target: tgt, outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, control: make(chan string, 1)}
}

func (f *fakeDaemon) AuthorizeProxy(_ context.Context, req *berthv1.AuthorizeProxyRequest) (*berthv1.AuthorizeProxyResponse, error) {
	f.mu.Lock()
	f.authorized++
	f.mu.Unlock()
	if f.outcome != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		return &berthv1.AuthorizeProxyResponse{Outcome: f.outcome}, nil
	}
	resp := &berthv1.AuthorizeProxyResponse{Outcome: f.outcome, SessionId: sessionID}
	for _, n := range req.GetNames() {
		resp.Tickets = append(resp.Tickets, &berthv1.ProxyTicket{Name: n, Ticket: bytes.Repeat([]byte{7}, 32), Target: f.target})
	}
	return resp, nil
}

func (f *fakeDaemon) WatchProxySession(ctx context.Context, req *berthv1.WatchProxySessionRequest, stream berthv1connect.SecretsServiceWatchProxySessionServerStream) error {
	if req.GetSessionId() != sessionID {
		return connect.NewError(connect.CodePermissionDenied, "unknown session")
	}
	if err := stream.Send(&berthv1.WatchProxySessionResponse{Event: &berthv1.ProxySessionEvent{Kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING}}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case c := <-f.control:
		if c == "drop" {
			return connect.NewError(connect.CodeUnavailable, "berthd went away")
		}
		return stream.Send(&berthv1.WatchProxySessionResponse{Event: &berthv1.ProxySessionEvent{
			Kind:       berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP,
			StopReason: berthv1.ProxyStopReason_PROXY_STOP_REASON_GRANT_REVOKED,
		}})
	}
}

func (f *fakeDaemon) EndProxySession(_ context.Context, req *berthv1.EndProxySessionRequest) (*berthv1.EndProxySessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, req)
	return &berthv1.EndProxySessionResponse{}, nil
}

func (f *fakeDaemon) endings() []*berthv1.EndProxySessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*berthv1.EndProxySessionRequest{}, f.ended...)
}

func (f *fakeDaemon) client() berthv1connect.SecretsServiceClient {
	server := connect.NewServer()
	berthv1connect.RegisterSecretsServiceHandler(server, f)
	return berthv1connect.NewSecretsServiceClient(connect.NewClient(connectinprocess.New(server)))
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testDeps(t *testing.T, d *fakeDaemon, u *upstream, value string) Deps {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(bundle, []byte("# system roots\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Deps{
		Environ: []string{"HOME=/nowhere", "PATH=/usr/bin:/bin", "TMPDIR=" + os.TempDir()},
		Connect: func(context.Context) (berthv1connect.SecretsServiceClient, error) { return d.client(), nil },
		Read: func(context.Context, []byte) ([]byte, error) {
			return []byte(value), nil
		},
		Roots:        upstreamPool(u),
		Dial:         loopbackDial,
		SystemBundle: bundle,
		TempDir:      t.TempDir(),
	}
}

type running struct {
	stdin  *io.PipeWriter
	ctl    proxyctl.Control
	line   string
	stderr *syncBuffer
	done   chan result
}

type result struct {
	code   int
	grants []*Grant
}

func startRun(t *testing.T, deps Deps) *running {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &running{stdin: inW, stderr: &syncBuffer{}, done: make(chan result, 1)}
	deps.Stdin, deps.Stdout, deps.Stderr = inR, outW, r.stderr
	go func() {
		code, grants := run(t.Context(), deps)
		_ = outW.Close()
		r.done <- result{code, grants}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	if _, err := io.WriteString(inW, `{"secrets":["api-key"],"mode":"owner"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("no control line: %v; stderr %s", err, r.stderr.String())
	}
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	r.line = line
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r.ctl); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *running) wait(t *testing.T) result {
	t.Helper()
	select {
	case res := <-r.done:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("berth-proxy did not exit")
		return result{}
	}
}

func (r *running) client(t *testing.T, u *upstream) *http.Client {
	t.Helper()
	f := &fixture{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(r.ctl.Port)), trust: upstreamPool(u)}
	pem, err := os.ReadFile(r.ctl.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if !f.trust.AppendCertsFromPEM(pem) {
		t.Fatal("ca.pem holds no certificate")
	}
	return f.client(t, r.ctl.Credential)
}

func assertZeroed(t *testing.T, grants []*Grant) {
	t.Helper()
	if len(grants) == 0 {
		t.Fatal("no grants to check")
	}
	for _, g := range grants {
		if g.value == nil || !bytes.Equal(g.value.mem, make([]byte, len(g.value.mem))) {
			t.Fatalf("value of %s not zeroed", g.Name)
		}
	}
}

func TestStdinCloseEndsTheSessionWithCounts(t *testing.T) {
	u := newUpstream(t)
	d := newFakeDaemon(target(u.port(), func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "API_KEY"; p.Methods = []string{"GET"} }))
	r := startRun(t, testDeps(t, d, u, canary))
	if r.ctl.Placeholders["API_KEY"] != Placeholder || len(r.ctl.Credential) != 32 {
		t.Fatalf("control line %s", r.line)
	}
	if fi, err := os.Stat(filepath.Dir(r.ctl.CAFile)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("run folder: %v %v", fi, err)
	}
	bundle, _ := os.ReadFile(r.ctl.BundleFile)
	if !bytes.HasPrefix(bundle, []byte("# system roots\n")) || !bytes.Contains(bundle, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("bundle.pem is not the system bundle plus the CA")
	}
	c := r.client(t, u)
	resp, body, err := get(t, c, "https://"+u.authority()+"/echo")
	if err != nil || resp.StatusCode != http.StatusOK || strings.Contains(body, canary) {
		t.Fatalf("%v %q %v", resp, body, err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, "https://"+u.authority()+"/secret-path-xyz", nil)
	del, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = del.Body.Close()
	if del.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE: %d", del.StatusCode)
	}
	_ = r.stdin.Close()
	res := r.wait(t)
	if res.code != ExitOK {
		t.Fatalf("exit %d; stderr %s", res.code, r.stderr.String())
	}
	assertZeroed(t, res.grants)
	if _, err := os.Stat(filepath.Dir(r.ctl.CAFile)); !os.IsNotExist(err) {
		t.Fatal("run folder left behind")
	}
	ended := d.endings()
	if len(ended) != 1 || ended[0].GetInjected() != 1 || ended[0].GetRefused() != 1 || ended[0].GetSessionId() != sessionID {
		t.Fatalf("EndProxySession %v", ended)
	}
	// Neither stdout nor stderr ever carries the value, the credential or a request path.
	for _, leak := range []string{canary, r.ctl.Credential, "secret-path-xyz", "/echo"} {
		if strings.Contains(r.stderr.String(), leak) {
			t.Fatalf("stderr holds %q: %s", leak, r.stderr.String())
		}
	}
	if strings.Contains(r.line, canary) {
		t.Fatal("the control line holds the value")
	}
}

func TestStopOrDroppedStreamZeroesAndExits(t *testing.T) {
	for _, how := range []string{"stop", "drop"} {
		t.Run(how, func(t *testing.T) {
			u := newUpstream(t)
			d := newFakeDaemon(target(u.port(), nil))
			r := startRun(t, testDeps(t, d, u, canary))
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.ctl.Port))
			d.control <- how
			res := r.wait(t)
			if res.code != ExitStopped {
				t.Fatalf("exit %d", res.code)
			}
			assertZeroed(t, res.grants)
			if _, err := os.Stat(filepath.Dir(r.ctl.CAFile)); !os.IsNotExist(err) {
				t.Fatal("run folder left behind")
			}
			if c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr); err == nil {
				_ = c.Close()
				t.Fatal("still listening after the session ended")
			}
			// Only a STOP leaves a berthd to hear the end.
			if got, want := len(d.endings()), map[string]int{"stop": 1, "drop": 0}[how]; got != want {
				t.Fatalf("EndProxySession called %d times, want %d", got, want)
			}
		})
	}
}

func TestEnvironmentRefusedBeforeAnyContact(t *testing.T) {
	for _, extra := range []string{"GODEBUG=x509sha1=1", "SSL_CERT_FILE=/tmp/evil.pem", "HTTPS_PROXY=http://evil", "GOTRACEBACK=crash", "BERTH_NATIVE=/tmp/x"} {
		var stderr bytes.Buffer
		code := Run(t.Context(), Deps{
			Environ: []string{"HOME=/h", "PATH=/bin", extra},
			Stdin:   readerFunc(func([]byte) (int, error) { t.Fatal("stdin read"); return 0, io.EOF }),
			Stdout:  writerFunc(func([]byte) (int, error) { t.Fatal("stdout written"); return 0, nil }),
			Stderr:  &stderr,
			Connect: func(context.Context) (berthv1connect.SecretsServiceClient, error) {
				t.Fatal("berthd contacted")
				return nil, nil
			},
		})
		name, _, _ := strings.Cut(extra, "=")
		if code != ExitRefused || !strings.Contains(stderr.String(), name) {
			t.Fatalf("%s: exit %d, stderr %q", extra, code, stderr.String())
		}
	}
	if err := CheckEnv([]string{"BERTH_NATIVE=/x"}, []string{"BERTH_NATIVE"}); err != nil {
		t.Fatalf("end to end builds accept BERTH_NATIVE: %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestValueWithCRLFOrNULIsRefused(t *testing.T) {
	for _, v := range []string{"abc\r\nX-Evil: 1", "abc\ndef", "abc\x00", ""} {
		u := newUpstream(t)
		d := newFakeDaemon(target(u.port(), nil))
		deps := testDeps(t, d, u, "")
		var raw []byte
		deps.Read = func(context.Context, []byte) ([]byte, error) { raw = []byte(v); return raw, nil }
		var stdout, stderr bytes.Buffer
		deps.Stdin, deps.Stdout, deps.Stderr = strings.NewReader(`{"secrets":["api-key"],"mode":"owner"}`+"\n"), &stdout, &stderr
		if code := Run(t.Context(), deps); code != ExitRefused || stdout.Len() != 0 {
			t.Fatalf("%q: exit %d, stdout %q", v, code, stdout.String())
		}
		if !bytes.Equal(raw, make([]byte, len(raw))) {
			t.Fatalf("%q: the refused value was not zeroed", v)
		}
		if v != "" && strings.Contains(stderr.String(), "abc") {
			t.Fatalf("stderr holds the value: %q", stderr.String())
		}
	}
}

func TestNoGrantOrBadRequestServesNothing(t *testing.T) {
	u := newUpstream(t)
	d := newFakeDaemon(target(u.port(), nil))
	d.outcome = berthv1.UseOutcome_USE_OUTCOME_NO_GRANT
	deps := testDeps(t, d, u, canary)
	deps.Read = func(context.Context, []byte) ([]byte, error) { return nil, errors.New("must not read") }
	for in, want := range map[string]int{
		`{"secrets":["api-key"],"mode":"owner"}` + "\n":     ExitNoGrant,
		`{"secrets":["API"],"mode":"owner"}` + "\n":         ExitRefused,
		`{"secrets":["a","a"],"mode":"owner"}` + "\n":       ExitRefused,
		`{"secrets":["a"],"mode":"owner","extra":1}` + "\n": ExitRefused,
		`{"secrets":["api-key"],"mode":"owner"}`:            ExitRefused,
		`{"secrets":[],"mode":"owner"}` + "\n":              ExitRefused,
	} {
		var stdout bytes.Buffer
		deps.Stdin, deps.Stdout, deps.Stderr = strings.NewReader(in), &stdout, io.Discard
		if code := Run(t.Context(), deps); code != want || stdout.Len() != 0 {
			t.Fatalf("%q: exit %d, stdout %q", in, code, stdout.String())
		}
	}
}

func TestLogRedactsSecretNamedAttributes(t *testing.T) {
	var out bytes.Buffer
	newLog(&out).Warn("upstream said", "authorization", canary, "token", canary, "host", grantHost)
	if strings.Contains(out.String(), canary) || !strings.Contains(out.String(), grantHost) {
		t.Fatalf("log line %q", out.String())
	}
}

// TestRequestMustNameItsMode: a starter that does not say whose run this is gets nothing, so the
// proxy never falls back to tunnelling for an agent.
func TestRequestMustNameItsMode(t *testing.T) {
	u := newUpstream(t)
	d := newFakeDaemon(target(u.port(), nil))
	deps := testDeps(t, d, u, canary)
	for _, in := range []string{`{"secrets":["api-key"]}`, `{"secrets":["api-key"],"mode":""}`, `{"secrets":["api-key"],"mode":"root"}`} {
		var stdout bytes.Buffer
		deps.Stdin, deps.Stdout, deps.Stderr = strings.NewReader(in+"\n"), &stdout, io.Discard
		if code := Run(t.Context(), deps); code != ExitRefused || stdout.Len() != 0 {
			t.Fatalf("%s: exit %d, stdout %q", in, code, stdout.String())
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authorized != 0 {
		t.Fatalf("%d requests without a valid mode reached berthd", d.authorized)
	}
}

// TestRequestWithTrailingBytesIsRefused: the request line holds one object and nothing after it.
func TestRequestWithTrailingBytesIsRefused(t *testing.T) {
	u := newUpstream(t)
	d := newFakeDaemon(target(u.port(), nil))
	deps := testDeps(t, d, u, canary)
	for _, in := range []string{
		`{"secrets":["api-key"],"mode":"owner"} {"secrets":[],"mode":"agent"}`,
		`{"secrets":["api-key"],"mode":"owner"}x`,
		`{"secrets":["api-key"],"mode":"owner"}]`,
	} {
		var stdout bytes.Buffer
		deps.Stdin, deps.Stdout, deps.Stderr = strings.NewReader(in+"\n"), &stdout, io.Discard
		if code := Run(t.Context(), deps); code != ExitRefused || stdout.Len() != 0 {
			t.Fatalf("%s: exit %d, stdout %q", in, code, stdout.String())
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authorized != 0 {
		t.Fatalf("%d requests with trailing bytes reached berthd", d.authorized)
	}
}

// TestControlLineRoundTripsThroughBerthRun runs the real berth run --proxy logic against this
// proxy in process, so the control line must pass the CLI's own decoder, and a child that only
// gets the proxy variables reaches the granted host with the header.
func TestControlLineRoundTripsThroughBerthRun(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	u := newUpstream(t)
	d := newFakeDaemon(target(u.port(), func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "API_KEY" }))
	deps := testDeps(t, d, u, canary)
	proxyCode := make(chan int, 1)
	start := func(ctx context.Context) (*cli.ProxyProcess, error) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		deps.Stdin, deps.Stdout, deps.Stderr = inR, outW, io.Discard
		done := make(chan struct{})
		go func() {
			proxyCode <- Run(ctx, deps)
			_ = outW.Close()
			close(done)
		}()
		return &cli.ProxyProcess{
			Stdin:  inW,
			Stdout: outR,
			Wait:   func() error { <-done; return nil },
			Kill:   func() error { _ = inW.Close(); return nil },
		}, nil
	}
	var stdout, stderr syncBuffer
	code := cli.Run(t.Context(), []string{
		"run", "--proxy", "api-key", "--", "/bin/sh", "-c",
		`test "$API_KEY" = ` + Placeholder + ` && exec "$0" -sS --cacert "$SSL_CERT_FILE" https://` + u.authority() + `/echo`, curl,
	},
		cli.Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, StartProxy: start})
	if code != 0 {
		t.Fatalf("berth run exit %d: %s %s", code, stdout.String(), stderr.String())
	}
	if got := <-proxyCode; got != ExitOK {
		t.Fatalf("proxy exit %d", got)
	}
	if want := "body says Bearer " + strings.Repeat("*", len(canary)); stdout.String() != want {
		t.Fatalf("child output %q, want %q", stdout.String(), want)
	}
	h, _, _, _ := u.snapshot()
	if len(h) != 1 || h[0].Get("Authorization") != "Bearer "+canary {
		t.Fatalf("upstream headers %v", h)
	}
	if strings.Contains(stderr.String(), canary) {
		t.Fatal("stderr holds the value")
	}
}
