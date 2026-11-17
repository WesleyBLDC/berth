package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"connectrpc.com/connect/v2/connectproto"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
)

// fakeCredential is low entropy on purpose, so secret scanners do not flag it.
const fakeCredential = "cccccccccccccccccccccccccccccccc"

const goodControl = `{"port":4123,"credential":"` + fakeCredential + `","ca_file":"/run/ca.pem",` +
	`"bundle_file":"/run/bundle.pem","placeholders":{"OPENAI_API_KEY":"berth-placeholder-openai"}}` + "\n"

// fakeProxy answers with control and then holds until berth closes its stdin. stopped records
// whether marker existed at that moment, which proves stdin closed only after the child ran.
type fakeProxy struct {
	control  string
	marker   string
	hang     bool
	request  string
	stopped  chan bool
	killed   chan struct{}
	killOnce sync.Once
}

func newFakeProxy(control, marker string) *fakeProxy {
	return &fakeProxy{control: control, marker: marker, stopped: make(chan bool, 1), killed: make(chan struct{})}
}

func (f *fakeProxy) start(context.Context) (*ProxyProcess, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer func() { _ = outW.Close() }()
		in := bufio.NewReader(inR)
		line, _ := in.ReadString('\n')
		f.request = line
		if f.control == "" {
			return
		}
		_, _ = io.WriteString(outW, f.control)
		_, _ = io.Copy(io.Discard, in)
		_, err := os.Stat(f.marker)
		f.stopped <- err == nil
		if f.hang {
			<-f.killed
		}
	}()
	return &ProxyProcess{
		Stdin:  inW,
		Stdout: outR,
		Wait:   func() error { <-exited; return nil },
		Kill: func() error {
			f.killOnce.Do(func() { close(f.killed) })
			_ = inR.CloseWithError(errors.New("killed"))
			return nil
		},
	}, nil
}

