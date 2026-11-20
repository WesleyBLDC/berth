package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/testutil"
)

const (
	canary    = "c4n4ry-s3cr3t-v4lue"
	testCred  = "0123456789abcdef0123456789abcdef"
	grantHost = "example.com"
)

// upstream is an httptest TLS server (its certificate covers example.com) that records what
// reached it.
type upstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// reqs holds each request's headers, body, length and transfer encoding.
	headers []http.Header
	bodies  []string
	lengths []int64
	te      [][]string
	conns   int
	active  int
}

func newUpstream(t *testing.T) *upstream { t.Helper(); return newUpstreamWith(t, nil) }

func newUpstreamWith(t *testing.T, configure func(*http.Server)) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.headers = append(u.headers, r.Header.Clone())
		u.bodies = append(u.bodies, string(body))
		u.lengths = append(u.lengths, r.ContentLength)
		u.te = append(u.te, r.TransferEncoding)
		u.mu.Unlock()
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/echo":
			w.Header().Set("X-Echo", auth)
			_, _ = io.WriteString(w, "body says "+auth)
		case "/gzip-echo":
			// An API that compresses when asked, and echoes the request's headers.
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("X-Echo", auth)
				_, _ = io.WriteString(w, "body says "+auth)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			gzipped(w, "body says "+auth)
		case "/gzip-204":
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusNoContent)
		case "/gzip-304":
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusNotModified)
		case "/bad-name":
			// An encoded body under a header name with a space before its colon.
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			var body strings.Builder
			gzipped(&body, "body says "+auth)
			fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Encoding : gzip\r\nContent-Length: %d\r\n\r\n%s", body.Len(), body.String())
			_ = rw.Flush()
		case "/always-gzip":
			// An API that ignores Accept-Encoding: identity.
			w.Header().Set("Content-Encoding", "gzip")
			gzipped(w, "body says "+auth)
		case "/stream":
			_, _ = io.WriteString(w, "first part ")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "then "+auth)
		default:
			_, _ = io.WriteString(w, "ok "+string(body))
		}
	}))
	u.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	u.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		u.mu.Lock()
		defer u.mu.Unlock()
		switch s {
		case http.StateNew:
			u.conns++
		case http.StateActive:
			u.active++
		case http.StateIdle, http.StateHijacked, http.StateClosed:
		}
	}
	if configure != nil {
		configure(u.srv.Config)
	}
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func gzipped(w io.Writer, s string) {
	zw := gzip.NewWriter(w)
	_, _ = io.WriteString(zw, s)
	_ = zw.Close()
}

func (u *upstream) port() int { return u.srv.Listener.Addr().(*net.TCPAddr).Port }

func (u *upstream) authority() string { return net.JoinHostPort(grantHost, strconv.Itoa(u.port())) }

func (u *upstream) snapshot() (headers []http.Header, bodies []string, conns, active int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header{}, u.headers...), append([]string{}, u.bodies...), u.conns, u.active
}

func target(port int, mod func(*berthv1.ProxyTarget)) *berthv1.ProxyTarget {
	t := &berthv1.ProxyTarget{Host: grantHost, Port: uint32(port), HeaderName: "Authorization", ValuePrefix: "Bearer "}
	if mod != nil {
		mod(t)
	}
	return t
}

func testGrant(t *testing.T, tgt *berthv1.ProxyTarget) *Grant {
	t.Helper()
	g, err := newGrant("api-key", tgt, false)
	if err != nil {
		t.Fatal(err)
	}
	if g.value, err = lockValue([]byte(canary)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.value.Zero)
	return g
}

// loopbackDial sends every name to 127.0.0.1, so example.com reaches the test servers.
func loopbackDial(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, "127.0.0.1:"+port)
}

type fixture struct {
	addr string
	srv  *Server
	ca   *CA
	// trust holds the proxy CA and the upstream test certificate, as bundle.pem would.
	trust *x509.CertPool
}

func upstreamPool(u *upstream) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(u.srv.Certificate())
	return p
}

func startProxy(t *testing.T, roots *x509.CertPool, trusted *upstream, grants ...*Grant) *fixture {
	t.Helper()
	return startProxyWith(t, ServerConfig{}, roots, trusted, grants...)
}

