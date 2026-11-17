package cli

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/proxyctl"
	"berth/internal/proxyproc"
)

// ProxyProcess is a started berth-proxy.
type ProxyProcess = proxyproc.Process

// ProxyStarter starts the berth-proxy beside berth with the scrubbed environment ExecProxy builds.
type ProxyStarter func(ctx context.Context) (*ProxyProcess, error)

// berthOwned are removed from the caller's environment in proxy mode, so a caller's proxy or
// trust setting can neither bypass the proxy nor trust another CA.
var berthOwned = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY",
}

const maxProxyNames = 16

var (
	secretName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	// proxyReadyTimeout matches berthd's: a session not Serving within 30 seconds is Lost.
	proxyReadyTimeout = 30 * time.Second
	proxyStopTimeout  = 5 * time.Second
	// systemBundle is the trust store bundle.pem starts with, as berth-proxy's own.
	systemBundle = "/etc/ssl/cert.pem"
)

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

// ExecProxy starts the berth-proxy at path, which outlives the child until berth closes its stdin.
func ExecProxy(ctx context.Context, path string) (*ProxyProcess, error) {
	return proxyproc.Start(ctx, path, os.Stderr)
}

// runProxied never reads a value: berth-proxy fetches the values itself, and berth hands the
// child only the proxy's address, credential and CA files.
func runProxied(ctx context.Context, names []string, exe string, argv []string, env Env) int {
	if env.Agent {
		return runProxiedByBerthd(ctx, names, exe, argv, env)
	}
	if env.StartProxy == nil {
		fmt.Fprintln(env.Stderr, "berth run --proxy cannot start berth-proxy in this build")
		return ExitProxyFailed
	}
	p, err := env.StartProxy(ctx)
	if err != nil {
		fmt.Fprintf(env.Stderr, "berth-proxy did not start: %v; the command did not start\n", err)
		return ExitProxyFailed
	}
	ctl, err := proxyproc.Handshake(p, names, proxyReadyTimeout)
	if err == nil {
		err = checkControl(ctl)
	}
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

// runProxiedByBerthd is berth run --proxy for an agent session: berthd starts berth-proxy as
// its own user, so no process the agent controls holds the values, and the proxy runs while the
// StartProxy stream stays open. When berth exits, killed or not, the stream closes and berthd
// stops the proxy.
func runProxiedByBerthd(ctx context.Context, names []string, exe string, argv []string, env Env) int {
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	// berthd gives a proxy 30 seconds to serve and then answers, so this only guards a stuck berthd.
	timer := time.AfterFunc(proxyReadyTimeout+connectTimeout, stop)
	stream, err := c.Secrets.StartProxy(streamCtx, &berthv1.StartProxyRequest{RequestId: requestID(), Names: names})
	var resp *berthv1.StartProxyResponse
	if err == nil {
		resp, err = stream.Receive()
	}
	timer.Stop()
	if err != nil {
		reportError(env.Stderr, err, strings.Join(names, ", "), "")
		fmt.Fprintln(env.Stderr, "berthd did not start berth-proxy; the command did not start")
		return ExitProxyFailed
	}
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		fmt.Fprintf(env.Stderr, "no active proxy grant for every one of %s; ask the person to grant it with berth secret proxy; the command did not start\n",
			strings.Join(names, ", "))
		return ExitProxyFailed
	}
	dir, ctl, err := writeCAFiles(resp)
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }()
	}
	if err == nil {
		err = checkControl(ctl)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "berthd's berth-proxy answer is unusable: %v; the command did not start\n", err)
		return ExitProxyFailed
	}
	return startChild(ctx, exe, argv, proxyChildEnv(os.Environ(), ctl), env)
}

// writeCAFiles writes the run's CA and a bundle of the system roots plus that CA into a folder
// only this user can read, since berth-proxy's own folder belongs to berthd's user.
func writeCAFiles(resp *berthv1.StartProxyResponse) (string, proxyctl.Control, error) {
	ca := []byte(resp.GetCaPem())
	if block, _ := pem.Decode(ca); block == nil || block.Type != "CERTIFICATE" {
		return "", proxyctl.Control{}, errors.New("no CA certificate")
	}
	system, err := os.ReadFile(systemBundle)
	if err != nil {
		return "", proxyctl.Control{}, fmt.Errorf("cannot read the system trust bundle: %w", err)
	}
	dir, err := os.MkdirTemp("", "berth-proxy-")
	if err != nil {
		return "", proxyctl.Control{}, err
	}
	// The child gets absolute paths, and TMPDIR may be relative.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, proxyctl.Control{}, err
	}
	ctl := proxyctl.Control{
		Port: int(resp.GetPort()), Credential: resp.GetCredential(), Placeholders: resp.GetPlaceholders(),
		CAFile: filepath.Join(abs, "ca.pem"), BundleFile: filepath.Join(abs, "bundle.pem"),
	}
	if err := os.WriteFile(ctl.CAFile, ca, 0o600); err != nil {
		return dir, proxyctl.Control{}, err
	}
	bundle := append(append(system, '\n'), ca...)
	if err := os.WriteFile(ctl.BundleFile, bundle, 0o600); err != nil { //nolint:gosec // inside the folder MkdirTemp just made
		return dir, proxyctl.Control{}, err
	}
	return dir, ctl, nil
}

// checkControl refuses control facts the child must not get; the placeholders may not name a
// variable Berth owns, so they cannot undo the proxy settings.
func checkControl(ctl proxyctl.Control) error {
	if err := proxyproc.Check(ctl); err != nil {
		return err
	}
	for k := range ctl.Placeholders {
		if slices.Contains(berthOwned, k) {
			return fmt.Errorf("its control line has an invalid placeholder %s", k)
		}
	}
	return nil
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
	if proxyproc.Stop(p, proxyStopTimeout, now) {
		fmt.Fprintf(env.Stderr, "berth-proxy did not exit within %s; killed it\n", proxyStopTimeout)
	}
}
