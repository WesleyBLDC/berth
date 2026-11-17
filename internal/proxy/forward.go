package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"berth/internal/mask"
)

// hopByHop are never forwarded in either direction; framing is rewritten by the proxy itself.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding",
	"Upgrade", "Content-Length", "Expect",
}

// forwarder serves one client tunnel to a granted host. It writes each request itself, from
// bytes, so the value goes from locked memory to the TLS connection without ever being a string
// or passing through a buffer the proxy does not zero.
type forwarder struct {
	s      *Server
	g      *Grant
	client *tls.Conn
	raw    net.Conn
	up     *tls.Conn
	upR    *bufio.Reader
}

func (f *forwarder) setUpstream(up *tls.Conn) {
	if f.up != nil {
		f.s.untrack(f.up)
	}
	f.up, f.upR = up, bufio.NewReader(up)
}

// upstreamAlive reports whether the kept upstream connection can carry another request: it has
// sent nothing unasked and has not been closed by the server while idle.
func (f *forwarder) upstreamAlive() bool {
	if f.upR.Buffered() > 0 {
		return false
	}
	_ = f.up.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := f.upR.Peek(1)
	_ = f.up.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (f *forwarder) serve() {
	limit := &headLimit{r: f.client}
	cr := bufio.NewReader(limit)
	for {
		_ = f.raw.SetReadDeadline(time.Now().Add(idleTimeout))
		limit.start()
		req, err := http.ReadRequest(cr)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				writeStatus(f.client, http.StatusBadRequest, "")
			}
			return
		}
		limit.stop()
		_ = f.raw.SetReadDeadline(time.Time{})
		if !f.forward(req) {
			return
		}
	}
}

// forward handles one request and reports whether the tunnel can carry another.
func (f *forwarder) forward(req *http.Request) bool {
	defer func() { _ = req.Body.Close() }()
	if code := f.check(req); code != 0 {
		if code != http.StatusBadRequest {
			f.s.refused.Add(1)
		}
		writeStatus(f.client, code, "")
		return false
	}
	if !f.upstreamAlive() {
		up, err := f.s.dialUpstream(f.g)
		if err != nil {
			f.s.log.Warn("upstream refused or failed verification", "host", f.g.Host, "port", f.g.Port)
			writeStatus(f.client, http.StatusBadGateway, "")
			return false
		}
		f.setUpstream(up)
	}
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		// Expect is not forwarded, so the client must hear 100 Continue from the proxy or it
		// would wait before sending the body.
		if _, err := io.WriteString(f.client, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return false
		}
	}
	if err := f.writeRequest(req); err != nil {
		return false
	}
	f.s.injected.Add(1)
	keep, err := f.relayResponse(req)
	return err == nil && keep && !req.Close
}

// check returns 0 when the request may get the header, or the status that refuses it.
func (f *forwarder) check(req *http.Request) int {
	if req.ProtoMajor != 1 || !strings.HasPrefix(req.RequestURI, "/") {
		return http.StatusBadRequest
	}
	for _, vs := range req.Header {
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n\x00") {
				return http.StatusBadRequest
			}
		}
	}
	host := strings.ToLower(req.Host)
	if host != f.g.authority() && (f.g.Port != 443 || host != f.g.Host) {
		return http.StatusMisdirectedRequest
	}
	if len(f.g.Methods) > 0 && !slices.Contains(f.g.Methods, req.Method) {
		return http.StatusForbidden
	}
	if f.g.PathGlob != "" {
		raw, _, _ := strings.Cut(req.RequestURI, "?")
		if ambiguousPath(raw) {
			return http.StatusForbidden
		}
		if ok, err := path.Match(f.g.PathGlob, raw); err != nil || !ok {
			return http.StatusForbidden
		}
	}
	return 0
}