// startProxyWith starts a proxy with cfg's switches; the grants, CA, credential, roots, dialer and
// log are the fixture's.
func startProxyWith(t *testing.T, cfg ServerConfig, roots *x509.CertPool, trusted *upstream, grants ...*Grant) *fixture {
	t.Helper()
	hosts := make([]string, 0, len(grants))
	for _, g := range grants {
		hosts = append(hosts, g.Host)
	}
	ca, err := NewCA(hosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Grants, cfg.CA, cfg.Credential, cfg.Roots, cfg.Dial, cfg.Log = grants, ca, testCred, roots, loopbackDial, slog.New(slog.DiscardHandler)
	srv := NewServer(cfg)
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(closeWait) })
	trust := upstreamPool(trusted)
	trust.AddCert(ca.Certificate())
	return &fixture{addr: ln.Addr().String(), srv: srv, ca: ca, trust: trust}
}

func (f *fixture) client(t *testing.T, cred string) *http.Client {
	t.Helper()
	tr := &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", User: url.UserPassword("berth", cred), Host: f.addr}),
		TLSClientConfig: &tls.Config{RootCAs: f.trust, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// reply is a response whose body get already read and closed.
type reply struct {
	StatusCode       int
	Header           http.Header
	TransferEncoding []string
}

func replyOf(resp *http.Response) *reply {
	_ = resp.Body.Close()
	return &reply{StatusCode: resp.StatusCode, Header: resp.Header, TransferEncoding: resp.TransferEncoding}
}

func get(t *testing.T, c *http.Client, rawURL string) (*reply, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return &reply{StatusCode: resp.StatusCode}, string(body), err
}

func basic(cred string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("berth:"+cred))
}

// raw sends one request head to the proxy itself and returns the response.
func (f *fixture) raw(t *testing.T, head string) *reply {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return replyOf(resp)
}

// tunnel opens an authenticated CONNECT to authority and a TLS session through it that trusts
// the proxy CA, for requests written by hand.
type tunnel struct {
	conn *tls.Conn
	r    *bufio.Reader
}

func (f *fixture) open(t *testing.T, authority string) *tunnel {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", authority, authority, basic(testCred))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %d", resp.StatusCode)
	}
	tc := tls.Client(c, &tls.Config{ServerName: grantHost, RootCAs: f.trust, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := tc.ConnectionState().NegotiatedProtocol; p != "" && p != "http/1.1" {
		t.Fatalf("ALPN %q", p)
	}
	return &tunnel{conn: tc, r: bufio.NewReader(tc)}
}

func (tn *tunnel) do(t *testing.T, method, request string) (*reply, string) {
	t.Helper()
	if _, err := io.WriteString(tn.conn, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(tn.r, &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return replyOf(resp), string(body)
}

func TestHeaderReachesOnlyTheGrantedHostAndPort(t *testing.T) {
	a, b := newUpstream(t), newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	c := f.client(t, testCred)
	for _, u := range []*upstream{a, b} {
		resp, body, err := get(t, c, "https://"+u.authority()+"/")
		if err != nil || resp.StatusCode != http.StatusOK || body != "ok " {
			t.Fatalf("%s: %v %q %v", u.authority(), resp, body, err)
		}
	}
	ha, _, _, _ := a.snapshot()
	hb, _, _, _ := b.snapshot()
	if got := ha[0].Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+canary {
		t.Fatalf("granted host got %q", got)
	}
	if got := hb[0].Values("Authorization"); len(got) != 0 {
		t.Fatalf("other port got %q", got)
	}
	if injected, _ := f.srv.Counts(); injected != 1 {
		t.Fatalf("injected %d, want 1", injected)
	}
}

// TestGrantedOnlyRefusesEveryOtherTarget: the proxy berthd runs for an agent session dials only
// its granted host and port, never loopback or any other target the agent's child names.
func TestGrantedOnlyRefusesEveryOtherTarget(t *testing.T) {
	a, b := newUpstream(t), newUpstream(t)
	f := startProxyWith(t, ServerConfig{GrantedOnly: true}, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	resp, body, err := get(t, f.client(t, testCred), "https://"+a.authority()+"/")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok " {
		t.Fatalf("granted host: %v %q %v", resp, body, err)
	}
	for _, authority := range []string{b.authority(), net.JoinHostPort("127.0.0.1", strconv.Itoa(b.port())), grantHost + ":443"} {
		head := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", authority, authority, basic(testCred))
		if r := f.raw(t, head); r.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s: %d, want 403", authority, r.StatusCode)
		}
	}
	if _, _, conns, _ := b.snapshot(); conns != 0 {
		t.Fatalf("a target without a grant was dialled %d times", conns)
	}
}

func TestClientCopiesAndHopByHopHeadersAreRemoved(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	tn := f.open(t, a.authority())
	resp, _ := tn.do(t, http.MethodGet, "GET / HTTP/1.1\r\nHost: "+a.authority()+"\r\n"+
		"Authorization: evil1\r\nauthorization: evil2\r\nAUTHORIZATION: evil3\r\n"+
		"Connection: X-Drop\r\nX-Drop: 1\r\nKeep-Alive: 5\r\nProxy-Authorization: x\r\nX-Keep: yes\r\n\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h, _, _, _ := a.snapshot()
	if got := h[0].Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+canary {
		t.Fatalf("Authorization %q", got)
	}
	for _, k := range []string{"X-Drop", "Keep-Alive", "Proxy-Authorization"} {
		if h[0].Get(k) != "" {
			t.Fatalf("%s forwarded", k)
		}
	}
	if h[0].Get("X-Keep") != "yes" {
		t.Fatal("an ordinary header was dropped")
	}
}

func TestUntrustedUpstreamGets502AndNoBytes(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, x509.NewCertPool(), a, testGrant(t, target(a.port(), nil)))
	_, _, err := get(t, f.client(t, testCred), "https://"+a.authority()+"/")
	if err == nil || !strings.Contains(err.Error(), "Bad Gateway") {
		t.Fatalf("want a 502 from the proxy, got %v", err)
	}
	if h, _, _, active := a.snapshot(); len(h) != 0 || active != 0 {
		t.Fatalf("upstream saw %d requests, %d active connections", len(h), active)
	}
	if injected, _ := f.srv.Counts(); injected != 0 {
		t.Fatalf("injected %d", injected)
	}
}

// TestSystemRootsRefuseASelfSignedUpstream: a release berth-proxy has no Roots and verifies
// upstreams against the system's trust store, which no test certificate is in.
func TestSystemRootsRefuseASelfSignedUpstream(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, nil, a, testGrant(t, target(a.port(), nil)))
	_, _, err := get(t, f.client(t, testCred), "https://"+a.authority()+"/")
	if err == nil || !strings.Contains(err.Error(), "Bad Gateway") {
		t.Fatalf("want a 502 from the proxy, got %v", err)
	}
	if h, _, _, active := a.snapshot(); len(h) != 0 || active != 0 {
		t.Fatalf("upstream saw %d requests, %d active connections", len(h), active)
	}
}

func TestPlainHTTPGets405(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	resp := f.raw(t, "GET http://"+a.authority()+"/ HTTP/1.1\r\nHost: "+a.authority()+"\r\nProxy-Authorization: "+basic(testCred)+"\r\n\r\n")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if h, _, _, _ := a.snapshot(); len(h) != 0 {
		t.Fatal("plain HTTP reached the upstream")
	}
}

func TestMissingOrWrongCredentialGets407(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	for name, auth := range map[string]string{
		"missing":  "",
		"wrong":    "Proxy-Authorization: " + basic("ffffffffffffffffffffffffffffffff") + "\r\n",
		"user":     "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("root:"+testCred)) + "\r\n",
		"bearer":   "Proxy-Authorization: Bearer " + testCred + "\r\n",
		"garbage":  "Proxy-Authorization: Basic !!!\r\n",
		"prefixed": "Proxy-Authorization: " + basic(testCred+"0") + "\r\n",
	} {
		resp := f.raw(t, "CONNECT "+a.authority()+" HTTP/1.1\r\nHost: "+a.authority()+"\r\n"+auth+"\r\n")
		if resp.StatusCode != http.StatusProxyAuthRequired || resp.Header.Get("Proxy-Authenticate") == "" {
			t.Fatalf("%s: status %d, Proxy-Authenticate %q", name, resp.StatusCode, resp.Header.Get("Proxy-Authenticate"))
		}
	}
	if _, _, err := get(t, f.client(t, "ffffffffffffffffffffffffffffffff"), "https://"+a.authority()+"/"); err == nil {
		t.Fatal("a wrong credential got through")
	}
	if h, _, conns, _ := a.snapshot(); len(h) != 0 || conns != 0 {
		t.Fatal("an unauthenticated client reached the upstream")
	}
}

func TestPathMethodAndHostLimits(t *testing.T) {
	a := newUpstream(t)
	g := testGrant(t, target(a.port(), func(p *berthv1.ProxyTarget) {
		p.Methods = []string{"GET"}
		p.PathGlob = "/v1/*"
	}))
	f := startProxy(t, upstreamPool(a), a, g)
	host := a.authority()
	cases := []struct {
		method, uri, host string
		want              int
		header            string
	}{
		{"GET", "/v1/models", host, http.StatusOK, ""},
		{"GET", "/v1/models", host, http.StatusForbidden, "X-HTTP-Method-Override: DELETE\r\n"},
		{"GET", "/v1/models", host, http.StatusForbidden, "x-http-method: DELETE\r\n"},
		{"GET", "/v1/models", host, http.StatusForbidden, "X-Method-Override: GET\r\n"},
		{"GET", "/v1/models", host, http.StatusForbidden, "X_HTTP_Method_Override: DELETE\r\n"},
		{"GET", "/v1/models", host, http.StatusForbidden, "x-http_method: DELETE\r\n"},
		{"GET", "/v1/models", host, http.StatusForbidden, "Any_Name: 1\r\n"},
		{"GET", "/v1/models?q=../..", host, http.StatusOK, ""},
		{"POST", "/v1/models", host, http.StatusForbidden, ""},
		{"GET", "/v2/models", host, http.StatusForbidden, ""},
		{"GET", "/v1/a/b", host, http.StatusForbidden, ""},
		{"GET", "/v1/../admin", host, http.StatusForbidden, ""},
		{"GET", "/v1/%2e%2e", host, http.StatusForbidden, ""},
		{"GET", "/v1//x", host, http.StatusForbidden, ""},
		{"GET", "/v1/..;", host, http.StatusForbidden, ""},
		{"GET", "/v1/models;admin", host, http.StatusForbidden, ""},
		{"GET", "/v1/models%3Badmin", host, http.StatusForbidden, ""},
		{"GET", "/v1/models", "evil.example", http.StatusMisdirectedRequest, ""},
		{"GET", "/v1/models", "example.com", http.StatusMisdirectedRequest, ""},
	}
	refused := 0
	for _, c := range cases {
		tn := f.open(t, host)
		resp, _ := tn.do(t, c.method, c.method+" "+c.uri+" HTTP/1.1\r\nHost: "+c.host+"\r\n"+c.header+"Content-Length: 0\r\n\r\n")
		if resp.StatusCode != c.want {
			t.Fatalf("%s %s Host %s %q: status %d, want %d", c.method, c.uri, c.host, c.header, resp.StatusCode, c.want)
		}
		if c.want != http.StatusOK {
			refused++
		}
	}
	injected, gotRefused := f.srv.Counts()
	if injected != 2 || gotRefused != uint64(refused) {
		t.Fatalf("injected %d refused %d, want 2 and %d", injected, gotRefused, refused)
	}
	if h, _, _, _ := a.snapshot(); len(h) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(h))
	}
}

