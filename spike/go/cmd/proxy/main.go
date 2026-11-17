// proxy launches Chrome over a pipe and exposes it to one agent through a token gated WebSocket.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"

	"berth/spike/go/chrome"

	"github.com/coder/websocket"
)

// refused lists methods an agent may never call: they reach beyond its own session.
var refused = map[string]bool{
	"Browser.setDownloadBehavior": true,
	"Browser.crash":               true,
	"Storage.getCookies":          true,
	"SystemInfo.getProcessInfo":   true,
}

type proxy struct {
	browser   *chrome.Browser
	tokenHash [32]byte
	addr      string

	mu     sync.Mutex
	client *websocket.Conn
}

func (p *proxy) authorized(r *http.Request) bool {
	got := sha256.Sum256([]byte(r.Header.Get("X-Berth-Token")))
	return subtle.ConstantTimeCompare(got[:], p.tokenHash[:]) == 1
}

func (p *proxy) version(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Browser":              "Berth spike",
		"webSocketDebuggerUrl": "ws://" + p.addr + "/devtools/browser/berth",
	})
}

func (p *proxy) ws(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		slog.Warn("refused connection without a valid token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	p.mu.Lock()
	p.client = conn
	p.mu.Unlock()
	slog.Info("agent connected")
	ctx := r.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			slog.Info("agent disconnected", "err", err)
			return
		}
		var m chrome.Message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		slog.Info("agent cdp", "method", m.Method)
		if refused[m.Method] {
			reply, _ := json.Marshal(map[string]any{"id": m.ID, "sessionId": m.SessionID, "error": map[string]any{"code": -32000, "message": "refused by Berth policy: " + m.Method}})
			_ = conn.Write(ctx, websocket.MessageText, reply)
			continue
		}
		if err := p.browser.Write(data); err != nil {
			return
		}
	}
}

func (p *proxy) pump(ctx context.Context) {
	for raw := range p.browser.Incoming {
		p.mu.Lock()
		c := p.client
		p.mu.Unlock()
		if c != nil {
			_ = c.Write(ctx, websocket.MessageText, raw)
		}
	}
}

const testPage = `<!doctype html><title>Berth spike page</title>
<h1 id="status">Waiting</h1>
<button onclick="document.getElementById('status').textContent='Clicked through Berth'">Press me</button>`

func main() {
	headless := flag.Bool("headless", true, "run Chrome headless")
	tokenFile := flag.String("token-file", "", "write the session token here (mode 0600)")
	flag.Parse()

	dir, err := os.MkdirTemp("", "berth-spike-profile-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	b, err := chrome.Launch(ctx, chrome.Options{UserDataDir: dir, Headless: *headless})
	if err != nil {
		panic(err)
	}
	defer b.Close()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	p := &proxy{browser: b, tokenHash: sha256.Sum256([]byte(token)), addr: ln.Addr().String()}
	go p.pump(ctx)

	pageLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go http.Serve(pageLn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testPage) }))

	if *tokenFile != "" {
		if err := os.WriteFile(*tokenFile, []byte(token), 0o600); err != nil {
			panic(err)
		}
	}
	fmt.Printf("CDP_ENDPOINT=http://%s\nTEST_PAGE=http://%s/\nCHROME_PID=%d\n", p.addr, pageLn.Addr(), b.PID())

	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", p.version)
	mux.HandleFunc("/json/version/", p.version)
	mux.HandleFunc("/devtools/browser/berth", p.ws)
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	_ = srv.Serve(ln)
}
