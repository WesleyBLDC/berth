// Package cdpproxy serves one managed browser to one agent: a loopback HTTP and WebSocket
// endpoint that needs the browser's token in the X-Berth-Token header, and passes only the CDP
// messages cdppolicy allows to the browser's private pipe.
package cdpproxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"berth/internal/core/cdppolicy"
	"berth/internal/core/ports"
)

const (
	// TokenHeader carries the browser's token. A header, never the URL: a web page can open a
	// WebSocket to loopback but cannot set a header, and URLs end up in logs.
	TokenHeader = "X-Berth-Token" //nolint:gosec // a header name, not a credential
	maxMessage  = 64 << 20
	// queued and DefaultMaxQueuedBytes bound what waits for a slow agent, by count and by size;
	// past either the agent is dropped, so the pipe's reader, which Berth's own calls share, never
	// waits on an agent and berthd never buffers without limit.
	queued                = 1024
	DefaultMaxQueuedBytes = 128 << 20
)

// Proxy opens one endpoint per ready browser.
type Proxy struct {
	Log *slog.Logger
	// MaxQueuedBytes bounds the bytes waiting for one agent; zero means DefaultMaxQueuedBytes.
	MaxQueuedBytes int64
}

// Serve listens on 127.0.0.1 on a free port until stop is called.
func (p *Proxy) Serve(id string, conn ports.BrowserConn, tokenHash [32]byte) (string, func(), error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	maxBytes := p.MaxQueuedBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxQueuedBytes
	}
	e := &endpoint{log: p.Log, id: id, conn: conn, tokenHash: tokenHash, addr: ln.Addr().String(), maxBytes: maxBytes}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", e.version)
	mux.HandleFunc("GET /json/version/", e.version)
	mux.HandleFunc("GET /devtools/browser/"+id, e.socket)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	srv := &http.Server{Handler: e.gate(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = srv.Close()
			e.closeActive()
		})
	}
	return "http://" + e.addr, stop, nil
}

type endpoint struct {
	log       *slog.Logger
	id        string
	conn      ports.BrowserConn
	tokenHash [32]byte
	addr      string
	maxBytes  int64

	mu     sync.Mutex
	active *websocket.Conn
}

// gate checks the token before anything else, so nothing about the browser answers without it,
// and refuses any request a web page could have made.
func (e *endpoint) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get(TokenHeader)))
		if subtle.ConstantTimeCompare(got[:], e.tokenHash[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "requests from web pages are refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (e *endpoint) version(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Browser":              "Berth managed browser",
		"Protocol-Version":     "1.3",
		"webSocketDebuggerUrl": "ws://" + e.addr + "/devtools/browser/" + e.id,
	})
}

func (e *endpoint) closeActive() {
	e.mu.Lock()
	c := e.active
	e.mu.Unlock()
	if c != nil {
		_ = c.CloseNow()
	}
}

func (e *endpoint) socket(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	if e.active != nil {
		e.mu.Unlock()
		http.Error(w, "another CDP connection is open", http.StatusConflict)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		e.mu.Unlock()
		return
	}
	e.active = ws
	e.mu.Unlock()
	defer func() {
		e.conn.Sink(nil)
		_ = ws.CloseNow()
		e.mu.Lock()
		e.active = nil
		e.mu.Unlock()
	}()
	ws.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out := make(chan []byte, queued)
	var dropOnce sync.Once
	drop := func() { dropOnce.Do(cancel) }
	var queuedBytes atomic.Int64
	push := func(raw []byte) {
		if queuedBytes.Add(int64(len(raw))) > e.maxBytes {
			drop()
			return
		}
		select {
		case out <- raw:
		default:
			drop()
		}
	}
	e.conn.Sink(push)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-out:
				err := ws.Write(ctx, websocket.MessageText, raw)
				queuedBytes.Add(-int64(len(raw)))
				if err != nil {
					drop()
					return
				}
			}
		}
	}()
	go func() {
		select {
		case <-e.conn.Done():
			drop()
		case <-ctx.Done():
		}
	}()

	var allowed, refused int
	defer func() { e.log.Info("CDP connection ended", "browser", e.id, "allowed", allowed, "refused", refused) }()
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		m, err := cdppolicy.Parse(raw)
		if err != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "malformed CDP message")
			return
		}
		if why := cdppolicy.Check(m, e.conn.OwnSession); why != "" {
			refused++
			push(refusal(m, why))
			continue
		}
		allowed++
		if err := e.conn.Send(raw); err != nil {
			if !errors.Is(err, ports.ErrBrowserGone) {
				e.log.Warn("could not write to the browser pipe", "browser", e.id, "err", err)
			}
			return
		}
	}
}

// refusal is the CDP error reply for a refused message, so the client's call ends instead of
// waiting forever.
func refusal(m cdppolicy.Message, why string) []byte {
	reply := map[string]any{
		"id":    m.ID,
		"error": map[string]any{"code": -32000, "message": "refused by Berth: " + m.Method + ": " + why},
	}
	if m.SessionID != "" {
		reply["sessionId"] = m.SessionID
	}
	b, _ := json.Marshal(reply)
	return b
}
