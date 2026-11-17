package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite" // reads proxy_sessions straight from berthd's database

	berthv1 "berth/gen/berth/v1"
	"berth/internal/client"
)

// testRoot signs the certificates of the granted test servers. berth-proxy trusts it only
// because the e2e build compiles it in; the consumer trusts it through E2E_EXTRA_CA.
var testRoot struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemFile string
}

// buildProxy builds berth-proxy with the test root compiled in and signs it ad hoc with the
// hardened runtime and its identifier, as TestMain does for the other binaries; berthd and the
// fake berth-native pin it beside them by that identifier.
func buildProxy(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "berth e2e test root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	pemFile := filepath.Join(dir, "test-root.pem")
	if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	testRoot.cert, testRoot.key, testRoot.pemFile = cert, key, pemFile
	for _, args := range [][]string{
		{
			"go", "build", "-tags", "berth_e2e", "-ldflags", "-X main.testRootDER=" + base64.StdEncoding.EncodeToString(der),
			"-o", filepath.Join(dir, "berth-proxy"), "berth/cmd/berth-proxy",
		},
		{"go", "build", "-o", filepath.Join(dir, "proxyconsumer"), "berth/test/e2e/proxyconsumer"},
		{"codesign", "--force", "-s", "-", "-o", "runtime", "-i", "com.wesleybldc.berth.proxy", filepath.Join(dir, "berth-proxy")},
	} {
		if out, err := exec.CommandContext(context.Background(), args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w\n%s", args, err, out)
		}
	}
	return nil
}

// leafCert is a certificate for localhost, signed by the test root or, with a nil parent, by
// itself.
func leafCert(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// upstream is one HTTPS test server on loopback that records the Authorization headers it got.
type upstream struct {
	port int
	addr string
	// hit is called in the handler before it answers, with the request's path.
	hit func(path string)

	mu         sync.Mutex
	headers    map[string][]string
	handshakes bytes.Buffer
}

func newUpstream(t *testing.T, cert tls.Certificate) *upstream {
	t.Helper()
	u := &upstream{headers: map[string][]string{}}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u.port = ln.Addr().(*net.TCPAddr).Port
	u.addr = "localhost:" + strconv.Itoa(u.port)
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ErrorLog:          log.New(lockedWriter{u}, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u.mu.Lock()
			u.headers[r.URL.Path] = append(u.headers[r.URL.Path], r.Header.Values("Authorization")...)
			hit := u.hit
			u.mu.Unlock()
			if hit != nil {
				hit(r.URL.Path)
			}
			_, _ = w.Write([]byte("ok"))
		}),
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return u
}

type lockedWriter struct{ u *upstream }

func (l lockedWriter) Write(p []byte) (int, error) {
	l.u.mu.Lock()
	defer l.u.mu.Unlock()
	return l.u.handshakes.Write(p)
}

func (u *upstream) got(path string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.headers[path]...)
}

func (u *upstream) all() map[string][]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string][]string{}
	for k, v := range u.headers {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (u *upstream) onHit(f func(string)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hit = f
}

func (u *upstream) handshakeErrors() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.handshakes.String()
}

func writeFake(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(h, "fake-native")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func approving(value string) map[string]any {
	return map[string]any{"answer": "approve", "value": base64.StdEncoding.EncodeToString([]byte(value))}
}

var grantLine = regexp.MustCompile(`(?m)^\s+grant\s+(\d+)$`)

// grantProxy grants name to the proxy for localhost:port and returns the grant id.
func grantProxy(t *testing.T, h string, outputs *[]string, name string, port int) int64 {
	t.Helper()
	out, code := cliRun(t, h, outputs, "secret", "proxy", name, "--host", "localhost", "--port", strconv.Itoa(port),
		"--header", "Authorization", "--prefix", "Bearer ", "--placeholder-env", "E2E_KEY")
	m := grantLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("proxy grant: %d %q", code, out)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

type sessionRow struct {
	id                string
	pid               int
	injected, refused int64
	ended             bool
}

// sessions reads proxy_sessions, oldest first.
func sessions(t *testing.T, h string) []sessionRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h, "berth.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT id, proxy_pid, injected, refused, ended_at IS NOT NULL FROM proxy_sessions ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.id, &r.pid, &r.injected, &r.refused, &r.ended); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func entityState(t *testing.T, c *client.Client, entity string) string {
	t.Helper()
	st, err := c.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: entity})
	if err != nil {
		t.Fatalf("state of %s: %v", entity, err)
	}
	return st.GetState()
}

