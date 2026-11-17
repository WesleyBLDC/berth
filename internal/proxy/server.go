package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxHead bounds one request head, like net/http's default.
	maxHead     = 1 << 20
	headTimeout = 30 * time.Second
	// idleTimeout closes a tunnel to a granted host that sends no request for this long.
	idleTimeout = 5 * time.Minute
	dialTimeout = 30 * time.Second
)

// DialFunc dials a TCP address; tests point it at local servers.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Server is the proxy's listener side.
type Server struct {
	grants map[string]*Grant
	ca     *CA
	auth   []byte
	roots  *x509.CertPool
	dial   DialFunc
	log    *slog.Logger

	injected atomic.Uint64
	refused  atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	ln     net.Listener
	conns  map[net.Conn]struct{}
}

// ServerConfig is what a Server needs. Roots nil means the system verifier.
type ServerConfig struct {
	Grants     []*Grant
	CA         *CA
	Credential string
	Roots      *x509.CertPool
	Dial       DialFunc
	Log        *slog.Logger
}

func NewServer(c ServerConfig) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		grants: map[string]*Grant{},
		ca:     c.CA,
		auth:   []byte("berth:" + c.Credential),
		roots:  c.Roots,
		dial:   c.Dial,
		log:    c.Log,
		ctx:    ctx,
		cancel: cancel,
		conns:  map[net.Conn]struct{}{},
	}
	for _, g := range c.Grants {
		s.grants[g.authority()] = g
	}
	if s.dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		s.dial = d.DialContext
	}
	return s
}

// Counts returns how many requests got the header and how many a limit turned away.
func (s *Server) Counts() (injected, refused uint64) { return s.injected.Load(), s.refused.Load() }

// Serve accepts until Close.
func (s *Server) Serve(ln net.Listener) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return
	}
	s.ln = ln
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !s.track(c) {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(c)
			s.handle(c)
		}()
	}
}

// Close stops accepting, closes every connection and waits up to wait for the handlers to
// return, so no goroutine still reads a value when the caller zeroes it. It reports whether
// every handler returned.
func (s *Server) Close(wait time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

// headLimit bounds what a head may read; body reads pass through unlimited.
type headLimit struct {
	r       io.Reader
	n       int
	limited bool
}

var errHeadTooLarge = errors.New("request head too large")

func (l *headLimit) start() { l.n, l.limited = maxHead, true }
func (l *headLimit) stop()  { l.limited = false }

func (l *headLimit) Read(p []byte) (int, error) {
	if !l.limited {
		return l.r.Read(p)
	}
	if l.n <= 0 {
		return 0, errHeadTooLarge
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= n
	return n, err
}

// bufConn reads through the buffered reader that parsed CONNECT, so bytes the client sent
// right behind it are not lost.
type bufConn struct {
	net.Conn
	r io.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func writeStatus(w io.Writer, code int, extra string) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\nConnection: close\r\n\r\n", code, http.StatusText(code), extra)
}

func (s *Server) authorized(header string) bool {
	scheme, encoded, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return false
	}
	defer clear(got)
	return subtle.ConstantTimeCompare(got, s.auth) == 1
}

// connectTarget lowercases the CONNECT authority; a missing port means 443.
func connectTarget(authority string) (host string, port int, ok bool) {
	h, p, err := net.SplitHostPort(authority)
	if err != nil {
		h, p = authority, "443"
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 || h == "" {
		return "", 0, false
	}
	return strings.ToLower(h), n, true
}

func (s *Server) handle(c net.Conn) {
	limit := &headLimit{r: c}
	br := bufio.NewReader(limit)
	_ = c.SetReadDeadline(time.Now().Add(headTimeout))
	limit.start()
	req, err := http.ReadRequest(br)
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "")
		return
	}
	limit.stop()
	_ = c.SetReadDeadline(time.Time{})
	if !s.authorized(req.Header.Get("Proxy-Authorization")) {
		writeStatus(c, http.StatusProxyAuthRequired, "Proxy-Authenticate: Basic realm=\"berth\"\r\n")
		return
	}
	if req.Method != http.MethodConnect {
		// Absolute form plain HTTP is never forwarded, so nothing is injected over cleartext.
		writeStatus(c, http.StatusMethodNotAllowed, "Allow: CONNECT\r\n")
		return
	}
	host, port, ok := connectTarget(req.Host)
	if !ok {
		writeStatus(c, http.StatusBadRequest, "")
		return
	}
	authority := net.JoinHostPort(host, strconv.Itoa(port))
	if g, ok := s.grants[authority]; ok {
		s.intercept(c, br, g)
		return
	}
	s.tunnel(c, br, authority)
}

// tunnel copies bytes both ways without looking at them; the client verifies the server itself.
func (s *Server) tunnel(c net.Conn, br *bufio.Reader, authority string) {
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	up, err := s.dial(ctx, "tcp", authority)
	cancel()
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "")
		return
	}
	if !s.track(up) {
		return
	}
	defer s.untrack(up)
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
	_ = up.Close()
	_ = c.Close()
	<-done
}

// dialUpstream connects to a granted host and verifies it as that host before any request
// byte exists to send, so a failed verification sends nothing.
func (s *Server) dialUpstream(g *Grant) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	raw, err := s.dial(ctx, "tcp", g.authority())
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{
		ServerName: g.Host,
		MinVersion: tls.VersionTLS12,
		RootCAs:    s.roots,
		NextProtos: []string{"http/1.1"},
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if !s.track(tc) {
		return nil, errors.New("proxy is closing")
	}
	return tc, nil
}

// intercept terminates the client's TLS with the granted host's leaf and forwards its requests.
func (s *Server) intercept(c net.Conn, br *bufio.Reader, g *Grant) {
	leaf, err := s.ca.Leaf(g.Host)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "")
		return
	}
	up, err := s.dialUpstream(g)
	if err != nil {
		s.log.Warn("upstream refused or failed verification", "host", g.Host, "port", g.Port)
		writeStatus(c, http.StatusBadGateway, "")
		return
	}
	f := &forwarder{s: s, g: g, raw: c}
	f.setUpstream(up)
	defer func() { s.untrack(f.up) }()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	client := tls.Server(bufConn{Conn: c, r: br}, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	_ = c.SetDeadline(time.Now().Add(headTimeout))
	if err := client.HandshakeContext(s.ctx); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	f.client = client
	f.serve()
}
