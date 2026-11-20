package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
)

// scriptConn is the client side of a tunnel: reads take the script, then EOF, and writes are
// kept. Deadlines do nothing, since nothing waits on it.
type scriptConn struct {
	net.Conn
	in  io.Reader
	mu  sync.Mutex
	out bytes.Buffer
}

func (c *scriptConn) Read(p []byte) (int, error) { return c.in.Read(p) }
func (c *scriptConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}
func (c *scriptConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }
func (c *scriptConn) Close() error                     { return nil }

func (c *scriptConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes())
}

var (
	fuzzCAOnce  sync.Once
	fuzzCALeaf  *tls.Certificate
	fuzzCARoots *x509.CertPool
	errFuzzCA   error
)

// fuzzLeaf is example.com's leaf from one CA made per process, and the pool that trusts it.
func fuzzLeaf(t testing.TB) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	fuzzCAOnce.Do(func() {
		var ca *CA
		if ca, errFuzzCA = NewCA([]string{grantHost}, time.Now()); errFuzzCA != nil {
			return
		}
		fuzzCALeaf, errFuzzCA = ca.Leaf(grantHost)
		fuzzCARoots = x509.NewCertPool()
		fuzzCARoots.AddCert(ca.Certificate())
	})
	if errFuzzCA != nil {
		t.Fatal(errFuzzCA)
	}
	return fuzzCALeaf, fuzzCARoots
}

// fuzzGrants are made once per fuzz target: each value is a locked mapping that is never freed.
func fuzzGrants(f *testing.F) (open, limited *Grant) {
	f.Helper()
	mk := func(mod func(*berthv1.ProxyTarget)) *Grant {
		g, err := newGrant("api-key", target(443, mod), false)
		if err != nil {
			f.Fatal(err)
		}
		if g.value, err = lockValue([]byte(canary)); err != nil {
			f.Fatal(err)
		}
		f.Cleanup(g.value.Zero)
		return g
	}
	return mk(nil), mk(func(t *berthv1.ProxyTarget) { t.Methods, t.PathGlob = []string{"GET", "POST"}, "/api/*" })
}

func fuzzServer() *Server {
	return NewServer(ServerConfig{
		Log: slog.New(slog.DiscardHandler),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no network in the fuzzer")
		},
	})
}

// seen is one request as a reader parsed it.
type seen struct {
	method, uri, host string
	header            http.Header
	body              []byte
}

// readAll parses requests from r until the first that does not parse or whose body is broken.
func readAll(r io.Reader) []seen {
	br := bufio.NewReader(r)
	var out []seen
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return out
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return out
		}
		out = append(out, seen{req.Method, req.RequestURI, req.Host, req.Header, body})
	}
}

// fakeUpstream serves the proxy's side of a pipe: it parses each request, records it once its
// body is whole, and answers with answer, until the proxy closes the pipe or sends something it
// cannot parse. With once it closes after its first answer, as a server ends a response that
// has no length.
func fakeUpstream(conn, raw net.Conn, answer []byte, once bool) (got func() []seen) {
	var mu sync.Mutex
	var reqs []seen
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Closing the pipe, not TLS: close_notify would wait 5 seconds for a reader.
		defer func() { _ = raw.Close() }()
		br := bufio.NewReader(conn)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return
			}
			mu.Lock()
			reqs = append(reqs, seen{req.Method, req.RequestURI, req.Host, req.Header, body})
			mu.Unlock()
			if _, err := conn.Write(answer); err != nil || once {
				return
			}
		}
	}()
	return func() []seen {
		<-done
		return reqs
	}
}

// tunnelRun runs one forwarder over a scripted client and a fake upstream and returns what the
// client got and what upstream parsed.
func tunnelRun(t testing.TB, s *Server, g *Grant, script, answer []byte, once bool) (client []byte, got []seen) {
	t.Helper()
	proxySide, upSide := net.Pipe()
	// Real TLS on the pipe, since the forwarder writes the value only to a *tls.Conn.
	leaf, roots := fuzzLeaf(t)
	got1 := fakeUpstream(tls.Server(upSide, &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS13}), upSide, answer, once)
	up := tls.Client(proxySide, &tls.Config{ServerName: grantHost, RootCAs: roots, MinVersion: tls.VersionTLS13})
	// The proxy handshakes before any request, in dialUpstream; so does the harness.
	if err := up.HandshakeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := &scriptConn{in: bytes.NewReader(script)}
	f := &forwarder{s: s, g: g, client: c, raw: c}
	f.setUpstream(up)
	served := make(chan struct{})
	go func() {
		defer close(served)
		f.serve()
	}()
	select {
	case <-served:
	case <-time.After(hangAfter):
		t.Fatalf("the forwarder hung on %q", script)
	}
	_ = proxySide.Close()
	return c.written(), got1()
}