func TestBodiesKeepAliveAndRedaction(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	host := a.authority()
	tn := f.open(t, host)

	resp, body := tn.do(t, "POST", "POST /len HTTP/1.1\r\nHost: "+host+"\r\nContent-Length: 5\r\n\r\nhello")
	if resp.StatusCode != http.StatusOK || body != "ok hello" {
		t.Fatalf("content length: %d %q", resp.StatusCode, body)
	}
	resp, body = tn.do(t, "POST", "POST /chunked HTTP/1.1\r\nHost: "+host+"\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nwor\r\n2\r\nld\r\n0\r\nAuthorization: trailer\r\n\r\n")
	if resp.StatusCode != http.StatusOK || body != "ok world" {
		t.Fatalf("chunked: %d %q", resp.StatusCode, body)
	}
	resp, body = tn.do(t, "GET", "GET /echo HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	if strings.Contains(body, canary) || strings.Contains(resp.Header.Get("X-Echo"), canary) {
		t.Fatalf("the value came back: %q %q", resp.Header.Get("X-Echo"), body)
	}
	stars := strings.Repeat("*", len(canary))
	if body != "body says Bearer "+stars || resp.Header.Get("X-Echo") != "Bearer "+stars {
		t.Fatalf("redaction: %q %q", resp.Header.Get("X-Echo"), body)
	}
	resp, body = tn.do(t, "GET", "GET /stream HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	if len(resp.TransferEncoding) == 0 || body != "first part then Bearer "+stars {
		t.Fatalf("chunked response: %v %q", resp.TransferEncoding, body)
	}
	_, _ = tn.conn.Write([]byte("POST /expect HTTP/1.1\r\nHost: " + host + "\r\nContent-Length: 2\r\nExpect: 100-continue\r\n\r\n"))
	cont, err := http.ReadResponse(tn.r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replyOf(cont).StatusCode != http.StatusContinue {
		t.Fatalf("expect: %d", cont.StatusCode)
	}
	resp, body = tn.do(t, "POST", "hi")
	if resp.StatusCode != http.StatusOK || body != "ok hi" {
		t.Fatalf("after 100 Continue: %d %q", resp.StatusCode, body)
	}

	h, bodies, conns, _ := a.snapshot()
	if conns != 1 {
		t.Fatalf("upstream connections %d, want 1 kept alive", conns)
	}
	if a.lengths[0] != 5 || len(a.te[1]) != 1 || a.te[1][0] != "chunked" || bodies[1] != "world" {
		t.Fatalf("framing: lengths %v te %v bodies %q", a.lengths, a.te, bodies)
	}
	if got := h[1].Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+canary {
		t.Fatalf("chunked request Authorization %q", got)
	}
	if h[4].Get("Expect") != "" {
		t.Fatal("Expect reached the upstream")
	}
}

func TestEncodedBodiesNeverCarryTheValue(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	host := a.authority()
	stars := strings.Repeat("*", len(canary))

	tn := f.open(t, host)
	resp, body := tn.do(t, "GET", "GET /gzip-echo HTTP/1.1\r\nHost: "+host+"\r\nAccept-Encoding: gzip, br\r\n\r\n")
	if resp.StatusCode != http.StatusOK || body != "body says Bearer "+stars || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("asked for gzip: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	h, _, _, _ := a.snapshot()
	if got := h[0].Values("Accept-Encoding"); len(got) != 1 || got[0] != "identity" {
		t.Fatalf("upstream Accept-Encoding %q, want identity", got)
	}

	tn = f.open(t, host)
	resp, body = tn.do(t, "GET", "GET /always-gzip HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Berth-Refused") == "" || body != "" {
		t.Fatalf("encoded anyway: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	if _, err := tn.r.ReadByte(); err == nil {
		t.Fatal("the tunnel carried more after the refusal")
	}
}

// A response with no body passes whatever encoding it names, since there is nothing to mask.
func TestEncodedResponsesWithNoBodyPass(t *testing.T) {
	a := newUpstream(t)
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	host := a.authority()
	tn := f.open(t, host)
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"HEAD", "/always-gzip", http.StatusOK},
		{"GET", "/gzip-204", http.StatusNoContent},
		{"GET", "/gzip-304", http.StatusNotModified},
	} {
		resp, _ := tn.do(t, tc.method, tc.method+" "+tc.path+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
		if resp.StatusCode != tc.code || resp.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s %s: %d %v, want %d with its encoding", tc.method, tc.path, resp.StatusCode, resp.Header, tc.code)
		}
	}
}

// A header name a server could read as one berth-proxy sets or removes, or any name that is not a
// valid token, is refused before anything reaches upstream; so is such a name from upstream.
func TestLookAlikeHeaderNamesAreRefused(t *testing.T) {
	a := newUpstream(t)
	plain := testGrant(t, target(a.port(), nil))
	f := startProxy(t, upstreamPool(a), a, plain)
	host := a.authority()
	for _, h := range []string{"Accept-Encoding : gzip", "Authorization : Bearer x", "Accept_Encoding: gzip"} {
		tn := f.open(t, host)
		resp, _ := tn.do(t, "GET", "GET /echo HTTP/1.1\r\nHost: "+host+"\r\n"+h+"\r\n\r\n")
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Berth-Refused") == "" {
			t.Errorf("%q: %d %v, want 400 with a reason", h, resp.StatusCode, resp.Header)
		}
	}
	if h, _, _, _ := a.snapshot(); len(h) != 0 {
		t.Fatalf("%d refused requests reached upstream", len(h))
	}

	b := newUpstream(t)
	g := startProxy(t, upstreamPool(b), b, testGrant(t, target(b.port(), func(t *berthv1.ProxyTarget) { t.HeaderName = "X-Api-Key"; t.ValuePrefix = "" })))
	tn := g.open(t, b.authority())
	if resp, _ := tn.do(t, "GET", "GET / HTTP/1.1\r\nHost: "+b.authority()+"\r\nX_Api_Key: mine\r\n\r\n"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an underscore spelling of the granted header: %d, want 400", resp.StatusCode)
	}

	tn = f.open(t, host)
	resp, body := tn.do(t, "GET", "GET /bad-name HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Berth-Refused") == "" || body != "" {
		t.Fatalf("an invalid name from upstream: %d %v %q, want 502", resp.StatusCode, resp.Header, body)
	}
}