func TestRunProxyGivesTheChildBerthsValuesAndPassesTheExitCode(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://evil.example:8080")
	t.Setenv("SSL_CERT_FILE", "/tmp/evil.pem")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("http_proxy", "http://evil.example:8080")
	t.Setenv("OPENAI_API_KEY", "caller-value")
	t.Setenv("CALLER_KEEP", "yes")
	marker := filepath.Join(t.TempDir(), "child-ran")
	proxy := newFakeProxy(goodControl, marker)
	fake := &fakeSecrets{}
	r := invokeWith(t, fake, nil, proxy.start, "run", "--proxy", "openai-key", "--proxy", "gh-token", "--",
		"/bin/sh", "-c", `/usr/bin/env; touch "$1"; exit 9`, "sh", marker)
	if r.code != 9 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if fake.authorize != 0 {
		t.Fatal("proxy mode called AuthorizeUse")
	}
	if proxy.request != `{"secrets":["openai-key","gh-token"],"mode":"owner"}`+"\n" {
		t.Fatalf("request %q", proxy.request)
	}
	if !<-proxy.stopped {
		t.Fatal("the proxy's stdin closed before the child exited")
	}
	env := map[string]string{}
	for line := range strings.SplitSeq(r.stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	want := map[string]string{
		"HTTPS_PROXY":         "http://berth:" + fakeCredential + "@127.0.0.1:4123",
		"https_proxy":         "http://berth:" + fakeCredential + "@127.0.0.1:4123",
		"SSL_CERT_FILE":       "/run/bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/run/bundle.pem",
		"CURL_CA_BUNDLE":      "/run/bundle.pem",
		"NODE_EXTRA_CA_CERTS": "/run/ca.pem",
		"NODE_USE_ENV_PROXY":  "1",
		"OPENAI_API_KEY":      "berth-placeholder-openai",
		"CALLER_KEEP":         "yes",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for _, gone := range []string{"NO_PROXY", "http_proxy"} {
		if _, ok := env[gone]; ok {
			t.Errorf("%s reached the child", gone)
		}
	}
	if n := strings.Count(r.stdout, "HTTPS_PROXY="); n != 1 {
		t.Errorf("HTTPS_PROXY set %d times", n)
	}
}

func TestRunProxyWithAMalformedControlLineNeverStartsTheChild(t *testing.T) {
	for name, control := range map[string]string{
		"not json":       "port=1\n",
		"bad credential": `{"port":1,"credential":"short","ca_file":"/a","bundle_file":"/b"}` + "\n",
		"relative paths": `{"port":1,"credential":"` + fakeCredential + `","ca_file":"a","bundle_file":"b"}` + "\n",
		"owned name":     strings.Replace(goodControl, "OPENAI_API_KEY", "SSL_CERT_FILE", 1),
		"unknown field":  strings.Replace(goodControl, `"port"`, `"extra":1,"port"`, 1),
		"exited early":   "",
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child-ran")
			proxy := newFakeProxy(control, marker)
			r := invokeWith(t, &fakeSecrets{}, nil, proxy.start, "run", "--proxy", "k", "--", "/usr/bin/touch", marker)
			if r.code != ExitProxyFailed || !strings.Contains(r.stderr, "the command did not start") {
				t.Fatalf("code %d, stderr %q", r.code, r.stderr)
			}
			if strings.Contains(r.stderr, fakeCredential) {
				t.Fatalf("the credential reached stderr: %q", r.stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the child ran: %v", err)
			}
		})
	}
}

func TestRunProxyKillsAProxyThatDoesNotExit(t *testing.T) {
	old := proxyStopTimeout
	proxyStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { proxyStopTimeout = old })
	proxy := newFakeProxy(goodControl, "/nonexistent")
	proxy.hang = true
	r := invokeWith(t, &fakeSecrets{}, nil, proxy.start, "run", "--proxy", "k", "--", "/usr/bin/true")
	if r.code != ExitOK || !strings.Contains(r.stderr, "killed it") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

func TestRunProxyAndEnvTogetherIsUsage(t *testing.T) {
	proxy := newFakeProxy(goodControl, "/nonexistent")
	started := false
	start := func(ctx context.Context) (*ProxyProcess, error) {
		started = true
		return proxy.start(ctx)
	}
	r := invokeWith(t, &fakeSecrets{}, nil, start, "run", "--proxy", "k", "--env", "K=k", "--", "/usr/bin/true")
	if r.code != ExitUsage || started {
		t.Fatalf("code %d, started %v", r.code, started)
	}
}

// TestExecProxyScrubsTheEnvironment runs a shell script as berth-proxy through the real starter:
// it must see only HOME, PATH, BERTH_HOME and TMPDIR.
func TestExecProxyScrubsTheEnvironment(t *testing.T) {
	t.Setenv("GODEBUG", "x509ignoreCN=0")
	t.Setenv("SSL_CERT_FILE", "/tmp/evil.pem")
	t.Setenv("BERTH_HOME", "/tmp/berth-home")
	dir := t.TempDir()
	seen := filepath.Join(dir, "env")
	script := filepath.Join(dir, "berth-proxy")
	body := "#!/bin/sh\n/usr/bin/env > '" + seen + "'\nread -r line\nprintf '%s' '" + goodControl + "'\ncat > /dev/null\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	r := invokeWith(t, &fakeSecrets{}, nil, func(ctx context.Context) (*ProxyProcess, error) { return ExecProxy(ctx, script) },
		"run", "--proxy", "k", "--", "/usr/bin/true")
	if r.code != ExitOK {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
	b, err := os.ReadFile(seen) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		k, _, _ := strings.Cut(line, "=")
		switch k {
		case "HOME", "PATH", "BERTH_HOME", "TMPDIR":
		case "PWD", "SHLVL", "_", "OLDPWD":
			// /bin/sh sets these itself.
		default:
			t.Errorf("berth-proxy inherited %s", k)
		}
	}
	if !strings.Contains(string(b), "BERTH_HOME=/tmp/berth-home") {
		t.Fatalf("BERTH_HOME missing:\n%s", b)
	}
}

const agentCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

// agentProxy is berthd's side of StartProxy for the agent mode tests: it answers resp, or err,
// and records whether the stream was still open when the child had run.
type agentProxy struct {
	berthv1connect.UnimplementedSecretsServiceHandler
	resp   *berthv1.StartProxyResponse
	err    error
	names  []string
	marker string
	closed chan bool
}

func (a *agentProxy) StartProxy(ctx context.Context, req *berthv1.StartProxyRequest, stream berthv1connect.SecretsServiceStartProxyServerStream) error {
	a.names = req.GetNames()
	if a.err != nil {
		return a.err
	}
	if err := stream.Send(a.resp); err != nil {
		return err
	}
	<-ctx.Done()
	_, err := os.Stat(a.marker)
	a.closed <- err == nil
	return nil
}

