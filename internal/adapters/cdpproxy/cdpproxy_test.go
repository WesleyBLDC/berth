package cdpproxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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
	for range sent {
		if err := fb.Emit(big); err != nil {
			t.Fatal(err)
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
