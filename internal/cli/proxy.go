package cli

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

// ProxyProcess is a started berth-proxy. Stdin carries the request and stays open for the run;
// closing it tells the proxy to end its session and exit.
type ProxyProcess struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Wait   func() error
	Kill   func() error
}

// ProxyStarter starts the berth-proxy beside berth with the scrubbed environment ExecProxy builds.
type ProxyStarter func(ctx context.Context) (*ProxyProcess, error)

// proxyEnvKeep is all berth-proxy may inherit; it refuses to start with anything more.
var proxyEnvKeep = []string{"HOME", "PATH", "BERTH_HOME", "TMPDIR"}

// berthOwned are removed from the caller's environment in proxy mode, so a caller's proxy or
// trust setting can neither bypass the proxy nor trust another CA.
var berthOwned = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY",
}

const maxProxyNames = 16

var (
	secretName     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	placeholderKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	credentialHex  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// proxyReadyTimeout matches berthd's: a session not Serving within 30 seconds is Lost.
	proxyReadyTimeout = 30 * time.Second
	proxyStopTimeout  = 5 * time.Second
)

const maxControlLine = 64 << 10

type proxyNames []string

func (p *proxyNames) String() string { return strings.Join(*p, ",") }

func (p *proxyNames) Set(s string) error {
	if !secretName.MatchString(s) {
		return fmt.Errorf("%q is not a secret name", s)
	}
	if slices.Contains(*p, s) {
		return fmt.Errorf("%s is set twice", s)
	}
	if len(*p) == maxProxyNames {
		return fmt.Errorf("at most %d proxies per run", maxProxyNames)
	}
	*p = append(*p, s)
	return nil
}

// ExecProxy starts the berth-proxy at path in its own process group, so a terminal Ctrl-C
// reaches only the child and the proxy outlives it until berth closes its stdin.
func ExecProxy(ctx context.Context, path string) (*ProxyProcess, error) {
	cmd := exec.CommandContext(ctx, path) //nolint:gosec // path is the berth-proxy beside berth
	cmd.Env = keepEnv(os.Environ(), proxyEnvKeep)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// An os.Pipe, not StdoutPipe: Wait would close a StdoutPipe while berth still drains it.
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
	return &ProxyProcess{
		Stdin:  stdin,
		Stdout: r,
		Wait: func() error {
			defer func() { _ = r.Close() }()
			return cmd.Wait()
		},
		Kill: cmd.Process.Kill,
	}, nil
}

func keepEnv(environ, names []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(names, k) {
			out = append(out, kv)
		}
	}
	return out
}

// runProxied never reads a value: berth-proxy fetches the values itself, and berth hands the
// child only the proxy's address, credential and CA files.
func runProxied(ctx context.Context, names []string, exe string, argv []string, env Env) int {
	if env.StartProxy == nil {
		fmt.Fprintln(env.Stderr, "berth run --proxy cannot start berth-proxy in this build")
		return ExitProxyFailed
	}
	p, err := env.StartProxy(ctx)
	if err != nil {
		fmt.Fprintf(env.Stderr, "berth-proxy did not start: %v; the command did not start\n", err)
		return ExitProxyFailed
	}
	ctl, err := handshake(p, names)
	if err != nil {
		stopProxy(p, env, true)
		fmt.Fprintf(env.Stderr, "berth-proxy did not start: %v; the command did not start\n", err)
		return ExitProxyFailed
	}
	go func() { _, _ = io.Copy(io.Discard, p.Stdout) }()
	code := startChild(ctx, exe, argv, proxyChildEnv(os.Environ(), ctl), env)
	stopProxy(p, env, false)
	return code
}

func handshake(p *ProxyProcess, names []string) (proxyctl.Control, error) {
	req, err := json.Marshal(proxyctl.Request{Secrets: names})
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
	timer := time.NewTimer(proxyReadyTimeout)
	defer timer.Stop()
	select {
	case r := <-got:
		switch {
		case errors.Is(r.err, bufio.ErrBufferFull):
			return proxyctl.Control{}, errors.New("its control line is too long")
		case r.err != nil:
			return proxyctl.Control{}, errors.New("it exited before it was ready")
		}
		return parseControl(r.line)
	case <-timer.C:
		return proxyctl.Control{}, fmt.Errorf("it was not ready within %s", proxyReadyTimeout)
	}
}

// parseControl never echoes the line, since it holds the run's credential.
func parseControl(line []byte) (proxyctl.Control, error) {
	var ctl proxyctl.Control
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ctl); err != nil {
		return proxyctl.Control{}, errors.New("its control line is malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return proxyctl.Control{}, errors.New("its control line is malformed")
	}
	switch {
	case ctl.Port < 1 || ctl.Port > 65535:
		return proxyctl.Control{}, errors.New("its control line has no valid port")
	case !credentialHex.MatchString(ctl.Credential):
		return proxyctl.Control{}, errors.New("its control line has no valid credential")
	case !filepath.IsAbs(ctl.CAFile) || !filepath.IsAbs(ctl.BundleFile):
		return proxyctl.Control{}, errors.New("its control line has no absolute CA and bundle paths")
	}
	for k, v := range ctl.Placeholders {
		if !placeholderKey.MatchString(k) || slices.Contains(berthOwned, k) || v == "" || strings.ContainsRune(v, 0) {
			return proxyctl.Control{}, fmt.Errorf("its control line has an invalid placeholder %s", k)
		}
	}
	return ctl, nil
}

// proxyChildEnv is the caller's environment minus every variable Berth owns, plus Berth's
// values. The child holds no secret, so it needs no allowlist.
func proxyChildEnv(environ []string, ctl proxyctl.Control) []string {
	owned := func(k string) bool {
		_, placeholder := ctl.Placeholders[k]
		return placeholder || slices.Contains(berthOwned, k)
	}
	var out []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); !owned(k) {
			out = append(out, kv)
		}
	}
	proxyURL := fmt.Sprintf("http://berth:%s@127.0.0.1:%d", ctl.Credential, ctl.Port)
	out = append(out,
		"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL,
		"SSL_CERT_FILE="+ctl.BundleFile, "REQUESTS_CA_BUNDLE="+ctl.BundleFile, "CURL_CA_BUNDLE="+ctl.BundleFile,
		"NODE_EXTRA_CA_CERTS="+ctl.CAFile, "NODE_USE_ENV_PROXY=1",
	)
	keys := make([]string, 0, len(ctl.Placeholders))
	for k := range ctl.Placeholders {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+ctl.Placeholders[k])
	}
	return out
}

// stopProxy closes the proxy's stdin, which ends its session, and kills it if it has not exited
// within proxyStopTimeout. A failed start kills it at once.
func stopProxy(p *ProxyProcess, env Env, now bool) {
	_ = p.Stdin.Close()
	if now {
		_ = p.Kill()
	}
	done := make(chan struct{})
	go func() {
		// The proxy's exit status changes nothing: the child's code is what berth run reports.
		_ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(proxyStopTimeout):
		fmt.Fprintf(env.Stderr, "berth-proxy did not exit within %s; killed it\n", proxyStopTimeout)
		_ = p.Kill()
		<-done
	}
}
