package berthd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
	"berth/internal/token"
)

const (
	// endGrace is how long a dropped watch stream waits for the EndProxySession a proxy sends
	// as it exits, before the session counts as Lost.
	endGrace = 2 * time.Second
	// sessionStopWait is how long a daemon stop gives proxies to end their sessions.
	sessionStopWait = 3 * time.Second
)

// proxySession is a running berth-proxy as berthd knows it. The session's state lives in its
// machine; this holds only what the shell needs to reach the proxy and tell it apart.
type proxySession struct {
	id  string
	pid int
	// token is the audit token of the process that authorized the session; only it may watch
	// or end the session.
	token  [32]byte
	stop   chan proxysession.StopReason
	done   chan struct{}
	cancel context.CancelFunc

	// watching is a fact about connections, not the session: a watch stream was opened, so a
	// second one is refused. Whether the session is ready is its machine's state.
	mu       sync.Mutex
	watching bool
	once     sync.Once
}

func (p *proxySession) finished() {
	p.once.Do(func() {
		close(p.done)
		p.cancel()
	})
}

var reasons = map[proxysession.StopReason]berthv1.ProxyStopReason{
	proxysession.StopGrantRevoked:      berthv1.ProxyStopReason_PROXY_STOP_REASON_GRANT_REVOKED,
	proxysession.StopSecretRemoved:     berthv1.ProxyStopReason_PROXY_STOP_REASON_SECRET_REMOVED,
	proxysession.StopDaemonStopping:    berthv1.ProxyStopReason_PROXY_STOP_REASON_DAEMON_STOPPING,
	proxysession.StopAgentSessionEnded: berthv1.ProxyStopReason_PROXY_STOP_REASON_AGENT_SESSION_ENDED,
}

func (s *secrets) runSession(entity string) machine.Runner[proxysession.Event, proxysession.Effect] {
	id := entityID(entity, proxysession.Entity)
	return func(_ context.Context, effect proxysession.Effect) ([]proxysession.Event, error) {
		if effect.Kind != proxysession.SendStop {
			return nil, fmt.Errorf("unknown proxy session effect %q", effect.Kind)
		}
		// The watch stream delivers it, or WatchProxySession finds it when the proxy opens one.
		if ps := s.session(id); ps != nil {
			select {
			case ps.stop <- effect.Reason:
			default:
			}
		}
		return nil, nil
	}
}

// TrustProxy accepts only a peer running the signed berth-proxy beside berthd, judged by the
// kernel's live code identity.
func (s *secrets) TrustProxy(ctx context.Context, peer ports.Peer) error {
	if !s.Proxy.Runs(ctx, peer) {
		return api.ErrPeerNotTrusted
	}
	return nil
}

func (s *secrets) AuthorizeProxy(ctx context.Context, req *berthv1.AuthorizeProxyRequest, peer ports.Peer) (*berthv1.AuthorizeProxyResponse, error) {
	noGrant := &berthv1.AuthorizeProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_NO_GRANT}
	grants := make([]ports.GrantRecord, 0, len(req.GetNames()))
	for _, name := range req.GetNames() {
		g, ok, err := s.activeProxyGrant(ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return noGrant, nil
		}
		grants = append(grants, g)
	}
	ps, err := s.openSession(ctx, peer, grants)
	if err != nil {
		return nil, err
	}
	resp := &berthv1.AuthorizeProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, SessionId: ps.id}
	for _, g := range grants {
		got, err := s.issueFor(ctx, useRequest{grantID: g.ID, name: g.Secret, pid: peer.PID, kind: ports.ConsumerProxy})
		if err != nil {
			s.lose(ps, "a grant changed while its tickets were issued")
			if errors.Is(err, machine.ErrIllegal) {
				return noGrant, nil
			}
			return nil, err
		}
		resp.Tickets = append(resp.Tickets, &berthv1.ProxyTicket{Name: g.Secret, Ticket: got.ticket[:], Target: proxyTarget(g.Proxy)})
		resp.ExpiresAt = timestamppb.New(got.expires)
	}
	return resp, nil
}

