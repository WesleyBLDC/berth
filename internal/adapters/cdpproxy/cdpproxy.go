// Package cdpproxy serves one managed browser to one agent: a loopback HTTP and WebSocket
// endpoint that needs the browser's token in the X-Berth-Token header, and passes only the CDP
// messages cdppolicy allows to the browser's private pipe.
package cdpproxy

import (
	"context"
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
	"berth/internal/token"
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
	// maxTracked bounds the calls in flight and the sessions one connection can have tracked, the
	// calls left unanswered across connections, and the hidden sessions.
	maxTracked = 10_000
	// MaxConns bounds the TCP connections open to one endpoint, token or not. The token is read
	// only after a connection takes a slot, so headerTimeout is short: a caller without the token
	// holds a slot for at most that long.
	MaxConns      = 32
	headerTimeout = time.Second
	// DefaultIdleTimeout closes a kept alive connection that sends nothing more.
	DefaultIdleTimeout = 30 * time.Second
	// DefaultResetWait bounds how long undoing a connection's state takes; until it is undone, a
	// new WebSocket gets 409.
	DefaultResetWait = 5 * time.Second
	// resetCalls bounds the undo calls in flight at once.
	resetCalls = 32
)

// accept is websocket.Accept; a test swaps it to hold a handshake open.
var accept = websocket.Accept

// Proxy opens one endpoint per ready browser.
type Proxy struct {
	Log *slog.Logger
	// MaxQueuedBytes bounds the bytes waiting for one agent; zero means DefaultMaxQueuedBytes.
	MaxQueuedBytes int64
	// ResetWait bounds undoing a connection's state once it ends; zero means DefaultResetWait.
	ResetWait time.Duration
	// IdleTimeout closes an idle kept alive connection; zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
}

// Serve listens on 127.0.0.1 on a free port until stop is called.
func (p *Proxy) Serve(id string, conn ports.BrowserConn, tokenHash [32]byte) (string, func(), error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &endpoint{
		log: p.Log, id: id, conn: conn, tokenHash: tokenHash, addr: ln.Addr().String(), ctx: ctx,
		maxBytes: orDefault(p.MaxQueuedBytes, DefaultMaxQueuedBytes), resetWait: orDefault(p.ResetWait, DefaultResetWait),
		stale: map[int64]string{}, hides: make(chan hideJob, maxTracked), slot: make(chan struct{}, 1),
	}
	for range resetCalls {
		go e.hideWorker()
	}
	// The sink stays for the endpoint's life, so a reply or an attach that comes while no agent is
	// connected still clears or hides what it should.
	conn.Sink(e.sink)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", e.version)
	mux.HandleFunc("GET /json/version/", e.version)
	mux.HandleFunc("GET /devtools/browser/"+id, e.socket)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	srv := &http.Server{
		Handler: e.gate(mux), ReadHeaderTimeout: headerTimeout, IdleTimeout: orDefault(p.IdleTimeout, DefaultIdleTimeout),
	}
	go func() { _ = srv.Serve(newLimitListener(ln, MaxConns)) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = srv.Close()
			// Cancelled before closeActive, so a handshake that finishes after this either finds the
			// endpoint stopped or sets active before closeActive closes it.
			cancel()
			e.closeActive()
			conn.Sink(nil)
		})
	}
	return "http://" + e.addr, stop, nil
}