// hangAfter is how long one input may take before the forwarder counts as hung; it is far above
// any run, and the -timeout of make fuzz, which bounds the run as a whole, stays above it.
const hangAfter = 30 * time.Second

const okAnswer = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"

// FuzzForward feeds the requests a client sends through a tunnel to a granted host, and checks
// that upstream sees exactly a prefix of the requests the client's bytes hold, each with the
// same method, target, host, headers less the hop by hop ones and body, the granted header once
// and nothing else: no request smuggled in, none split or merged, no limit skipped.
func FuzzForward(f *testing.F) {
	open, limited := fuzzGrants(f)
	s := fuzzServer()
	seeds := []string{
		"GET / HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com:443\r\nAuthorization: Bearer stolen\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nContent-Length: 3\r\n\r\n5\r\nhello\r\n0\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\nhello!",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n5;ext=1\r\nhello\r\n0\r\nTrailer: x\r\nAuthorization: Bearer t\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: gzip, chunked\r\n\r\n0\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\nffffffffffffffffff\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n5\nhello\n0\n\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: Chunked\r\n\r\n0\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding : chunked\r\n\r\n0\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: +5\r\n\r\nhello",
		"POST /api/x HTTP/1.1\r\nHost: example.com\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\nhi",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nConnection: keep-alive, X-Drop\r\nX-Drop: 1\r\nX-Keep: 2\r\n\r\nGET /api/y HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\nGET /api/y HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: other.example\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nHost: other.example\r\n\r\n",
		"GET http://example.com/api/x HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/../admin HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nX-HTTP-Method-Override: DELETE\r\n\r\n",
		"DELETE /api/x HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nX: a\r\n b\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nX: a\rb\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nX: a\x00b\r\n\r\n",
		"GET /api/x HTTP/1.1\nHost: example.com\n\n",
		"GET /api/x HTTP/1.0\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/2.0\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: Basic x\r\nKeep-Alive: 1\r\nTE: trailers\r\nUpgrade: h2c\r\n\r\n",
		"\r\nGET /api/x HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n",
		"HEAD /api/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nbody",
	}
	for _, s := range seeds {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Add([]byte("GET /api/x HTTP/1.1\r\nHost: example.com\r\nX: "+strings.Repeat("a", maxHead)+"\r\n\r\n"), false)
	if _, got := tunnelRun(f, s, limited, []byte(seeds[2]+seeds[3]), []byte(okAnswer), false); len(got) != 2 {
		f.Fatalf("the harness forwards %d of two good requests", len(got))
	}
	f.Fuzz(func(t *testing.T, script []byte, limit bool) {
		g := open
		if limit {
			g = limited
		}
		before, _ := s.Counts()
		_, got := tunnelRun(t, s, g, script, []byte(okAnswer), false)
		after, _ := s.Counts()
		if int(after-before) != len(got) {
			t.Fatalf("%q: the proxy injected %d requests, upstream parsed %d", script, after-before, len(got))
		}
		head := script[:min(len(script), maxHead)]
		if len(got) > 0 && !bytes.Contains(head, []byte("\n\r\n")) && !bytes.Contains(head, []byte("\n\n")) {
			t.Fatalf("%q: a head past %d bytes reached upstream", script, maxHead)
		}
		sent := readAll(bytes.NewReader(script))
		if len(got) > len(sent) {
			t.Fatalf("%q: upstream parsed %d requests from %d the client sent", script, len(got), len(sent))
		}
		for i, up := range got {
			want := sent[i]
			if up.method != want.method || up.uri != want.uri || !bytes.Equal(up.body, want.body) {
				t.Fatalf("%q: request %d reached upstream as %s %s %q, the client sent %s %s %q",
					script, i, up.method, up.uri, up.body, want.method, want.uri, want.body)
			}
			if limit && (!slices.Contains(g.Methods, up.method) || !strings.HasPrefix(up.uri, "/api/")) {
				t.Fatalf("%q: %s %s passed the method and path limits", script, up.method, up.uri)
			}
			if !strings.EqualFold(up.host, want.host) || !strings.HasPrefix(strings.ToLower(up.host), grantHost) {
				t.Fatalf("%q: request %d reached upstream for host %q, the client said %q", script, i, up.host, want.host)
			}
			if v := up.header.Values("Authorization"); len(v) != 1 || v[0] != "Bearer "+canary {
				t.Fatalf("%q: request %d reached upstream with Authorization %q", script, i, v)
			}
			wantHeader := forwardedHeader(want.header)
			gotHeader := up.header.Clone()
			gotHeader.Del("Authorization")
			gotHeader.Del("Content-Length")
			gotHeader.Del("Transfer-Encoding")
			if !maps.EqualFunc(gotHeader, wantHeader, slices.Equal) {
				t.Fatalf("%q: request %d reached upstream with headers %v, the client sent %v", script, i, gotHeader, wantHeader)
			}
		}
	})
}

// forwardedHeader is what of the client's headers the proxy must pass: everything but the hop
// by hop headers, the ones Connection names, Proxy-* and the granted header.
func forwardedHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range append(connectionTokens(h), hopByHop...) {
		out.Del(k)
	}
	for k := range out {
		if strings.HasPrefix(k, "Proxy-") {
			delete(out, k)
		}
	}
	out.Del("Authorization")
	return out
}