// activeProxyGrant returns the newest Active proxy grant of a secret that serves uses; when there
// is none it records the refusal on the secret.
func (s *secrets) activeProxyGrant(ctx context.Context, name string) (ports.GrantRecord, bool, error) {
	usable, err := s.usable(ctx, name)
	if err != nil || !usable {
		return ports.GrantRecord{}, false, err
	}
	grants, err := s.Records.Grants(ctx, name)
	if err != nil {
		return ports.GrantRecord{}, false, err
	}
	for _, g := range grants {
		if g.Kind != ports.ConsumerProxy {
			continue
		}
		st, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
		if err != nil {
			return ports.GrantRecord{}, false, err
		}
		if grant.State(st) == grant.Active {
			return g, true, nil
		}
	}
	_, _, err = step(ctx, s.secrets, secret.Entity(name), secret.Event{Kind: secret.UseRefused, Note: "no active proxy grant"}, nil, secret.UseRefused)
	return ports.GrantRecord{}, false, err
}

// openSession records the session row before any ticket exists, so a revoke that lands while
// tickets are issued already finds the session to stop. The proxy's exit is watched from here on.
func (s *secrets) openSession(ctx context.Context, peer ports.Peer, grants []ports.GrantRecord) (*proxySession, error) {
	id, err := token.Hex(16)
	if err != nil {
		return nil, err
	}
	// The watch outlives this call, so it hangs off berthd's context, not the caller's.
	sessCtx, cancel := context.WithCancel(s.ctx)
	exited, err := s.Exits(sessCtx, peer.PID) //nolint:contextcheck // see above
	if err != nil {
		cancel()
		s.Log.Warn("cannot watch the proxy process", "pid", peer.PID, "err", err)
		return nil, api.ErrCallerUnknown
	}
	// The pid could have been reused between the call and the watch; its audit token cannot.
	if !s.Proxy.Runs(ctx, peer) {
		cancel()
		return nil, api.ErrPeerNotTrusted
	}
	agent := api.AgentSession(ctx)
	if agent == "" {
		// A proxy berthd started for an agent session calls on the owner socket, as every proxy does.
		agent = s.startedFor(peer.PID)
	}
	rec := ports.ProxySessionRecord{ID: id, PID: peer.PID, CreatedAt: s.Clock.Now(), AgentSession: agent}
	for _, g := range grants {
		rec.Grants = append(rec.Grants, g.ID)
	}
	if err := s.Sessions.AddProxySession(ctx, rec); err != nil {
		cancel()
		return nil, err
	}
	// The agent session's end stops the proxy sessions it finds recorded, so one that ended
	// before this row existed is caught here: either it sees the row or this sees its end.
	agentEnded := agent != "" && !s.AgentLive(ctx, agent)
	ps := &proxySession{
		id: rec.ID, pid: peer.PID, token: peer.AuditToken, cancel: cancel,
		stop: make(chan proxysession.StopReason, 1), done: make(chan struct{}),
	}
	s.mu.Lock()
	s.running[ps.id] = ps
	s.mu.Unlock()
	if _, err := s.sessions.get(ctx, proxysession.Entity(ps.id)); err != nil {
		s.lose(ps, "no actor")
		return nil, err
	}
	if agentEnded {
		s.lose(ps, "its agent session ended")
		return nil, api.ErrAgentTokenInvalid
	}
	go s.monitor(ps, exited)
	return ps, nil
}

// monitor makes the session Lost when the proxy exits (kqueue NOTE_EXIT) or opens no watch
// stream within readyTimeout.
func (s *secrets) monitor(ps *proxySession, exited <-chan struct{}) {
	timer := time.NewTimer(s.readyTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ps.done:
			return
		case <-exited:
			s.lose(ps, "the proxy exited")
			return
		case <-timer.C:
			if st, err := s.state(s.ctx, proxysession.Entity(ps.id), string(proxysession.Initial)); err != nil || proxysession.State(st) == proxysession.Authorized {
				s.lose(ps, "no ready within 30 seconds")
				return
			}
		}
	}
}