func waitState(t *testing.T, c *client.Client, entity, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if got = entityState(t, c, entity); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s is %s, want %s", entity, got, want)
}

// lines maps the consumer's "KEY rest" lines.
func lines(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			m[k] = v
		}
	}
	return m
}

// canaryChecker collects every place the value must never be.
type canaryChecker struct {
	t     *testing.T
	forms []string
}

func newCanaryChecker(t *testing.T, canaries ...string) *canaryChecker {
	c := &canaryChecker{t: t}
	for _, v := range canaries {
		c.forms = append(c.forms, v, hex.EncodeToString([]byte(v)), base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return c
}

func (c *canaryChecker) check(where string, b []byte) {
	c.t.Helper()
	for _, f := range c.forms {
		if bytes.Contains(b, []byte(f)) {
			c.t.Errorf("canary found in %s", where)
		}
	}
}

// checkDir reads every file under dir; it returns how many it read.
func (c *canaryChecker) checkDir(where, dir string) int {
	c.t.Helper()
	n := 0
	// The proxy may delete its folder mid walk, so a file that vanished is skipped.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil //nolint:nilerr // see above
		}
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // the test's own folders
			c.check(where+" "+filepath.Base(p), b)
			n++
		}
		return nil
	})
	return n
}

func (c *canaryChecker) checkHome(h string, outputs []string) {
	c.t.Helper()
	for i, o := range outputs {
		c.check("output "+strconv.Itoa(i), []byte(o))
	}
	c.check("berthd log", []byte(readFile(c.t, filepath.Join(h, "berthd.log"))))
	dbFiles := 0
	for _, e := range mustReadDir(c.t, h) {
		if strings.HasPrefix(e, "berth.db") {
			c.check(e, []byte(readFile(c.t, filepath.Join(h, e))))
			dbFiles++
		}
	}
	if dbFiles == 0 {
		c.t.Fatal("no database files found to check")
	}
}

func tempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestProxyCanary is the E2b canary: three servers on loopback, a consumer that holds no
// secret, and the value showing up in exactly one place, the granted server's header.
func TestProxyCanary(t *testing.T) {
	h := home(t)
	canary := "canary-proxy-" + rand.Text()
	writeFake(t, h, approving(canary))
	start(t, h)
	c := waitServing(t, h)
	var outputs []string
	for _, name := range []string{"e2e-key", "e2e-key-c"} {
		if out, code := cliRun(t, h, &outputs, "secret", "request", name, "--why", "proxy canary"); code != 0 {
			t.Fatalf("request %s: %d %q", name, code, out)
		}
	}
	a := newUpstream(t, leafCert(t, testRoot.cert, testRoot.key))
	b := newUpstream(t, leafCert(t, testRoot.cert, testRoot.key))
	selfSigned := newUpstream(t, leafCert(t, nil, nil))
	grantProxy(t, h, &outputs, "e2e-key", a.port)
	grantProxy(t, h, &outputs, "e2e-key-c", selfSigned.port)
	if out, code := cliRun(t, h, &outputs, "secret", "ls"); code != 0 || !strings.Contains(out, "localhost:"+strconv.Itoa(a.port)+"  Authorization") {
		t.Fatalf("ls: %d %q", code, out)
	}

	tmp := tempDir(t, "berth-tmp-")
	check := newCanaryChecker(t, canary)
	runFiles := 0
	a.onHit(func(string) { runFiles = check.checkDir("proxy temp folder during the run", tmp) })
	env := []string{"TMPDIR=" + tmp, "E2E_EXTRA_CA=" + testRoot.pemFile, "NO_PROXY=localhost", "HTTPS_PROXY=http://127.0.0.1:1"}
	out, code := cliRunEnv(t, h, env, &outputs, "run", "--proxy", "e2e-key", "--proxy", "e2e-key-c", "--",
		filepath.Join(binDir, "proxyconsumer"), "canary", a.addr, b.addr, selfSigned.addr)
	if code != 0 {
		t.Fatalf("run: %d %q", code, out)
	}
	got := lines(out)

	if g := a.got("/a"); got["A"] != "200" || len(g) != 1 || g[0] != "Bearer "+canary {
		t.Errorf("A: status %s, %d Authorization headers, injected %v", got["A"], len(g), len(g) == 1 && g[0] == "Bearer "+canary)
	}
	if g := b.got("/b"); got["B"] != "200" || got["OTHER"] != "200" || len(g) != 1 || g[0] != "Bearer berth-proxy-placeholder" {
		t.Errorf("B: status %s, CONNECT %s, headers %q", got["B"], got["OTHER"], g)
	}
	if got["PLAIN"] != "405" || len(a.got("/plain")) != 0 {
		t.Errorf("plain HTTP: %q, A saw %q", got["PLAIN"], a.got("/plain"))
	}
	// The proxy verifies C before it answers the CONNECT, so the consumer sees the 502 there.
	if !strings.HasSuffix(got["C"], "Bad Gateway") || len(selfSigned.all()) != 0 || !strings.Contains(selfSigned.handshakeErrors(), "TLS handshake error") {
		t.Errorf("self signed C: status %q, requests %v, handshake log %q", got["C"], selfSigned.all(), selfSigned.handshakeErrors())
	}
	if got["WRONG"] != "407" || got["NOAUTH"] != "407" {
		t.Errorf("wrong credential %q, no credential %q", got["WRONG"], got["NOAUTH"])
	}
	var childEnv []string
	if err := json.Unmarshal([]byte(got["ENV"]), &childEnv); err != nil {
		t.Fatalf("environment dump: %v", err)
	}
	vars := map[string]string{}
	for _, kv := range childEnv {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	if _, ok := vars["NO_PROXY"]; ok || vars["E2E_KEY"] != "berth-proxy-placeholder" || !strings.HasPrefix(vars["HTTPS_PROXY"], "http://berth:") ||
		vars["NODE_USE_ENV_PROXY"] != "1" || filepath.Base(vars["SSL_CERT_FILE"]) != "bundle.pem" {
		t.Errorf("child environment: %v", vars)
	}
	if runFiles < 2 {
		t.Errorf("read %d files from the proxy temp folder during the run, want ca.pem and bundle.pem", runFiles)
	}
	if n := check.checkDir("proxy temp folder after the run", tmp); n != 0 {
		t.Errorf("%d files left in the proxy temp folder", n)
	}

	rows := sessions(t, h)
	if len(rows) != 1 || !rows[0].ended || rows[0].injected != 1 {
		t.Fatalf("sessions %+v", rows)
	}
	if st := entityState(t, c, "proxy/"+rows[0].id); st != "Ended" {
		t.Fatalf("session %s", st)
	}

	// The compatibility matrix: the same grant through real tools, which load the CA files and
	// the proxy from the environment themselves.
	tools := []struct {
		name string
		argv []string
	}{
		{"curl", []string{"/usr/bin/curl", "-sS", "-H", "Authorization: Bearer x", "https://" + a.addr + "/curl"}},
	}
	if node, err := exec.LookPath("node"); err == nil {
		tools = append(tools, struct {
			name string
			argv []string
		}{"node", []string{node, "-e", `fetch("https://` + a.addr + `/node", {headers: {authorization: "Bearer x"}})` +
			`.then(r => r.text()).then(t => process.stdout.write(t)).catch(e => { console.error(String(e.cause || e)); process.exit(1) })`}})
	}
	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			out, code := cliRunEnv(t, h, env, &outputs, append([]string{"run", "--proxy", "e2e-key", "--"}, tool.argv...)...)
			g := a.got("/" + tool.name)
			works := code == 0 && out == "ok" && len(g) == 1 && g[0] == "Bearer "+canary
			t.Logf("compatibility: %s works=%v exit=%d", tool.name, works, code)
			if !works {
				t.Errorf("%s through the proxy: exit %d, output %q, %d headers", tool.name, code, out, len(g))
			}
		})
	}
	check.checkHome(h, outputs)
}