// FuzzRelay answers one request with any bytes upstream might send and checks that the client
// gets no byte of the value outside a body the proxy cannot read, and the same responses
// upstream sent: each status and body, the value masked, no response smuggled in.
func FuzzRelay(f *testing.F) {
	open, _ := fuzzGrants(f)
	s := fuzzServer()
	const v = "$V"
	for _, a := range []string{
		okAnswer,
		"HTTP/1.1 200 OK\r\nX-Echo: Bearer $V\r\nContent-Length: 26\r\n\r\nbody says Bearer $V",
		"HTTP/1.1 200 $V\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 200 OK\r\n$V: 1\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nc4n4\r\n15\r\nry-s3cr3t-v4lue tail\r\n0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\n$V\r\n0\r\nX-Trailer: $V\r\n\r\n",
		"HTTP/1.1 200 OK\r\n\r\nuntil close $V",
		"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: $V\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\n\r\n$V",
		"HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\n$V",
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 20\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Length: 3\r\n\r\nok!",
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nokHTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.1 200 OK\r\nX: a\r\n b $V\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nConnection: X-Secret\r\nX-Secret: $V\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.0 200 OK\r\n\r\n$V",
		"garbage",
	} {
		f.Add([]byte(a))
	}
	if client, _ := tunnelRun(f, s, open, []byte("GET /x HTTP/1.1\r\nHost: example.com\r\n\r\n"), []byte(okAnswer), true); !bytes.HasSuffix(client, []byte("\r\n\r\nok")) {
		f.Fatalf("the harness relays %q", client)
	}
	f.Fuzz(func(t *testing.T, answer []byte) {
		answer = bytes.ReplaceAll(answer, []byte(v), []byte(canary))
		script := []byte("GET /x HTTP/1.1\r\nHost: example.com\r\n\r\n")
		client, _ := tunnelRun(t, s, open, script, answer, true)
		encoded := bytes.Contains(bytes.ToLower(answer), []byte("content-encoding"))
		if !encoded && bytes.Contains(client, []byte(canary)) {
			t.Fatalf("upstream answered %q and the client got the value: %q", answer, client)
		}
		want, wantErr := responses(bytes.NewReader(answer))
		got, _ := responses(bytes.NewReader(client))
		if wantErr != nil || encoded || len(want) == 0 || want[len(want)-1].code == http.StatusSwitchingProtocols {
			return
		}
		masked := func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(canary), bytes.Repeat([]byte("*"), len(canary)))
		}
		if len(got) != len(want) {
			t.Fatalf("upstream answered %q (%d responses), the client got %q (%d)", answer, len(want), client, len(got))
		}
		for i := range want {
			if got[i].code != want[i].code || !bytes.Equal(got[i].body, masked(want[i].body)) {
				t.Fatalf("upstream answered %q, the client got %q: response %d is %d %q, want %d %q",
					answer, client, i, got[i].code, got[i].body, want[i].code, masked(want[i].body))
			}
		}
	})
}