// ambiguousPath is a path a server might resolve to somewhere the glob did not match: dot
// segments, doubled slashes, backslashes, path parameters (servers such as Tomcat drop what
// follows a semicolon in a segment, so ..;/ climbs), or an encoded slash, dot, backslash or
// semicolon.
func ambiguousPath(p string) bool {
	l := strings.ToLower(p)
	if strings.Contains(l, "//") || strings.Contains(l, `\`) || strings.Contains(l, ";") || strings.Contains(l, "%2f") ||
		strings.Contains(l, "%2e") || strings.Contains(l, "%5c") || strings.Contains(l, "%3b") {
		return true
	}
	for seg := range strings.SplitSeq(l, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func connectionTokens(h http.Header) []string {
	var tokens []string
	for _, v := range h.Values("Connection") {
		for t := range strings.SplitSeq(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, http.CanonicalHeaderKey(t))
			}
		}
	}
	return tokens
}

// forwardable reports whether a header from one side may reach the other.
func forwardable(key string, dropped []string) bool {
	return !slices.Contains(hopByHop, key) && !strings.HasPrefix(key, "Proxy-") &&
		!slices.Contains(dropped, key)
}

// writeRequest writes the head in three pieces so the value goes straight from locked memory to
// the connection, then streams the body with the client's framing.
func (f *forwarder) writeRequest(req *http.Request) error {
	dropped := append(connectionTokens(req.Header), f.g.Header, "Host")
	var head bytes.Buffer
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, req.RequestURI, req.Host)
	for _, k := range sortedKeys(req.Header) {
		if !forwardable(k, dropped) {
			continue
		}
		for _, v := range req.Header[k] {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
	}
	head.WriteString(f.g.Header)
	head.WriteString(": ")
	head.Write(f.g.Prefix)
	if _, err := f.up.Write(head.Bytes()); err != nil {
		return err
	}
	if _, err := f.up.Write(f.g.value.Bytes()); err != nil {
		return err
	}
	chunked := len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked"
	var tail bytes.Buffer
	tail.Write(f.g.Suffix)
	tail.WriteString("\r\n")
	switch {
	case chunked:
		tail.WriteString("Transfer-Encoding: chunked\r\n")
	case req.ContentLength > 0 || req.Header.Get("Content-Length") != "":
		fmt.Fprintf(&tail, "Content-Length: %d\r\n", max(req.ContentLength, 0))
	}
	tail.WriteString("\r\n")
	bw := bufio.NewWriter(f.up)
	_, _ = bw.Write(tail.Bytes())
	switch {
	case chunked:
		cw := &chunkWriter{w: bw}
		if _, err := io.Copy(cw, req.Body); err != nil {
			return err
		}
		// Request trailers are dropped: one could carry a second copy of the header.
		_, _ = bw.WriteString("0\r\n\r\n")
	case req.ContentLength > 0:
		n, err := io.Copy(bw, req.Body)
		if err != nil {
			return err
		}
		if n != req.ContentLength {
			return io.ErrUnexpectedEOF
		}
	}
	return bw.Flush()
}

func sortedKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// chunkWriter writes each Write as one chunk and flushes it, so a streamed body (server sent
// events, say) reaches the other side as it arrives.
type chunkWriter struct{ w *bufio.Writer }

func (c *chunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	_, _ = c.w.WriteString(strconv.FormatInt(int64(len(p)), 16) + "\r\n")
	_, _ = c.w.Write(p)
	_, _ = c.w.WriteString("\r\n")
	return len(p), c.w.Flush()
}

// flushWriter flushes after every Write, for the same reason.
type flushWriter struct{ w *bufio.Writer }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.w.Flush()
}

// redact replaces the value in s with asterisks of the same length. The copy it makes of a
// string that held the value is zeroed; the string itself came from the upstream response and
// is out of the proxy's reach.
func (f *forwarder) redact(s string) string {
	v := f.g.value.Bytes()
	b := []byte(s)
	defer clear(b)
	if !bytes.Contains(b, v) {
		return s
	}
	return string(bytes.ReplaceAll(b, v, bytes.Repeat([]byte{'*'}, len(v))))
}

func bodyAllowed(method string, code int) bool {
	return method != http.MethodHead && code != http.StatusNoContent && code != http.StatusNotModified &&
		(code < 100 || code >= 200)
}

// relayResponse copies one response to the client with the value redacted from its headers
// and, when it is identity encoded, its body. Redirects are passed to the client, never followed.
func (f *forwarder) relayResponse(req *http.Request) (keep bool, err error) {
	var resp *http.Response
	for {
		resp, err = http.ReadResponse(f.upR, req)
		if err != nil {
			writeStatus(f.client, http.StatusBadGateway, "")
			return false, err
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			return false, errors.New("upstream switched protocols")
		}
		if resp.StatusCode >= 200 {
			break
		}
		if err := f.writeHead(resp, ""); err != nil {
			return false, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	cw := bufio.NewWriter(f.client)
	hasBody := bodyAllowed(req.Method, resp.StatusCode)
	chunked := false
	keep = !resp.Close
	var framing string
	switch {
	case !hasBody:
		if cl := resp.Header.Get("Content-Length"); cl != "" && resp.StatusCode != http.StatusNoContent {
			framing = "Content-Length: " + cl + "\r\n"
		}
	case resp.ContentLength >= 0:
		framing = "Content-Length: " + strconv.FormatInt(resp.ContentLength, 10) + "\r\n"
	case len(resp.TransferEncoding) > 0 && resp.TransferEncoding[0] == "chunked":
		chunked = true
		framing = "Transfer-Encoding: chunked\r\n"
	default:
		// The body ends when upstream closes, so the client's connection must end with it.
		keep = false
	}
	if !keep {
		framing += "Connection: close\r\n"
	}
	if err := f.writeHead(resp, framing); err != nil {
		return false, err
	}
	if !hasBody {
		return keep, nil
	}
	var out io.Writer = flushWriter{w: cw}
	if chunked {
		out = &chunkWriter{w: cw}
	}
	ce := resp.Header.Get("Content-Encoding")
	if ce == "" || strings.EqualFold(ce, "identity") {
		m := mask.NewSameLength(out, [][]byte{f.g.value.Bytes()})
		_, err = io.Copy(m, resp.Body)
		if cerr := m.Close(); err == nil {
			err = cerr
		}
	} else {
		_, err = io.Copy(out, resp.Body)
	}
	if err != nil {
		return false, err
	}
	if chunked {
		_, _ = cw.WriteString("0\r\n\r\n")
	}
	return keep, cw.Flush()
}

func (f *forwarder) writeHead(resp *http.Response, framing string) error {
	dropped := connectionTokens(resp.Header)
	var head bytes.Buffer
	head.WriteString("HTTP/1.1 ")
	head.WriteString(f.redact(resp.Status))
	head.WriteString("\r\n")
	for _, k := range sortedKeys(resp.Header) {
		if !forwardable(k, dropped) || f.redact(k) != k {
			continue
		}
		for _, v := range resp.Header[k] {
			fmt.Fprintf(&head, "%s: %s\r\n", k, f.redact(v))
		}
	}
	head.WriteString(framing)
	head.WriteString("\r\n")
	_, err := f.client.Write(head.Bytes())
	return err
}