// startRun starts berth run in the background and returns a function that waits for its exit
// code.
func startRun(t *testing.T, h string, env []string, outputs *[]string, args ...string) func() int {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(binDir, "berth"), args...)
	cmd.Env = append(append(os.Environ(), "BERTH_HOME="+h), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return func() int {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			*outputs = append(*outputs, out.String())
			if err == nil {
				return 0
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			t.Fatalf("berth run: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatalf("berth run did not exit; output %q", out.String())
		}
		return 0
	}
}

// firstHit returns a channel that receives once, when the server gets its first request.
func firstHit(u *upstream) <-chan struct{} {
	hit := make(chan struct{})
	var once sync.Once
	u.onHit(func(string) { once.Do(func() { close(hit) }) })
	return hit
}

func waitHit(t *testing.T, hit <-chan struct{}) {
	t.Helper()
	select {
	case <-hit:
	case <-time.After(15 * time.Second):
		t.Fatal("the granted server never got a request")
	}
}

func TestProxyLifecycle(t *testing.T) {
	h := home(t)
	canary := "canary-life-" + rand.Text()
	rotated := "canary-rotated-" + rand.Text()
	writeFake(t, h, approving(canary))
	start(t, h)
	c := waitServing(t, h)
	var outputs []string
	if out, code := cliRun(t, h, &outputs, "secret", "request", "e2e-key", "--why", "proxy lifecycle"); code != 0 {
		t.Fatalf("request: %d %q", code, out)
	}
	a := newUpstream(t, leafCert(t, testRoot.cert, testRoot.key))
	env := []string{"E2E_EXTRA_CA=" + testRoot.pemFile}
	loop := []string{"run", "--proxy", "e2e-key", "--", filepath.Join(binDir, "proxyconsumer"), "loop", a.addr}
	lastSession := func() sessionRow {
		rows := sessions(t, h)
		if len(rows) == 0 {
			t.Fatal("no proxy session")
		}
		return rows[len(rows)-1]
	}

	t.Run("revoke during a run stops injection", func(t *testing.T) {
		grant := grantProxy(t, h, &outputs, "e2e-key", a.port)
		hit := firstHit(a)
		wait := startRun(t, h, env, &outputs, loop...)
		waitHit(t, hit)
		if out, code := cliRun(t, h, &outputs, "secret", "revoke", strconv.FormatInt(grant, 10)); code != 0 || out != "revoked grant "+strconv.FormatInt(grant, 10)+"\n" {
			t.Fatalf("revoke: %d %q", code, out)
		}
		before := len(a.got("/loop"))
		if code := wait(); code != 0 {
			t.Fatalf("run: %d", code)
		}
		if after := a.got("/loop"); len(after) != before {
			t.Fatalf("%d requests reached the server after the revoke", len(after)-before)
		}
		waitState(t, c, "proxy/"+lastSession().id, "Ended")
	})

	t.Run("killing the proxy makes the session Lost", func(t *testing.T) {
		grantProxy(t, h, &outputs, "e2e-key", a.port)
		hit := firstHit(a)
		wait := startRun(t, h, env, &outputs, loop...)
		waitHit(t, hit)
		s := lastSession()
		if err := syscall.Kill(s.pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if code := wait(); code != 0 {
			t.Fatalf("run: %d", code)
		}
		waitState(t, c, "proxy/"+s.id, "Lost")
		// kqueue reports the exit at once, well before the dropped stream's grace runs out.
		st, err := c.Debug.GetState(t.Context(), &berthv1.GetStateRequest{Entity: "proxy/" + s.id, HistoryLimit: 1})
		if err != nil || st.GetHistory()[0].GetDetail() != "the proxy exited" {
			t.Fatalf("lost session history %v %v", st.GetHistory(), err)
		}
	})

	t.Run("rotate puts the new value in the next run", func(t *testing.T) {
		writeFake(t, h, approving(rotated))
		if out, code := cliRun(t, h, &outputs, "secret", "rotate", "e2e-key"); code != 0 || !strings.HasPrefix(out, "rotated e2e-key") {
			t.Fatalf("rotate: %d %q", code, out)
		}
		hit := firstHit(a)
		wait := startRun(t, h, env, &outputs, "run", "--proxy", "e2e-key", "--", "/usr/bin/curl", "-sS", "https://"+a.addr+"/rotated")
		waitHit(t, hit)
		if code := wait(); code != 0 {
			t.Fatalf("run: %d", code)
		}
		if g := a.got("/rotated"); len(g) != 1 || g[0] != "Bearer "+rotated {
			t.Fatalf("after rotate the server got %d headers, the new value %v", len(g), len(g) == 1 && g[0] == "Bearer "+rotated)
		}
	})

	t.Run("a proxy ticket redeemed by the CLI path is refused, and the reverse", func(t *testing.T) {
		exe := consumerCopy(t)
		if out, code := cliRun(t, h, &outputs, "secret", "grant", "e2e-key", "--exe", exe); code != 0 {
			t.Fatalf("grant: %d %q", code, out)
		}
		// The fake helper takes its parent to be the other kind of consumer without checking its
		// signature, so only the consumer kind berthd recorded on the ticket can refuse it.
		cfg := approving(rotated)
		cfg["trust_parent"] = "cli"
		writeFake(t, h, cfg)
		before := len(sessions(t, h))
		if out, code := cliRunEnv(t, h, env, &outputs, loop...); code != 8 {
			t.Fatalf("proxy ticket read as the CLI: %d %q", code, out)
		}
		if rows := sessions(t, h); len(rows) != before+1 {
			t.Fatalf("sessions %+v", rows)
		}
		waitState(t, c, "proxy/"+lastSession().id, "Lost")
		cfg["trust_parent"] = "proxy"
		writeFake(t, h, cfg)
		if out, code := cliRun(t, h, &outputs, "run", "--env", "K=e2e-key", "--", exe); code != 1 || !strings.Contains(out, "cannot read secret e2e-key") {
			t.Fatalf("CLI ticket read as the proxy: %d %q", code, out)
		}
		reads := 0
		for _, l := range strings.Split(strings.TrimSpace(readFile(t, filepath.Join(h, "fake-native", "requests.jsonl"))), "\n") {
			if strings.Contains(l, `"command":"read"`) && strings.Contains(l, `"redeemed":true`) {
				reads++
			}
		}
		if reads == 0 {
			t.Fatal("no read redeemed its ticket, so the kind rule was never reached")
		}
		writeFake(t, h, approving(rotated))
	})

	t.Run("rm revokes grants and refuses new runs", func(t *testing.T) {
		out, code := cliRun(t, h, &outputs, "secret", "rm", "e2e-key")
		if code != 0 || !strings.HasPrefix(out, "removed e2e-key\nrevoked grants ") {
			t.Fatalf("rm: %d %q", code, out)
		}
		before := len(sessions(t, h))
		if out, code := cliRunEnv(t, h, env, &outputs, loop...); code != 5 || !strings.Contains(out, "no active proxy grant") {
			t.Fatalf("run after rm: %d %q", code, out)
		}
		if rows := sessions(t, h); len(rows) != before {
			t.Fatalf("a run after rm made a session: %+v", rows)
		}
		if st := entityState(t, c, "secret/e2e-key"); st != "Removed" {
			t.Fatalf("secret %s", st)
		}
	})

	t.Run("berth-proxy with GODEBUG exits before contacting berthd", func(t *testing.T) {
		before := len(sessions(t, h))
		cmd := exec.CommandContext(t.Context(), filepath.Join(binDir, "berth-proxy"))
		cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "BERTH_HOME=" + h, "GODEBUG=x509sha1=1"}
		cmd.Stdin = strings.NewReader(`{"secrets":["e2e-key"]}` + "\n")
		out, err := cmd.CombinedOutput()
		outputs = append(outputs, string(out))
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "GODEBUG") {
			t.Fatalf("berth-proxy with GODEBUG: %v %q", err, out)
		}
		if rows := sessions(t, h); len(rows) != before {
			t.Fatalf("berth-proxy reached berthd: %+v", rows)
		}
	})

	newCanaryChecker(t, canary, rotated).checkHome(h, outputs)
}
