package cdpproxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"berth/internal/adapters/cdpproxy"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

const token = "proxy-test-token-canary-0c4f"

func serve(t *testing.T) (*testutil.FakeBrowser, string, func()) {
	t.Helper()
	return serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler)})
}

func serveWith(t *testing.T, p *cdpproxy.Proxy) (*testutil.FakeBrowser, string, func()) {
	t.Helper()
	conn, err := (&testutil.Browsers{}).Launch(t.Context(), ports.BrowserLaunch{})
	if err != nil {
		t.Fatal(err)
	}
	fb := conn.(*testutil.FakeBrowser)
	endpoint, stop, err := p.Serve("0123456789abcdef", fb, sha256.Sum256([]byte(token)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return fb, endpoint, stop
}

func get(t *testing.T, url string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// dial opens the browser's WebSocket, with the token when withToken is set, and returns the
// HTTP status of a refused upgrade.
func dial(t *testing.T, endpoint string, withToken bool) (*websocket.Conn, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{}
	if withToken {
		opts.HTTPHeader = http.Header{cdpproxy.TokenHeader: {token}}
	}
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(endpoint, "http")+"/devtools/browser/0123456789abcdef", opts)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return ws, status, err
}

func read(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTheTokenGatesEverything(t *testing.T) {
	_, endpoint, _ := serve(t)
	for _, tc := range []struct {
		path   string
		header map[string]string
		code   int
	}{
		{"/json/version", nil, http.StatusUnauthorized},
		{"/json/version", map[string]string{cdpproxy.TokenHeader: "wrong"}, http.StatusUnauthorized},
		{"/json/list", nil, http.StatusUnauthorized},
		{"/devtools/browser/0123456789abcdef", nil, http.StatusUnauthorized},
		{"/json/version", map[string]string{cdpproxy.TokenHeader: token, "Origin": "https://evil.example"}, http.StatusForbidden},
		{"/json/list", map[string]string{cdpproxy.TokenHeader: token}, http.StatusNotFound},
		{"/json/new", map[string]string{cdpproxy.TokenHeader: token}, http.StatusNotFound},
	} {
		if code, _ := get(t, endpoint+tc.path, tc.header); code != tc.code {
			t.Errorf("%s %v: %d, want %d", tc.path, tc.header, code, tc.code)
		}
	}
	code, body := get(t, endpoint+"/json/version", map[string]string{cdpproxy.TokenHeader: token})
	want := `"webSocketDebuggerUrl":"ws://` + strings.TrimPrefix(endpoint, "http://") + `/devtools/browser/0123456789abcdef"`
	if code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("version: %d %s", code, body)
	}
	if _, status, err := dial(t, endpoint, false); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("a WebSocket without the token must get 401: %v", err)
	}
}

func TestAllowedPassesAndDeniedIsAnsweredByTheProxy(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	ctx := t.Context()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Page.enable","sessionId":"S"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"id":2,"method":"SystemInfo.getInfo","sessionId":"S"}`)); err != nil {
		t.Fatal(err)
	}
	m := read(t, ws)
	if m["id"] != float64(2) || m["sessionId"] != "S" || !strings.Contains(m["error"].(map[string]any)["message"].(string), "refused by Berth: SystemInfo.getInfo") {
		t.Fatalf("refusal = %v", m)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(fb.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sent := fb.Sent(); len(sent) != 1 || !strings.Contains(string(sent[0]), "Page.enable") {
		t.Fatalf("browser got %q; only the allowed message may reach it", sent)
	}
	if err := fb.Emit([]byte(`{"id":1,"result":{},"sessionId":"S"}`)); err != nil {
		t.Fatal(err)
	}
	if m := read(t, ws); m["id"] != float64(1) {
		t.Fatalf("reply = %v", m)
	}
}

func TestOneConnectionAtATimeAndMalformedEndsIt(t *testing.T) {
	_, endpoint, stop := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, err := dial(t, endpoint, true); err == nil || status != http.StatusConflict {
		t.Fatalf("second connection: %v", err)
	}
	if err := ws.Write(t.Context(), websocket.MessageText, []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("a malformed message must end the connection with a policy violation, got %v", err)
	}
	// The slot frees once the connection is gone.
	deadline := time.Now().Add(2 * time.Second)
	var next *websocket.Conn
	for next == nil {
		if next, _, err = dial(t, endpoint, true); err != nil && time.Now().After(deadline) {
			t.Fatalf("no new connection after the first ended: %v", err)
		}
	}
	stop()
	if _, _, err := next.Read(ctx); err == nil {
		t.Fatal("stop must end the open connection")
	}
}

func TestTheConnectionEndsWithTheBrowser(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	fb.Exit()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("the connection must end when the browser does: %v", err)
	}
}

// An agent that stops reading while large replies pile up is dropped once the queued bytes pass
// the limit, long before the count limit, so berthd never buffers without bound.
func TestASlowAgentIsDroppedPastTheByteLimit(t *testing.T) {
	fb, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), MaxQueuedBytes: 4 << 20})
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(8 << 20)
	big := []byte(`{"method":"Page.big","params":{"pad":"` + strings.Repeat("x", 1<<20) + `"}}`)
	const sent = 64
	// The handshake can finish before the proxy sets its sink, and the proxy lets go of the sink
	// once it drops the agent, so wait for the first message to land and stop at the drop.
	for deadline := time.Now().Add(5 * time.Second); fb.Emit(big) != nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the proxy never took the browser's messages")
		}
	}
	for range sent - 1 {
		if fb.Emit(big) != nil {
			break
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := 0
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the connection neither ended nor delivered everything")
			}
			break
		}
		got++
	}
	if got >= sent {
		t.Fatalf("the agent got all %d replies (%d MiB); the byte limit must drop it first", got, got)
	}
}