// session finds a running session by id.
func (s *secrets) session(id string) *proxySession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id]
}

// owned finds a running session the peer authorized; any other answer looks the same to the
// caller, so session ids cannot be probed.
func (s *secrets) owned(id string, peer ports.Peer) (*proxySession, error) {
	ps := s.session(id)
	if ps == nil || ps.token != peer.AuditToken {
		return nil, api.ErrPeerNotTrusted
	}
	return ps, nil
}

func (s *secrets) WatchProxySession(ctx context.Context, id string, peer ports.Peer, send func(*berthv1.ProxySessionEvent) error) error {
	ps, err := s.owned(id, peer)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	second := ps.watching
	ps.watching = true
	ps.mu.Unlock()
	if second {
		return api.ErrPeerNotTrusted
	}
	_, _, err = step(ctx, s.sessions, proxysession.Entity(id), proxysession.Event{Kind: proxysession.ProxyReady}, nil, proxysession.ProxyReady)
	switch {
	case err == nil:
		if err := send(&berthv1.ProxySessionEvent{Kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING}); err != nil {
			go s.dropped(ps)
			return err
		}
	case errors.Is(err, machine.ErrIllegal):
		// Stopped before it was ready: the stop below is already waiting, or the session is over.
	default:
		go s.dropped(ps)
		return err
	}
	select {
	case r := <-ps.stop:
		return send(&berthv1.ProxySessionEvent{Kind: berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP, StopReason: reasons[r]})
	case <-ps.done:
		// Lost while the proxy still listens: ending the stream makes it zero its values and exit.
		return errors.New("the proxy session is over")
	case <-ctx.Done():
		go s.dropped(ps)
		return ctx.Err()
	}
}

// dropped makes the session Lost unless the proxy ends it within endGrace, since a proxy that
// exits normally closes its stream only after EndProxySession returns.
func (s *secrets) dropped(ps *proxySession) {
	select {
	case <-ps.done:
	case <-time.After(endGrace):
		s.lose(ps, "watch stream dropped")
	}
}

func (s *secrets) EndProxySession(ctx context.Context, req *berthv1.EndProxySessionRequest, peer ports.Peer) (*berthv1.EndProxySessionResponse, error) {
	ps, err := s.owned(req.GetSessionId(), peer)
	if err != nil {
		return nil, err
	}
	injected, refused := count(req.GetInjected()), count(req.GetRefused())
	note := fmt.Sprintf("injected %d, refused %d", injected, refused)
	if err := s.finish(ctx, ps, proxysession.Event{Kind: proxysession.ProxyEnded, Note: note}, injected, refused); err != nil {
		if errors.Is(err, machine.ErrIllegal) {
			return nil, api.ErrPeerNotTrusted
		}
		return nil, err
	}
	return &berthv1.EndProxySessionResponse{}, nil
}