type answered struct {
	code int
	body []byte
}

// responses parses the answers to one GET: informational ones, then the final one.
func responses(r io.Reader) ([]answered, error) {
	br := bufio.NewReader(r)
	var out []answered
	for {
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
		if err != nil {
			return out, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		out = append(out, answered{resp.StatusCode, body})
		if err != nil || resp.StatusCode >= 200 || resp.StatusCode == http.StatusSwitchingProtocols {
			return out, err
		}
	}
}

// FuzzConnectHead feeds the first head a client sends berth-proxy and checks that it dials
// only for an authorized CONNECT, only the target that CONNECT names, answers with one status
// line and never echoes the credential.
func FuzzConnectHead(f *testing.F) {
	auth := basic(testCred)
	for _, h := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT EXAMPLE.com HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT example.com:443 HTTP/1.1\r\nProxy-Authorization: Basic eDp5\r\n\r\n",
		"CONNECT example.com:443 HTTP/1.1\r\n\r\n",
		"GET http://example.com/ HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT example.com:0 HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT example.com:99999 HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT [::1]:443 HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT :443 HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT a.example:443 HTTP/1.1\r\nHost: b.example:443\r\nProxy-Authorization: " + auth + "\r\n\r\n",
		"CONNECT example.com:443 HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\nProxy-Authorization: Basic eDp5\r\n\r\n",
		"CONNECT example.com:443 HTTP/1.1\r\nProxy-Authorization: basic  " + strings.TrimPrefix(auth, "Basic ") + " \r\n\r\n",
		"CONNECT /path HTTP/1.1\r\nProxy-Authorization: " + auth + "\r\n\r\n",
	} {
		f.Add([]byte(h), false)
		f.Add([]byte(h), true)
	}
	f.Fuzz(func(t *testing.T, head []byte, grantedOnly bool) {
		var dialed []string
		s := NewServer(ServerConfig{
			Credential: testCred, GrantedOnly: grantedOnly, Log: slog.New(slog.DiscardHandler),
			Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
				dialed = append(dialed, addr)
				return nil, errors.New("no network in the fuzzer")
			},
		})
		c := &scriptConn{in: bytes.NewReader(head)}
		s.handle(c)
		out := c.written()
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("%q answered %q, which is not one response: %v", head, out, err)
		}
		_ = resp.Body.Close()
		if rest := bytes.SplitN(out, []byte("\r\n\r\n"), 2); len(rest) != 2 || len(rest[1]) != 0 {
			t.Fatalf("%q answered more than one head: %q", head, out)
		}
		if bytes.Contains(out, []byte(testCred)) || bytes.Contains(out, []byte(strings.TrimPrefix(auth, "Basic "))) {
			t.Fatalf("%q answered with the credential: %q", head, out)
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
		authorized := err == nil && req.Method == http.MethodConnect && s.authorized(req.Header.Get("Proxy-Authorization"))
		switch {
		case grantedOnly && len(dialed) > 0:
			t.Fatalf("%q dialed %v with no grant in granted only mode", head, dialed)
		case !authorized && len(dialed) > 0:
			t.Fatalf("%q dialed %v without an authorized CONNECT", head, dialed)
		case len(dialed) > 1:
			t.Fatalf("%q dialed %v", head, dialed)
		case len(dialed) == 1:
			host, port, ok := connectTarget(req.Host)
			if !ok || dialed[0] != net.JoinHostPort(host, strconv.Itoa(port)) || !strings.Contains(strings.ToLower(req.Host), host) || resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("%q dialed %v and answered %d", head, dialed, resp.StatusCode)
			}
		case resp.StatusCode == http.StatusOK:
			t.Fatalf("%q answered 200 without a tunnel", head)
		}
	})
}