func TestIdleUpstreamCloseIsRedialled(t *testing.T) {
	a := newUpstreamWith(t, func(s *http.Server) { s.IdleTimeout = 50 * time.Millisecond })
	f := startProxy(t, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	tn := f.open(t, a.authority())
	for range 2 {
		resp, body := tn.do(t, "GET", "GET / HTTP/1.1\r\nHost: "+a.authority()+"\r\n\r\n")
		if resp.StatusCode != http.StatusOK || body != "ok " {
			t.Fatalf("%d %q", resp.StatusCode, body)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, _, conns, _ := a.snapshot(); conns != 2 {
		t.Fatalf("upstream connections %d, want 2", conns)
	}
}

// TestStalledBodyIsCutOff: a client that sends a head and then stops mid body loses its
// connection after the idle timeout instead of holding a handler and an upstream forever.
func TestStalledBodyIsCutOff(t *testing.T) {
	a := newUpstream(t)
	f := startProxyWith(t, ServerConfig{IdleTimeout: 200 * time.Millisecond}, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	tn := f.open(t, a.authority())
	if _, err := io.WriteString(tn.conn, "POST /x HTTP/1.1\r\nHost: "+a.authority()+"\r\nContent-Length: 10\r\n\r\nab"); err != nil {
		t.Fatal(err)
	}
	_ = tn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := tn.r.ReadByte()
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the proxy kept a stalled body open: %v", err)
	}
}

// TestConnectionCap: one client connection beyond the cap gets 503, and a freed slot is reused.
func TestConnectionCap(t *testing.T) {
	a := newUpstream(t)
	f := startProxyWith(t, ServerConfig{MaxConns: 2}, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	dial := func() net.Conn {
		c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	held := []net.Conn{dial(), dial()}
	over := dial()
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(over), &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection over the cap: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	_ = held[0].Close()
	head := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", a.authority(), a.authority(), basic(testCred))
	status := 0
	if !testutil.Eventually(func() bool { status = f.raw(t, head).StatusCode; return status == http.StatusOK }) {
		t.Fatalf("a freed slot was not reused: %d", status)
	}
}

// TestLaterHeadHasTheHeadTimeout: a tunnel may sit idle between requests, but a head that has
// started must arrive within the head timeout, not the idle one.
func TestLaterHeadHasTheHeadTimeout(t *testing.T) {
	a := newUpstream(t)
	f := startProxyWith(t, ServerConfig{HeadTimeout: 300 * time.Millisecond}, upstreamPool(a), a, testGrant(t, target(a.port(), nil)))
	tn := f.open(t, a.authority())
	time.Sleep(500 * time.Millisecond) // idle between requests is allowed
	if _, err := io.WriteString(tn.conn, "GET / HTTP/1.1\r\nHost: "); err != nil {
		t.Fatal(err)
	}
	_ = tn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(tn.r, &http.Request{Method: http.MethodGet})
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the proxy waited past the head timeout for a started head")
	}
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("a stalled head got %d", resp.StatusCode)
		}
	}
}
