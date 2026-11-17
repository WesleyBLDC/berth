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
	"maps"
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
	// maxTracked bounds the calls in flight and the sessions one connection can have tracked, and
	// the calls left in flight across connections.
	maxTracked = 10_000
	// DefaultResetWait bounds how long undoing a connection's state takes, and so how long the
	// next connection waits.
	DefaultResetWait = 5 * time.Second
)

// Proxy opens one endpoint per ready browser.
type Proxy struct {
	Log *slog.Logger
	// MaxQueuedBytes bounds the bytes waiting for one agent; zero means DefaultMaxQueuedBytes.
	MaxQueuedBytes int64
	// ResetWait bounds undoing a connection's state once it ends; zero means DefaultResetWait.
	ResetWait time.Duration
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
	resetWait := p.ResetWait
	if resetWait <= 0 {
		resetWait = DefaultResetWait
	}
	e := &endpoint{
		log: p.Log, id: id, conn: conn, tokenHash: tokenHash, addr: ln.Addr().String(), maxBytes: maxBytes,
		resetWait: resetWait, stale: map[callKey]bool{},
	}
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
	resetWait time.Duration

	mu     sync.Mutex
	active *websocket.Conn
	// stale holds the calls an ended connection left in flight; their replies are dropped, and
	// the next connection may not reuse their ids until they come.
	stale map[callKey]bool
}

type callKey struct {
	session string
	id      int64
}

