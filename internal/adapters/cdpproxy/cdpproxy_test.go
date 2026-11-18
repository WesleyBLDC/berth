package cdpproxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	attach(t, fb, ws, "S", "")
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
	testutil.Eventually(func() bool { return len(fb.Sent()) > 0 })
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
	if !testutil.Eventually(func() bool { return fb.Emit(big) == nil }) {
		t.Fatal("the proxy never took the browser's messages")
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
	var ws *websocket.Conn
	var err error
	if !testutil.Eventually(func() bool { ws, _, err = dial(t, endpoint, true); return err == nil }) {
		t.Fatalf("no new connection after the first ended: %v", err)
	}
	return ws
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
	fb.Answer("Target.getTargetInfo", `{"targetInfo":{"targetId":"T2","type":"page","url":"about:blank"}}`)
	write(t, ws, `{"id":1,"method":"Target.setAutoAttach","params":{"autoAttach":true,"waitForDebuggerOnStart":true,"flatten":true}}`)
	write(t, ws, `{"id":2,"method":"Fetch.enable"}`)
	write(t, ws, `{"id":3,"method":"Target.attachToTarget","params":{"targetId":"T2","flatten":true}}`)
	write(t, ws, `{"id":7,"method":"Target.getTargets"}`)
	if !testutil.Eventually(func() bool { return len(fb.Sent()) >= 4 }) {
		t.Fatal("the browser never got the messages")
	}
	for _, raw := range []string{
		`{"id":1,"result":{}}`, `{"id":2,"result":{}}`,
		`{"method":"Target.attachedToTarget","params":{"sessionId":"S1","targetInfo":{"type":"page"},"waitingForDebugger":true}}`,
		`{"method":"Target.attachedToTarget","sessionId":"S1","params":{"sessionId":"S1-child","targetInfo":{"type":"iframe"},"waitingForDebugger":false}}`,
		`{"method":"Target.attachedToTarget","params":{"sessionId":"S2","targetInfo":{"targetId":"T2","type":"page"},"waitingForDebugger":false}}`,
		`{"id":3,"result":{"sessionId":"S2"}}`,
	} {
		if err := fb.Emit([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for range 6 {
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
	if !testutil.Eventually(func() bool { return len(fb.Sent()) >= 1 }) {
		t.Fatal("the browser never got the message")
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

// An agent attaches only to web content: the browser target would give it a browser session,
// which is Berth's, and Chrome's own pages reach settings and the disk. A session auto attach
// opens to such a target never reaches the agent and is detached.
func TestOnlyWebContentCanBeAttachedTo(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	fb.Answer("Target.getTargetInfo", `{"targetInfo":{"targetId":"B","type":"browser","url":""}}`)
	write(t, ws, `{"id":1,"method":"Target.attachToTarget","params":{"targetId":"B","flatten":true}}`)
	if m := read(t, ws); m["id"] != float64(1) || !strings.Contains(m["error"].(map[string]any)["message"].(string), "only web content") {
		t.Fatalf("attach to the browser target = %v, want a refusal", m)
	}
	fb.Answer("Target.getTargetInfo", `{"targetInfo":{"targetId":"P","type":"page","url":"https://a.example/"}}`)
	write(t, ws, `{"id":2,"method":"Target.attachToTarget","params":{"targetId":"P","flatten":true}}`)
	if !testutil.Eventually(func() bool { return len(fb.Sent()) >= 1 }) {
		t.Fatal("an attach to a web page never reached the browser")
	}
	if sent := fb.Sent(); len(sent) != 1 || !strings.Contains(string(sent[0]), `"targetId":"P"`) {
		t.Fatalf("browser got %q; only the attach to the page may reach it", sent)
	}

	for _, raw := range []string{
		`{"method":"Target.attachedToTarget","params":{"sessionId":"UI","targetInfo":{"type":"page","url":"chrome://settings/"},"waitingForDebugger":true}}`,
		`{"method":"Runtime.executionContextCreated","sessionId":"UI","params":{}}`,
		`{"method":"Target.detachedFromTarget","params":{"sessionId":"UI"}}`,
		`{"method":"Target.attachedToTarget","params":{"sessionId":"WEB","targetInfo":{"type":"page","url":"https://a.example/"},"waitingForDebugger":false}}`,
	} {
		if err := fb.Emit([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if m := read(t, ws); m["params"].(map[string]any)["sessionId"] != "WEB" {
		t.Fatalf("the agent got %v; nothing about a session to Chrome's own page may reach it", m)
	}
	var got []string
	for deadline := time.Now().Add(5 * time.Second); len(got) < 2; time.Sleep(5 * time.Millisecond) {
		got = got[:0]
		for _, c := range fb.Calls() {
			if c.Session == "UI" || strings.Contains(c.Params, `"UI"`) {
				got = append(got, c.Session+" "+c.Method+" "+c.Params)
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !slices.Equal(got, []string{`UI Runtime.runIfWaitingForDebugger `, ` Target.detachFromTarget {"sessionId":"UI"}`}) {
		t.Fatalf("calls about the hidden session = %q, want it resumed and detached", got)
	}
}

func waitSent(t *testing.T, fb *testutil.FakeBrowser, n int) {
	t.Helper()
	if !testutil.Eventually(func() bool { return len(fb.Sent()) >= n }) {
		t.Fatalf("the browser got %d messages, want %d", len(fb.Sent()), n)
	}
}

func refusedWith(t *testing.T, m map[string]any, id float64, why string) {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	if m["id"] != id || e == nil || !strings.Contains(e["message"].(string), why) {
		t.Fatalf("reply = %v, want a refusal of %v naming %q", m, id, why)
	}
}

// A late reply that comes while no agent is connected clears its id, so the next connection
// may use it again.
func TestAReplyWhileNobodyIsConnectedFreesItsID(t *testing.T) {
	fb, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), ResetWait: 20 * time.Millisecond})
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, `{"id":5,"method":"Target.getTargets"}`)
	waitSent(t, fb, 1)
	_ = ws.CloseNow()
	time.Sleep(300 * time.Millisecond)
	_ = fb.Emit([]byte(`{"id":5,"result":{"targetInfos":[]}}`))
	next := redial(t, endpoint)
	defer func() { _ = next.CloseNow() }()
	write(t, next, `{"id":5,"method":"Target.getTargets"}`)
	waitSent(t, fb, 2)
}

// An id already in flight is refused, so a second reply to it can never reach the next
// connection.
func TestAnIDAlreadyInFlightIsRefused(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	write(t, ws, `{"id":3,"method":"Target.getTargets"}`)
	write(t, ws, `{"id":3,"method":"Target.getTargets"}`)
	refusedWith(t, read(t, ws), 3, "already in flight")
	if len(fb.Sent()) != 1 {
		t.Fatalf("browser got %q; the repeated id must not reach it", fb.Sent())
	}
}

// Auto attach goes off before the sessions are counted, so a target attached while the reset
// runs is resumed and detached too, and never reaches the next connection.
func TestATargetAttachedDuringTheResetIsUndone(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, `{"id":1,"method":"Target.setAutoAttach","params":{"autoAttach":true,"waitForDebuggerOnStart":true,"flatten":true}}`)
	waitSent(t, fb, 1)
	if err := fb.Emit([]byte(`{"id":1,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	read(t, ws)
	fb.OnCall("Target.setAutoAttach", func() {
		_ = fb.Emit([]byte(`{"method":"Target.attachedToTarget","params":{"sessionId":"S9","targetInfo":{"targetId":"T9","type":"page","url":"about:blank"},"waitingForDebugger":true}}`))
	})
	_ = ws.CloseNow()
	next := redial(t, endpoint)
	defer func() { _ = next.CloseNow() }()
	waitCall(t, fb, "S9", "Target.detachFromTarget")
	var got []string
	for _, c := range fb.Calls() {
		got = append(got, c.Session+" "+c.Method+" "+c.Params)
	}
	off := slices.Index(got, ` Target.setAutoAttach {"autoAttach":false,"waitForDebuggerOnStart":false,"flatten":true}`)
	resumed := slices.Index(got, `S9 Runtime.runIfWaitingForDebugger `)
	detached := slices.Index(got, ` Target.detachFromTarget {"sessionId":"S9"}`)
	if off < 0 || resumed < off || detached < resumed {
		t.Fatalf("calls %q; auto attach must go off first, then the new target be resumed and detached", got)
	}
	// The session attached after the connection ended is hidden: nothing of it reaches the next.
	if err := fb.Emit([]byte(`{"method":"Runtime.consoleAPICalled","sessionId":"S9","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if err := fb.Emit([]byte(`{"method":"Target.targetCreated","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if m := read(t, next); m["method"] != "Target.targetCreated" {
		t.Fatalf("the next connection got %v", m)
	}
}

// attach hands the connection a session, as Chrome's attachedToTarget does, and reads it.
func attach(t *testing.T, fb *testutil.FakeBrowser, ws *websocket.Conn, session, parent string) {
	t.Helper()
	msg := `{"method":"Target.attachedToTarget","params":{"sessionId":"` + session + `","targetInfo":{"targetId":"T-` + session + `","type":"page","url":"about:blank"},"waitingForDebugger":false}`
	if parent != "" {
		msg += `,"sessionId":"` + parent + `"`
	}
	if err := fb.Emit([]byte(msg + `}`)); err != nil {
		t.Fatal(err)
	}
	if m := read(t, ws); m["method"] != "Target.attachedToTarget" {
		t.Fatalf("got %v, want the attach", m)
	}
}

// waitCall waits until Berth made a call of method about session.
func waitCall(t *testing.T, fb *testutil.FakeBrowser, session, method string) {
	t.Helper()
	if !testutil.Eventually(func() bool {
		return slices.ContainsFunc(fb.Calls(), func(c testutil.FakeCall) bool {
			return c.Method == method && (c.Session == session || strings.Contains(c.Params, `"`+session+`"`))
		})
	}) {
		t.Fatalf("no %s call about %s; calls %v", method, session, fb.Calls())
	}
}

// A call goes only to a session the connection holds: never one it was not handed, one that
// detached, or one hidden from it, which would otherwise run on Chrome's own page before the
// detach lands. A reply on a hidden session still clears its call, so the id never sticks.
func TestACallOnASessionNotHeldIsRefused(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	write(t, ws, `{"id":4,"sessionId":"NOPE","method":"Runtime.evaluate","params":{"expression":"1"}}`)
	refusedWith(t, read(t, ws), 4, "not attached")

	attach(t, fb, ws, "GONE", "")
	if err := fb.Emit([]byte(`{"method":"Target.detachedFromTarget","params":{"sessionId":"GONE"}}`)); err != nil {
		t.Fatal(err)
	}
	read(t, ws)
	write(t, ws, `{"id":5,"sessionId":"GONE","method":"Runtime.evaluate","params":{"expression":"1"}}`)
	refusedWith(t, read(t, ws), 5, "not attached")

	attach(t, fb, ws, "S", "")
	write(t, ws, `{"id":7,"sessionId":"S","method":"Runtime.evaluate","params":{"expression":"1"}}`)
	waitSent(t, fb, 1)
	if err := fb.Emit([]byte(`{"method":"Target.targetInfoChanged","params":{"targetInfo":{"targetId":"T-S","type":"page","url":"chrome://settings/"}}}`)); err != nil {
		t.Fatal(err)
	}
	read(t, ws)
	write(t, ws, `{"id":8,"sessionId":"S","method":"Runtime.evaluate","params":{"expression":"1"}}`)
	refusedWith(t, read(t, ws), 8, "not attached")
	if err := fb.Emit([]byte(`{"id":7,"sessionId":"S","result":{}}`)); err != nil {
		t.Fatal(err)
	}
	write(t, ws, `{"id":7,"method":"Target.getTargets"}`)
	waitSent(t, fb, 2)
	if len(fb.Sent()) != 2 {
		t.Fatalf("browser got %q", fb.Sent())
	}
}

// A hidden session whose detach Chrome refuses is gone, so its calls are forgotten and their ids
// free again.
func TestAFailedDetachFreesTheHiddenSessionsCalls(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	fb.Refuse("Target.detachFromTarget")
	attach(t, fb, ws, "S", "")
	write(t, ws, `{"id":7,"sessionId":"S","method":"Runtime.evaluate","params":{"expression":"1"}}`)
	waitSent(t, fb, 1)
	if err := fb.Emit([]byte(`{"method":"Target.targetInfoChanged","params":{"targetInfo":{"targetId":"T-S","type":"page","url":"chrome://settings/"}}}`)); err != nil {
		t.Fatal(err)
	}
	read(t, ws)
	waitCall(t, fb, "S", "Target.detachFromTarget")
	for deadline := time.Now().Add(5 * time.Second); ; {
		write(t, ws, `{"id":7,"method":"Target.getTargets"}`)
		for wait := time.Now().Add(200 * time.Millisecond); len(fb.Sent()) < 2 && time.Now().Before(wait); {
			time.Sleep(5 * time.Millisecond)
		}
		if len(fb.Sent()) >= 2 {
			return
		}
		refusedWith(t, read(t, ws), 7, "already in flight")
		if time.Now().After(deadline) {
			t.Fatal("id 7 never came free after the failed detach")
		}
	}
}

// A target the agent holds that comes to show Chrome's own pages leaves the agent: it is told
// the session detached, the proxy detaches it, and nothing more about it reaches the agent.
func TestASessionWhoseTargetLeavesTheWebIsDetached(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	attach(t, fb, ws, "S1", "")
	attach(t, fb, ws, "S1-child", "S1")
	for _, raw := range []string{
		`{"method":"Target.targetInfoChanged","params":{"targetInfo":{"targetId":"T-S1","type":"page","url":"chrome://settings/"}}}`,
		`{"method":"Runtime.consoleAPICalled","sessionId":"S1","params":{}}`,
		`{"method":"Runtime.consoleAPICalled","sessionId":"S1-child","params":{}}`,
		// A child attached under the hidden session is hidden and detached too.
		`{"method":"Target.attachedToTarget","sessionId":"S1","params":{"sessionId":"S1-late","targetInfo":{"targetId":"T-late","type":"iframe","url":"https://a.example/"},"waitingForDebugger":false}}`,
		`{"method":"Target.detachedFromTarget","sessionId":"S1","params":{"sessionId":"S1-child"}}`,
		`{"method":"Target.targetCreated","params":{}}`,
	} {
		if err := fb.Emit([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	for range 2 {
		m := read(t, ws)
		if m["method"] != "Target.detachedFromTarget" {
			t.Fatalf("got %v, want a detach notice; the change naming Chrome's page must not pass", m)
		}
		notices = append(notices, m["params"].(map[string]any)["sessionId"].(string))
	}
	slices.Sort(notices)
	if !slices.Equal(notices, []string{"S1", "S1-child"}) {
		t.Fatalf("detach notices for %v, want the session and its child", notices)
	}
	if m := read(t, ws); m["method"] != "Target.targetCreated" {
		t.Fatalf("got %v; nothing about the detached sessions may follow", m)
	}
	for _, s := range []string{"S1", "S1-child", "S1-late"} {
		waitCall(t, fb, s, "Target.detachFromTarget")
	}
}

// A history entry is a URL to load like any other: one that is not web content is refused.
func TestAHistoryEntryMustBeLoadable(t *testing.T) {
	fb, endpoint, _ := serve(t)
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	attach(t, fb, ws, "S", "")
	fb.Answer("Page.getNavigationHistory", `{"currentIndex":1,"entries":[{"id":1,"url":"file:///etc/hosts"},{"id":2,"url":"https://a.example/"}]}`)
	write(t, ws, `{"id":1,"sessionId":"S","method":"Page.navigateToHistoryEntry","params":{"entryId":1}}`)
	refusedWith(t, read(t, ws), 1, "only http")
	write(t, ws, `{"id":2,"sessionId":"S","method":"Page.navigateToHistoryEntry","params":{"entryId":3}}`)
	refusedWith(t, read(t, ws), 2, "not known")
	write(t, ws, `{"id":3,"sessionId":"S","method":"Page.navigateToHistoryEntry","params":{"entryId":2}}`)
	waitSent(t, fb, 1)
	if sent := fb.Sent(); len(sent) != 1 || !strings.Contains(string(sent[0]), `"entryId":2`) {
		t.Fatalf("browser got %q", sent)
	}
}

// A kept alive connection that sends nothing more is closed.
func TestAnIdleConnectionIsClosed(t *testing.T) {
	_, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), IdleTimeout: 200 * time.Millisecond})
	var d net.Dialer
	raw, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(endpoint, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Write([]byte("GET /json/version HTTP/1.1\r\nHost: x\r\n" + cdpproxy.TokenHeader + ": " + token + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(raw)
	if err != nil || !strings.Contains(string(got), "200 OK") {
		t.Fatalf("an idle kept alive connection must be served and then closed: %q %v", got, err)
	}
}

// Once the calls earlier connections left unanswered fill their tracking, new calls are refused
// rather than letting a late reply through untracked.
func TestCallsAreRefusedWhileStaleTrackingIsFull(t *testing.T) {
	fb, endpoint, _ := serveWith(t, &cdpproxy.Proxy{Log: slog.New(slog.DiscardHandler), ResetWait: 20 * time.Millisecond})
	ws, _, err := dial(t, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	const n = 10_000
	for i := range n {
		write(t, ws, fmt.Sprintf(`{"id":%d,"method":"Target.getTargets"}`, i+1))
	}
	waitSent(t, fb, n)
	_ = ws.CloseNow()
	next := redial(t, endpoint)
	defer func() { _ = next.CloseNow() }()
	write(t, next, `{"id":20000,"method":"Target.getTargets"}`)
	refusedWith(t, read(t, next), 20000, "too many calls left unanswered")
}