func orDefault[T int64 | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

type endpoint struct {
	log       *slog.Logger
	id        string
	conn      ports.BrowserConn
	tokenHash [32]byte
	addr      string
	maxBytes  int64
	resetWait time.Duration
	// ctx ends with the endpoint; the detaches of hidden sessions run on it.
	ctx context.Context

	// slot is held by the one connection allowed at a time, from before its handshake until its
	// state is undone. It is apart from mu, so a client that stalls its handshake holds up no one
	// else, the pipe's reader included.
	slot chan struct{}

	mu     sync.Mutex
	active *websocket.Conn
	// cur is the connection whose state is tracked, from its start until its state is undone.
	cur *client
	// stale maps each call an ended connection left unanswered to its session; its reply is
	// dropped, and no connection may reuse its id until the reply or the session's detach comes.
	stale map[int64]string
	// hides queues the sessions to resume and detach, for resetCalls workers.
	hides chan hideJob
}

// call is one call in flight. Calls are tracked by id alone, across sessions: Chrome answers a
// call on a session it no longer has without the session id, so only an id unique among the
// calls in flight names its call. Playwright and Puppeteer number calls once per connection.
type call struct {
	session, method string
}

type hideJob struct {
	session, parent string
	waiting         bool
}

type session struct {
	parent, target string
}

// client is what one connection turned on in the browser, which every connection shares through
// the one pipe, so its end can undo it: its calls in flight by method, the sessions it was handed
// (with the session each was attached from and its target), the targets' sessions, those waiting
// for the debugger, and the browser level Fetch, auto attach and discovery it enabled. push
// delivers to the connection until it ends. Guarded by the endpoint's mu.
type client struct {
	inFlight   map[int64]call
	sessions   map[string]session
	targets    map[string]map[string]bool
	waiting    map[string]bool
	fetch      bool
	autoAttach bool
	discover   bool
	push       func([]byte)
	drop       func()
}

// gate checks the token before anything else, so nothing about the browser answers without it,
// and refuses any request a web page could have made.
func (e *endpoint) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !token.Matches(r.Header.Get(TokenHeader), e.tokenHash) {
			// A caller without the token keeps no connection open.
			w.Header().Set("Connection", "close")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Connection", "close")
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
	select {
	case e.slot <- struct{}{}:
	default:
		http.Error(w, "another CDP connection is open", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
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
	// The connection is tracked from before its handshake, so what the browser sends while the
	// handshake runs is queued for it, and its state is undone even when the handshake fails.
	e.mu.Lock()
	e.cur = &client{
		inFlight: map[int64]call{}, sessions: map[string]session{}, targets: map[string]map[string]bool{},
		waiting: map[string]bool{}, push: push, drop: drop,
	}
	e.mu.Unlock()
	ws, err := accept(w, r, nil)
	// The reset runs after the request ends, so it must not end with it.
	defer e.release(context.WithoutCancel(r.Context()), ws)
	defer cancel()
	if err != nil {
		return
	}
	e.mu.Lock()
	if e.ctx.Err() != nil {
		// Stopped during the handshake: the hijacked connection must not outlive the endpoint.
		e.mu.Unlock()
		return
	}
	e.active = ws
	e.mu.Unlock()
	ws.SetReadLimit(maxMessage)
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
		if errors.Is(err, cdppolicy.ErrText) {
			refused++
			push(refusal(m, "its text holds invalid UTF-8 or a lone surrogate, which Chrome may read differently"))
			continue
		}
		if err != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "malformed CDP message")
			return
		}
		if cdppolicy.Answers(m, e.conn.OwnSession) {
			answered++
			push(emptyResult(m))
			continue
		}
		why := cdppolicy.Check(m, e.conn.OwnSession)
		if why == "" {
			why = e.lookUp(ctx, m)
		}
		if why == "" {
			why = e.toBrowser(m)
		}
		if why != "" {
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
func (e *endpoint) release(ctx context.Context, ws *websocket.Conn) {
	if ws != nil {
		_ = ws.CloseNow()
	}
	e.mu.Lock()
	c := e.cur
	c.push = nil
	e.mu.Unlock()
	e.reset(ctx, c)
	e.mu.Lock()
	e.cur = nil
	e.active = nil
	e.mu.Unlock()
	<-e.slot
}

// toBrowser records a message about to be sent, or says why it may not be: a session the
// connection does not hold (never attached, detached, or hidden from it), an id already in
// flight, one an ended connection left unanswered, or too many calls to track.
func (e *endpoint) toBrowser(m cdppolicy.Message) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cur
	_, held := c.sessions[m.SessionID]
	_, inFlight := c.inFlight[m.ID]
	_, stale := e.stale[m.ID]
	switch {
	case m.SessionID != "" && !held:
		return "the session is not attached"
	case inFlight:
		return "the id is already in flight"
	case stale:
		return "the id is still in flight from the previous connection"
	case len(e.stale) >= maxTracked:
		return "too many calls left unanswered by earlier connections"
	case len(c.inFlight) >= maxTracked:
		return "too many calls in flight"
	}
	c.inFlight[m.ID] = call{session: m.SessionID, method: m.Method}
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
	case m.Method == "Target.setDiscoverTargets":
		var p struct {
			Discover bool `json:"discover"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.discover = p.Discover
	}
	return ""
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// browserMessage is what the proxy reads of a message from the browser.
type browserMessage struct {
	ID        *int64 `json:"id"`
	Method    string `json:"method"`
	SessionID string `json:"sessionId"`
	Params    struct {
		SessionID          string     `json:"sessionId"`
		WaitingForDebugger bool       `json:"waitingForDebugger"`
		TargetInfo         targetInfo `json:"targetInfo"`
	} `json:"params"`
}

// sink takes every message from the browser for the endpoint's whole life.
func (e *endpoint) sink(raw []byte) {
	out, push := e.fromBrowser(raw)
	if push == nil {
		return
	}
	for _, msg := range out {
		push(msg)
	}
}

// fromBrowser tracks the sessions and replies of a message from the browser and returns what may
// reach the connection, with the connection's push, nil when none is connected. Only messages of
// the browser session or of a session the connection holds reach it; a reply still clears its
// call whatever its session, and a reply to a call an ended connection made never reaches one.
func (e *endpoint) fromBrowser(raw []byte) ([][]byte, func([]byte)) {
	var m browserMessage
	// Chrome's output is well formed; whatever a type mismatch leaves unread is not tracked.
	_ = json.Unmarshal(raw, &m)
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cur
	var push func([]byte)
	if c != nil {
		push = c.push
	}
	visible := m.SessionID == "" || (c != nil && c.holds(m.SessionID))
	shown := func(out ...[]byte) ([][]byte, func([]byte)) {
		if !visible {
			return nil, nil
		}
		return out, push
	}
	if m.ID != nil {
		if c != nil && c.answered(*m.ID) {
			return shown(raw)
		}
		if _, stale := e.stale[*m.ID]; stale {
			delete(e.stale, *m.ID)
			return nil, nil
		}
		return shown(raw)
	}
	info := m.Params.TargetInfo
	switch m.Method {
	case "Target.attachedToTarget":
		s, waiting := m.Params.SessionID, m.Params.WaitingForDebugger
		// A session no live connection asked for, one under a session it does not hold, or one to
		// a target that is not web content is hidden and detached.
		if c == nil || push == nil || !visible || !cdppolicy.WebTarget(info.Type, info.URL) {
			e.hide(s, m.SessionID, waiting)
			return nil, nil
		}
		if len(c.sessions) >= maxTracked {
			c.drop()
			e.hide(s, m.SessionID, waiting)
			return nil, nil
		}
		c.attached(s, session{parent: m.SessionID, target: info.TargetID}, waiting)
	case "Target.detachedFromTarget":
		s := m.Params.SessionID
		e.forgetCalls(s)
		if c == nil || !c.holds(s) {
			return nil, nil
		}
		c.forget(s)
	case "Target.targetInfoChanged":
		if cdppolicy.WebTarget(info.Type, info.URL) {
			break
		}
		// A target the agent holds that now shows Chrome's own pages leaves the agent: it is told
		// its sessions, and theirs, detached, and the proxy detaches them. The change itself, which
		// names the page, is not passed on.
		var out [][]byte
		if c != nil {
			for s := range c.targets[info.TargetID] {
				out = append(out, e.hideTree(c, s)...)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, push
	}
	return shown(raw)
}

// hideTree takes a session and every session attached under it from the connection, queues them
// to be detached, and returns the detach notices the connection gets for them.
func (e *endpoint) hideTree(c *client, s string) [][]byte {
	held, ok := c.sessions[s]
	if !ok {
		return nil
	}
	var out [][]byte
	for child, cs := range c.sessions {
		if cs.parent == s {
			out = append(out, e.hideTree(c, child)...)
		}
	}
	waiting := c.waiting[s]
	c.forget(s)
	e.hide(s, held.parent, waiting)
	return append(out, detachedEvent(s, held.parent, held.target))
}

func (c *client) holds(s string) bool {
	_, ok := c.sessions[s]
	return ok
}

// answered clears a reply's call and reports whether it was this connection's.
func (c *client) answered(id int64) bool {
	if _, mine := c.inFlight[id]; mine {
		delete(c.inFlight, id)
		return true
	}
	return false
}

// forgetCalls drops the calls in flight and the stale calls on a session that is gone, since
// Chrome answers none of them once it is.
func (e *endpoint) forgetCalls(s string) {
	if s == "" {
		return
	}
	if e.cur != nil {
		for id, cl := range e.cur.inFlight {
			if cl.session == s {
				delete(e.cur.inFlight, id)
			}
		}
	}
	for id, session := range e.stale {
		if session == s {
			delete(e.stale, id)
		}
	}
}

func detachedEvent(sessionID, parent, targetID string) []byte {
	msg := map[string]any{"method": "Target.detachedFromTarget", "params": map[string]string{"sessionId": sessionID, "targetId": targetID}}
	if parent != "" {
		msg["sessionId"] = parent
	}
	b, _ := json.Marshal(msg)
	return b
}

// lookUp asks the browser what a message would act on, for the checks the policy cannot make
// from the message alone: an attach only to web content, never the browser target, whose session
// is Berth's, nor Chrome's own pages; and a history entry only to a loadable URL.
func (e *endpoint) lookUp(ctx context.Context, m cdppolicy.Message) string {
	ctx, cancel := context.WithTimeout(ctx, e.resetWait)
	defer cancel()
	switch m.Method {
	case "Target.attachToTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		params, _ := json.Marshal(p)
		raw, err := e.conn.Call(ctx, "", "Target.getTargetInfo", params)
		if err != nil {
			return "the target is not known"
		}
		var info struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(raw, &info) != nil || !cdppolicy.WebTarget(info.TargetInfo.Type, info.TargetInfo.URL) {
			return "only web content can be attached to"
		}
	case "Page.navigateToHistoryEntry":
		var p struct {
			EntryID int64 `json:"entryId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		raw, err := e.conn.Call(ctx, m.SessionID, "Page.getNavigationHistory", nil)
		if err != nil {
			return "the history is not known"
		}
		var history struct {
			Entries []struct {
				ID  int64  `json:"id"`
				URL string `json:"url"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(raw, &history)
		for _, entry := range history.Entries {
			if entry.ID == p.EntryID {
				if !cdppolicy.Loadable(entry.URL) {
					return "only http, https, data, blob and about:blank URLs load"
				}
				return ""
			}
		}
		return "the history entry is not known"
	}
	return ""
}

// hide queues a session to be resumed if it waits for the debugger and detached, by the workers,
// since the calls need the pipe's reader this runs on. No connection holds it, so nothing about
// it reaches one meanwhile. A full queue leaves the session attached but still unseen.
func (e *endpoint) hide(sessionID, parent string, waiting bool) {
	if sessionID == "" {
		return
	}
	select {
	case e.hides <- hideJob{session: sessionID, parent: parent, waiting: waiting}:
	default:
		e.log.Warn("too many hidden CDP sessions queued to detach", "browser", e.id)
	}
}

// hideWorker detaches queued sessions until the endpoint ends. A detach Chrome refuses means the
// session is gone, so its calls are forgotten.
func (e *endpoint) hideWorker() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case j := <-e.hides:
			ctx, cancel := context.WithTimeout(e.ctx, e.resetWait)
			err := e.undoSession(ctx, j.session, j.parent, j.waiting)
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				e.mu.Lock()
				e.forgetCalls(j.session)
				e.mu.Unlock()
			}
		}
	}
}

// undoSession resumes a session's target if it waits for the debugger, then detaches it, and
// returns the detach's error.
func (e *endpoint) undoSession(ctx context.Context, sessionID, parent string, waiting bool) error {
	// A failed resume leaves nothing better to do than the detach.
	if waiting {
		_, _ = e.conn.Call(ctx, sessionID, "Runtime.runIfWaitingForDebugger", nil)
	}
	p, _ := json.Marshal(map[string]string{"sessionId": sessionID})
	_, err := e.conn.Call(ctx, parent, "Target.detachFromTarget", p)
	return err
}

func (c *client) attached(sessionID string, s session, waiting bool) {
	if sessionID == "" {
		return
	}
	c.sessions[sessionID] = s
	if c.targets[s.target] == nil {
		c.targets[s.target] = map[string]bool{}
	}
	c.targets[s.target][sessionID] = true
	if waiting {
		c.waiting[sessionID] = true
	}
}

// forget drops a session from what the connection holds; its calls in flight stay until Chrome
// answers them or reports the session detached.
func (c *client) forget(sessionID string) {
	s, ok := c.sessions[sessionID]
	if !ok {
		return
	}
	delete(c.sessions, sessionID)
	delete(c.waiting, sessionID)
	if delete(c.targets[s.target], sessionID); len(c.targets[s.target]) == 0 {
		delete(c.targets, s.target)
	}
}

// reset undoes what an ended connection turned on. Browser level auto attach, discovery and
// Fetch go off first, so no new session appears; then every target it holds that waits for the
// debugger is resumed and every session detached, which takes its own state with it, many at
// once within the deadline. It waits for its calls in flight, detaches any session an attach
// still in flight opened, and the calls unanswered by the deadline become stale, so their
// replies never reach the next connection.
func (e *endpoint) reset(ctx context.Context, c *client) {
	ctx, cancel := context.WithTimeout(ctx, e.resetWait)
	defer cancel()
	e.mu.Lock()
	fetch, autoAttach, discover := c.fetch, c.autoAttach, c.discover
	e.mu.Unlock()
	call := func(method string, params []byte) { _, _ = e.conn.Call(ctx, "", method, params) }
	if autoAttach {
		call("Target.setAutoAttach", []byte(`{"autoAttach":false,"waitForDebuggerOnStart":false,"flatten":true}`))
	}
	if discover {
		call("Target.setDiscoverTargets", []byte(`{"discover":false}`))
	}
	if fetch {
		call("Fetch.disable", nil)
	}
	e.undoSessions(ctx, c)
	if !e.answered(ctx, c) {
		e.mu.Lock()
		for id, cl := range c.inFlight {
			if len(e.stale) >= maxTracked {
				e.log.Warn("too many CDP calls left in flight to track", "browser", e.id)
				break
			}
			e.stale[id] = cl.session
		}
		e.mu.Unlock()
	}
	e.undoSessions(ctx, c)
}

// undoSessions resumes and detaches every session the connection holds now, resetCalls at once.
func (e *endpoint) undoSessions(ctx context.Context, c *client) {
	e.mu.Lock()
	sessions := maps.Clone(c.sessions)
	waiting := maps.Clone(c.waiting)
	for s := range sessions {
		c.forget(s)
	}
	e.mu.Unlock()
	slots := make(chan struct{}, resetCalls)
	var wg sync.WaitGroup
	for id, s := range sessions {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			_ = e.undoSession(ctx, id, s.parent, waiting[id])
		})
	}
	wg.Wait()
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

// limitListener lets at most cap(slots) connections be open at once; Accept waits for a free
// slot, so more connections wait in the kernel's queue instead of holding berthd's descriptors.
type limitListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newLimitListener(ln net.Listener, n int) *limitListener {
	return &limitListener{Listener: ln, slots: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
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