func invokeAgent(t *testing.T, a *agentProxy, args ...string) result {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterSecretsServiceHandler(server, a)
	cl := &client.Client{Secrets: berthv1connect.NewSecretsServiceClient(connect.NewClient(connectinprocess.New(server)))}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Agent: true,
		Connect: func(context.Context) (*client.Client, error) { return cl, nil },
		StartProxy: func(context.Context) (*ProxyProcess, error) {
			t.Error("agent mode started berth-proxy itself")
			return nil, errors.New("no")
		},
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func authorizedStart() *berthv1.StartProxyResponse {
	return &berthv1.StartProxyResponse{
		Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, SessionId: strings.Repeat("a", 32), Port: 4123,
		Credential: fakeCredential, CaPem: agentCA, Placeholders: map[string]string{"OPENAI_API_KEY": "berth-placeholder-openai"},
	}
}

// TestAgentRunProxyUsesBerthdsProxy: in agent mode berth never starts berth-proxy; it writes the
// CA berthd sent into a folder of its own for the child, and closes the stream only after the
// child ran, which is what stops berthd's proxy.
func TestAgentRunProxyUsesBerthdsProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://evil.example:8080")
	t.Setenv("NO_PROXY", "*")
	marker := filepath.Join(t.TempDir(), "child-ran")
	a := &agentProxy{resp: authorizedStart(), marker: marker, closed: make(chan bool, 1)}
	r := invokeAgent(t, a, "run", "--proxy", "openai-key", "--", "/bin/sh", "-c",
		`/usr/bin/env; echo CA; cat "$NODE_EXTRA_CA_CERTS"; echo BUNDLE; cat "$SSL_CERT_FILE"; touch "$1"; exit 7`, "sh", marker)
	if r.code != 7 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if !slices.Equal(a.names, []string{"openai-key"}) {
		t.Fatalf("asked berthd for %v", a.names)
	}
	if !<-a.closed {
		t.Fatal("the stream closed before the child ran")
	}
	env := map[string]string{}
	for line := range strings.SplitSeq(r.stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	if env["HTTPS_PROXY"] != "http://berth:"+fakeCredential+"@127.0.0.1:4123" || env["OPENAI_API_KEY"] != "berth-placeholder-openai" {
		t.Fatalf("child environment %v", env)
	}
	if _, ok := env["NO_PROXY"]; ok {
		t.Fatal("NO_PROXY reached the child")
	}
	ca, bundle, _ := strings.Cut(strings.SplitN(r.stdout, "\nCA\n", 2)[1], "BUNDLE\n")
	if ca != agentCA || !strings.HasSuffix(bundle, agentCA) || len(bundle) <= len(agentCA)+1 {
		t.Fatalf("CA file %q, bundle of %d bytes", ca, len(bundle))
	}
	if _, err := os.Stat(filepath.Dir(env["SSL_CERT_FILE"])); !os.IsNotExist(err) {
		t.Fatalf("the CA folder outlived the run: %v", err)
	}
}

func TestAgentRunProxyRefusalsNeverStartTheChild(t *testing.T) {
	owned := authorizedStart()
	owned.Placeholders = map[string]string{"SSL_CERT_FILE": "x"}
	noCA := authorizedStart()
	noCA.CaPem = "not a certificate"
	for name, tc := range map[string]struct {
		a    *agentProxy
		want string
	}{
		"no grant":       {&agentProxy{resp: &berthv1.StartProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_NO_GRANT}}, "no active proxy grant"},
		"rate limited":   {&agentProxy{err: withReasonErr(t, connect.CodeResourceExhausted, berthv1.ErrorReason_ERROR_REASON_RATE_LIMITED)}, "berthd did not start berth-proxy"},
		"owned name":     {&agentProxy{resp: owned}, "invalid placeholder SSL_CERT_FILE"},
		"no CA":          {&agentProxy{resp: noCA}, "no CA certificate"},
		"bad credential": {&agentProxy{resp: &berthv1.StartProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, Port: 1, CaPem: agentCA}}, "no valid credential"},
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child-ran")
			tc.a.marker, tc.a.closed = marker, make(chan bool, 1)
			r := invokeAgent(t, tc.a, "run", "--proxy", "k", "--", "/usr/bin/touch", marker)
			if r.code != ExitProxyFailed || !strings.Contains(r.stderr, tc.want) || !strings.Contains(r.stderr, "the command did not start") {
				t.Fatalf("code %d, stderr %q", r.code, r.stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the child ran: %v", err)
			}
		})
	}
}

func withReasonErr(t *testing.T, code connect.Code, reason berthv1.ErrorReason) error {
	t.Helper()
	detail, err := connectproto.NewErrorDetail(&berthv1.ErrorInfo{Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return connect.NewError(code, "refused").WithDetail(detail)
}
