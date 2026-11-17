package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	if proxy.request != `{"secrets":["openai-key","gh-token"]}`+"\n" {
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