// client is what one connection turned on in the browser, which every connection shares through
// the one pipe, so its end can undo it: its calls in flight by method, the sessions it was handed
// with the session each was attached from, those waiting for the debugger, and the browser level
// Fetch and auto attach it enabled. Guarded by the endpoint's mu.
type client struct {
	inFlight   map[callKey]string
	sessions   map[string]string
	waiting    map[string]bool
	fetch      bool
	autoAttach bool
	drop       func()
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
	ws.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	c := &client{inFlight: map[callKey]string{}, sessions: map[string]string{}, waiting: map[string]bool{}}
	// The reset runs after the request ends, so it must not end with it.
	defer e.release(context.WithoutCancel(r.Context()), ws, c)
	defer cancel()

	out := make(chan []byte, queued)
	var dropOnce sync.Once
	drop := func() { dropOnce.Do(cancel) }
	c.drop = drop
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
	e.conn.Sink(func(raw []byte) {
		if e.fromBrowser(c, raw) && ctx.Err() == nil {
			push(raw)
		}
	})
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

	var allowed, refused, answered int
	defer func() {
		e.log.Info("CDP connection ended", "browser", e.id, "allowed", allowed, "refused", refused, "answered", answered)
	}()
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
		if cdppolicy.Answers(m, e.conn.OwnSession) {
			answered++
			push(emptyResult(m))
			continue
		}
		if why := cdppolicy.Check(m, e.conn.OwnSession); why != "" {
			refused++
			push(refusal(m, why))
			continue
		}
		if why := e.toBrowser(c, m); why != "" {
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

// release ends a connection and frees its slot only once its state is undone, so nothing it
// left reaches the next connection.
func (e *endpoint) release(ctx context.Context, ws *websocket.Conn, c *client) {
	_ = ws.CloseNow()
	e.reset(ctx, c)
	e.conn.Sink(nil)
	e.mu.Lock()
	e.active = nil
	e.mu.Unlock()
}

// toBrowser records a message about to be sent, or says why it may not be: an id an ended
// connection left in flight, or too many calls in flight.
func (e *endpoint) toBrowser(c *client, m cdppolicy.Message) string {
	key := callKey{m.SessionID, m.ID}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stale[key] {
		return "the id is still in flight from the previous connection"
	}
	if len(c.inFlight) >= maxTracked {
		return "too many calls in flight"
	}
	c.inFlight[key] = m.Method
	switch {
	case m.Method == "Runtime.runIfWaitingForDebugger":
		delete(c.waiting, m.SessionID)
	case m.SessionID != "":
		// A session's own state goes with the session, which the reset detaches.
	case m.Method == "Fetch.enable":
		c.fetch = true
	case m.Method == "Fetch.disable":
		c.fetch = false
	case m.Method == "Target.setAutoAttach":
		var p struct {
			AutoAttach bool `json:"autoAttach"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.autoAttach = p.AutoAttach
	}
	return ""
}

// browserMessage is what the proxy reads of a message from the browser.
type browserMessage struct {
	ID        *int64 `json:"id"`
	Method    string `json:"method"`
	SessionID string `json:"sessionId"`
	Params    struct {
		SessionID          string `json:"sessionId"`
		WaitingForDebugger bool   `json:"waitingForDebugger"`
	} `json:"params"`
	Result struct {
		SessionID string `json:"sessionId"`
	} `json:"result"`
}

// fromBrowser tracks the sessions and replies of a message from the browser and reports whether
// it may reach the connection: a reply to a call an ended connection made never does.
func (e *endpoint) fromBrowser(c *client, raw []byte) bool {
	var m browserMessage
	// Chrome's output is well formed; whatever a type mismatch leaves unread is not tracked.
	_ = json.Unmarshal(raw, &m)
	e.mu.Lock()
	defer e.mu.Unlock()
	if m.ID != nil {
		key := callKey{m.SessionID, *m.ID}
		method, mine := c.inFlight[key]
		if !mine {
			if e.stale[key] {
				delete(e.stale, key)
				return false
			}
			return true
		}
		delete(c.inFlight, key)
		if method == "Target.attachToTarget" && m.Result.SessionID != "" {
			c.attached(m.Result.SessionID, m.SessionID, false)
		}
		return true
	}
	switch m.Method {
	case "Target.attachedToTarget":
		c.attached(m.Params.SessionID, m.SessionID, m.Params.WaitingForDebugger)
	case "Target.detachedFromTarget":
		delete(c.sessions, m.Params.SessionID)
		delete(c.waiting, m.Params.SessionID)
		// A detached session's calls are never answered.
		for key := range c.inFlight {
			if key.session == m.Params.SessionID {
				delete(c.inFlight, key)
			}
		}
	}
	return true
}

// attached records a session the connection was handed; past the limit the connection is
// dropped, since a session it could not undo would outlive it.
func (c *client) attached(session, parent string, waiting bool) {
	if session == "" {
		return
	}
	if len(c.sessions) >= maxTracked {
		c.drop()
		return
	}
	c.sessions[session] = parent
	if waiting {
		c.waiting[session] = true
	}
}

// reset undoes what an ended connection turned on: it resumes the targets waiting for the
// debugger, detaches its sessions, which takes their own state with them, turns off the browser
// level Fetch and auto attach it enabled, and waits for its calls in flight; the ones still
// unanswered by the deadline become stale, so their replies never reach the next connection.
func (e *endpoint) reset(ctx context.Context, c *client) {
	ctx, cancel := context.WithTimeout(ctx, e.resetWait)
	defer cancel()
	e.mu.Lock()
	waiting, sessions, fetch, autoAttach := maps.Clone(c.waiting), maps.Clone(c.sessions), c.fetch, c.autoAttach
	e.mu.Unlock()
	call := func(session, method string, params []byte) {
		// A failed undo leaves nothing better to do; the browser may already be gone.
		_, _ = e.conn.Call(ctx, session, method, params)
	}
	for session := range waiting {
		call(session, "Runtime.runIfWaitingForDebugger", nil)
	}
	for session, parent := range sessions {
		p, _ := json.Marshal(map[string]string{"sessionId": session})
		call(parent, "Target.detachFromTarget", p)
	}
	if fetch {
		call("", "Fetch.disable", nil)
	}
	if autoAttach {
		call("", "Target.setAutoAttach", []byte(`{"autoAttach":false,"waitForDebuggerOnStart":false,"flatten":true}`))
	}
	if e.answered(ctx, c) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range c.inFlight {
		if len(e.stale) >= maxTracked {
			e.log.Warn("too many CDP calls left in flight to track", "browser", e.id)
			return
		}
		e.stale[key] = true
	}
}

// answered waits until the connection's calls in flight are answered, ctx ends or the browser
// is gone, and reports whether nothing is left to track.
func (e *endpoint) answered(ctx context.Context, c *client) bool {
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		e.mu.Lock()
		left := len(c.inFlight)
		e.mu.Unlock()
		if left == 0 {
			return true
		}
		select {
		case <-poll.C:
		case <-e.conn.Done():
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// emptyResult is the success reply for a method the proxy answers itself.
func emptyResult(m cdppolicy.Message) []byte {
	reply := map[string]any{"id": m.ID, "result": map[string]any{}}
	if m.SessionID != "" {
		reply["sessionId"] = m.SessionID
	}
	b, _ := json.Marshal(reply)
	return b
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