func TestDownloadBehaviourIsAnsweredByTheProxy(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if err := ws.Write(t.Context(), websocket.MessageText, []byte(`{"id":4,"method":"Browser.setDownloadBehavior","params":{"behavior":"allow","downloadPath":"/Users/you"}}`)); err != nil {
		t.Fatal(err)
	}
	m := read(t, ws)
	if m["id"] != float64(4) || m["error"] != nil || m["result"] == nil {
		t.Fatalf("reply = %v, want an empty success", m)
	}
	if len(fb.Sent()) != 0 {
		t.Fatalf("browser got %q; the download behaviour must never reach it", fb.Sent())
	}
}

func write(t *testing.T, ws *websocket.Conn, raw string) {
	t.Helper()
	if err := ws.Write(t.Context(), websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

// redial connects again once the previous connection's state is undone and its slot free.
func redial(t *testing.T, endpoint string) *websocket.Conn {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ws, _, err := dial(t, endpoint, true)
		if err == nil {
			return ws
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new connection after the first ended: %v", err)
		}
	}
}

// The browser and its pipe outlive each connection, so whatever one connection turned on must
// not reach the next: a target paused waiting for the debugger is resumed, its sessions are
// detached, browser level Fetch and auto attach are turned off, and the replies to its calls
// still in flight are dropped.
func TestAnEndedConnectionLeavesNothingToTheNext(t *testing.T) {
	fb, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), ResetWait: 100 * time.Millisecond})
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, `{"id":1,"method":"Target.setAutoAttach","params":{"autoAttach":true,"waitForDebuggerOnStart":true,"flatten":true}}`)
	write(t, ws, `{"id":2,"method":"Fetch.enable"}`)
	write(t, ws, `{"id":3,"method":"Target.attachToTarget","params":{"targetId":"T2","flatten":true}}`)
	write(t, ws, `{"id":7,"method":"Target.getTargets"}`)
	for deadline := time.Now().Add(5 * time.Second); len(fb.Sent()) < 4; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the browser never got the messages")
		}
	}
	for _, raw := range []string{
		`{"id":1,"result":{}}`, `{"id":2,"result":{}}`,
		`{"method":"Target.attachedToTarget","params":{"sessionId":"S1","targetInfo":{},"waitingForDebugger":true}}`,
		`{"method":"Target.attachedToTarget","sessionId":"S1","params":{"sessionId":"S1-child","targetInfo":{},"waitingForDebugger":false}}`,
		`{"id":3,"result":{"sessionId":"S2"}}`,
	} {
		if err := fb.Emit([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for range 5 {
		read(t, ws)
	}
	_ = ws.CloseNow()

	next := redial(t, endpoint)
	defer func() { _ = next.CloseNow() }()
	var got []string
	for _, c := range fb.Calls() {
		got = append(got, c.Session+" "+c.Method+" "+c.Params)
	}
	for _, want := range []string{
		`S1 Runtime.runIfWaitingForDebugger `,
		` Target.detachFromTarget {"sessionId":"S1"}`,
		`S1 Target.detachFromTarget {"sessionId":"S1-child"}`,
		` Target.detachFromTarget {"sessionId":"S2"}`,
		` Fetch.disable `,
		` Target.setAutoAttach {"autoAttach":false,"waitForDebuggerOnStart":false,"flatten":true}`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the reset made no call %q; calls %q", want, got)
		}
	}
	// The first connection's getTargets is answered only now; the next connection must not see it.
	if err := fb.Emit([]byte(`{"id":7,"result":{"targetInfos":[]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := fb.Emit([]byte(`{"method":"Target.targetCreated","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if m := read(t, next); m["method"] != "Target.targetCreated" {
		t.Fatalf("the next connection got %v; a reply to the previous one's call must never reach it", m)
	}
	// Once that reply came, its id is free again.
	write(t, next, `{"id":7,"method":"Target.getTargets"}`)
	if err := fb.Emit([]byte(`{"id":7,"result":{"targetInfos":[]}}`)); err != nil {
		t.Fatal(err)
	}
	if m := read(t, next); m["id"] != float64(7) || m["error"] != nil {
		t.Fatalf("reply = %v", m)
	}
}

// An id an ended connection left unanswered is refused to the next until its reply comes, so a
// reply can never be read as the answer to the wrong call.
func TestAnIDLeftInFlightIsRefusedToTheNextConnection(t *testing.T) {
	fb, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), ResetWait: 50 * time.Millisecond})
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, `{"id":5,"method":"Target.getTargets"}`)
	for deadline := time.Now().Add(5 * time.Second); len(fb.Sent()) < 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the browser never got the message")
		}
	}
	_ = ws.CloseNow()
	next := redial(t, endpoint)
	defer func() { _ = next.CloseNow() }()
	write(t, next, `{"id":5,"method":"Target.getTargets"}`)
	m := read(t, next)
	if m["id"] != float64(5) || !strings.Contains(m["error"].(map[string]any)["message"].(string), "still in flight") {
		t.Fatalf("reply = %v, want a refusal", m)
	}
	if len(fb.Sent()) != 1 {
		t.Fatalf("browser got %q; the reused id must not reach it", fb.Sent())
	}
}

// Connections without the token hold no descriptor for long: a refused request closes its
// connection, and no more than MaxConns are open at once, whoever holds them.
func TestConnectionsAreCappedAndRefusalsClose(t *testing.T) {
	_, endpoint, _ := serve(t)
	addr := strings.TrimPrefix(endpoint, "http://")
	var d net.Dialer
	raw, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write([]byte("GET /json/version HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(raw); err != nil {
		t.Fatalf("a request without the token must get its answer and a closed connection, got %v", err)
	}
	_ = raw.Close()

	var held []net.Conn
	for range cdpproxy.MaxConns {
		c, err := d.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	versionWithin := func(d time.Duration) error {
		ctx, cancel := context.WithTimeout(t.Context(), d)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
		req.Header.Set(cdpproxy.TokenHeader, token)
		resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := versionWithin(300 * time.Millisecond); err == nil {
		t.Fatalf("a request past %d open connections was served", cdpproxy.MaxConns)
	}
	_ = held[0].Close()
	if err := versionWithin(5 * time.Second); err != nil {
		t.Fatalf("a freed slot must serve the next request: %v", err)
	}
}