func count(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// finish moves the session to Ended or Lost and records its counts once: the second of two
// racing endings is an illegal event and records nothing.
func (s *secrets) finish(ctx context.Context, ps *proxySession, ev proxysession.Event, injected, refused int64) error {
	if _, _, err := step(ctx, s.sessions, proxysession.Entity(ps.id), ev, nil, ev.Kind); err != nil {
		return err
	}
	if err := s.Sessions.EndProxySession(ctx, ps.id, injected, refused, s.Clock.Now()); err != nil {
		s.Log.Error("cannot record the end of a proxy session", "session", ps.id, "err", err)
	}
	s.mu.Lock()
	delete(s.running, ps.id)
	for t, l := range s.live {
		if l.kind == ports.ConsumerProxy && l.pid == ps.pid {
			delete(s.live, t)
		}
	}
	s.mu.Unlock()
	ps.finished()
	return nil
}

// lose records ProxyLost with zero counts, unless the session already ended.
func (s *secrets) lose(ps *proxySession, note string) {
	err := s.finish(s.ctx, ps, proxysession.Event{Kind: proxysession.ProxyLost, Note: note}, 0, 0)
	if err != nil && !errors.Is(err, machine.ErrIllegal) {
		s.Log.Error("cannot record a lost proxy session", "session", ps.id, "err", err)
	}
}

// stopSessions sends StopRequested to every open session holding the grant. Revoke and remove can
// race on one session, so a second stop, illegal in Stopping, is expected and ignored.
func (s *secrets) stopSessions(ctx context.Context, grantID int64, reason proxysession.StopReason) {
	ids, err := s.Sessions.ProxySessionsForGrant(ctx, grantID)
	s.stopListed(ctx, ids, err, reason, "grant", grantID)
}

// stopAgentSessions stops every open proxy session authorized through the agent session, once
// that agent session has ended.
func (s *secrets) stopAgentSessions(ctx context.Context, agentSession string) {
	// An ended session starts nothing more, so its hourly count is dropped with it.
	s.mu.Lock()
	delete(s.startTimes, agentSession)
	s.mu.Unlock()
	ids, err := s.Sessions.ProxySessionsForAgentSession(ctx, agentSession)
	s.stopListed(ctx, ids, err, proxysession.StopAgentSessionEnded, "agent_session", agentSession)
}

// stopListed stops each session a lookup listed, or logs, under key and value, why the lookup
// failed.
func (s *secrets) stopListed(ctx context.Context, ids []string, err error, reason proxysession.StopReason, key string, value any) {
	if err != nil {
		s.Log.Error("cannot list proxy sessions to stop", key, value, "err", err)
		return
	}
	for _, id := range ids {
		s.stopSession(ctx, id, reason)
	}
}

func (s *secrets) stopSession(ctx context.Context, id string, reason proxysession.StopReason) {
	ev := proxysession.Event{Kind: proxysession.StopRequested, Reason: reason, Note: string(reason)}
	if _, _, err := step(ctx, s.sessions, proxysession.Entity(id), ev, nil, proxysession.StopRequested); err != nil && !errors.Is(err, machine.ErrIllegal) {
		s.Log.Error("cannot stop a proxy session", "session", id, "err", err)
	}
}

// stopAllSessions sends STOP to every running session and waits up to timeout for them to end.
func (s *secrets) stopAllSessions(timeout time.Duration) bool {
	s.mu.Lock()
	running := make([]*proxySession, 0, len(s.running))
	for _, ps := range s.running {
		running = append(running, ps)
	}
	s.mu.Unlock()
	for _, ps := range running {
		s.stopSession(s.ctx, ps.id, proxysession.StopDaemonStopping)
	}
	deadline := time.Now().Add(timeout)
	for _, ps := range running {
		if done, _ := awaitDone(s.ctx, ps.done, time.Until(deadline)); !done {
			return false
		}
	}
	return true
}

// recoverSessions marks every session a previous run left open as Lost with zero counts.
func (s *secrets) recoverSessions(ctx context.Context) error {
	ids, err := s.Sessions.OpenProxySessions(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		entity := proxysession.Entity(id)
		st, err := s.state(ctx, entity, string(proxysession.Initial))
		if err != nil {
			return err
		}
		switch proxysession.State(st) {
		case proxysession.Authorized, proxysession.Serving, proxysession.Stopping:
			if _, _, err := step(ctx, s.sessions, entity, proxysession.Event{Kind: proxysession.ProxyLost, Note: "berthd restarted"}, nil, proxysession.ProxyLost); err != nil {
				return fmt.Errorf("end interrupted proxy session %s: %w", id, err)
			}
		case proxysession.Ended, proxysession.Lost:
		}
		if err := s.Sessions.EndProxySession(ctx, id, 0, 0, s.Clock.Now()); err != nil {
			return fmt.Errorf("record interrupted proxy session %s: %w", id, err)
		}
	}
	return nil
}
