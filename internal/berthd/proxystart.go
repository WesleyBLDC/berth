package berthd

import (
	"context"
	"io"
	"os"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/agentsession"
	"berth/internal/proxyctl"
	"berth/internal/proxyproc"
)

const (
	// maxStartedProxies bounds the berth-proxy processes one agent session can have berthd run
	// at once, since each one is a process of the owner's that the agent asked for.
	maxStartedProxies = 4
	// maxStartsPerHour bounds how many proxies one agent session can have berthd start in any
	// rolling hour, so a session cannot churn owner processes within the concurrent limit.
	maxStartsPerHour = 30
	maxCAFile        = 64 << 10
)

// StartProxy starts the signed berth-proxy beside berthd, as berthd's user, for the caller, so an
// agent never holds the process that holds the values: it cannot signal, trace or read it. The
// proxy authorizes on the owner socket like any other, and berthd records the caller's agent
// session on its proxy session, so the agent session's end stops it. It runs until ctx ends
// (the caller's stream closed), its session stops, or it exits.
func (s *secrets) StartProxy(ctx context.Context, req *berthv1.StartProxyRequest, send func(*berthv1.StartProxyResponse) error) error {
	for _, name := range req.GetNames() {
		_, ok, err := s.activeProxyGrant(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return send(&berthv1.StartProxyResponse{Outcome: berthv1.UseOutcome_USE_OUTCOME_NO_GRANT})
		}
	}
	agent := api.AgentSession(ctx)
	if !s.reserveStart(agent) {
		return api.ErrRateLimited
	}
	defer s.releaseStart(agent)
	if s.StartProxyProcess == nil {
		return api.ErrProxyNotStarted
	}
	// The proxy outlives this call by up to stopWait, so it hangs off berthd's context.
	p, err := s.StartProxyProcess(s.ctx) //nolint:contextcheck // see above
	if err != nil {
		s.Log.Error("cannot start berth-proxy", "agent_session", agent, "err", err)
		return api.ErrProxyNotStarted
	}
	// Recorded before the request goes out, so the AuthorizeProxy it leads to finds the agent
	// session. The child is reaped only after the record is gone, so while it exists the pid can
	// name no other process; the removal takes only this call's own record.
	rec := &startRecord{agent: agent}
	s.mu.Lock()
	s.started[p.PID] = rec
	s.mu.Unlock()
	// The exit comes from kqueue NOTE_EXIT, which fires before the child is reaped; a child already
	// gone fails the watch and counts as exited.
	watchCtx, stopWatch := context.WithCancel(s.ctx) //nolint:contextcheck // the watch outlives the call by up to stopWait
	defer stopWatch()
	exited, err := s.Exits(watchCtx, p.PID) //nolint:contextcheck // see above
	if err != nil {
		closed := make(chan struct{})
		close(closed)
		exited = closed
	}
	// Closing stdin is how the proxy learns its run is over: it ends its session and exits.
	closeOnDone := context.AfterFunc(ctx, func() { _ = p.Stdin.Close() })
	defer func() {
		closeOnDone()
		_ = p.Stdin.Close()
		select {
		case <-exited:
		case <-time.After(s.stopWait):
			s.Log.Warn("berth-proxy did not exit in time; killed it", "pid", p.PID)
			_ = p.Kill()
			<-exited
		}
		s.forget(p.PID, rec)
		_ = p.Wait()
	}()

	ctl, err := proxyproc.Handshake(p, req.GetNames(), proxyctl.ModeAgent, s.readyTimeout)
	if err != nil {
		s.Log.Warn("berth-proxy did not start for a caller", "agent_session", agent, "pid", p.PID, "err", err)
		_ = p.Kill()
		return api.ErrProxyNotStarted
	}
	// Only a process the proxy pin trusted holds a session; a control line from anything else
	// berthd might have started is never passed on.
	ps := s.sessionOf(p.PID)
	if ps == nil {
		s.Log.Warn("the process berthd started as berth-proxy printed a control line without a session", "pid", p.PID)
		_ = p.Kill()
		return api.ErrProxyNotStarted
	}
	ca, err := readCA(ctl.CAFile)
	if err != nil {
		s.Log.Warn("cannot read berth-proxy's CA certificate", "pid", p.PID, "err", err)
		_ = p.Kill()
		return api.ErrProxyNotStarted
	}
	if err := send(&berthv1.StartProxyResponse{
		Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, SessionId: ps.id,
		Port: uint32(ctl.Port), Credential: ctl.Credential, CaPem: string(ca), Placeholders: ctl.Placeholders, //nolint:gosec // Check bounds the port to 1 to 65535
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-exited:
	}
	return nil
}

// startRecord is one StartProxy call's claim on its child's pid.
type startRecord struct{ agent string }

// reserveStart counts a proxy start against the agent session's limits, concurrent and hourly;
// the owner has none.
func (s *secrets) reserveStart(agent string) bool {
	if agent == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, kept := agentsession.Limit{Open: maxStartedProxies, PerHour: maxStartsPerHour}.Admit(s.starting[agent], s.startTimes[agent], s.Clock.Now())
	s.startTimes[agent] = kept
	if !ok {
		return false
	}
	s.starting[agent]++
	return true
}

// forget removes the pid's record only if it is still rec, so it never drops a later call's.
func (s *secrets) forget(pid int, rec *startRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started[pid] == rec {
		delete(s.started, pid)
	}
}

func (s *secrets) releaseStart(agent string) {
	if agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.starting[agent]--; s.starting[agent] <= 0 {
		delete(s.starting, agent)
	}
}

// startedFor is the agent session berthd started the proxy with this pid for, or "".
func (s *secrets) startedFor(pid int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.started[pid]; rec != nil {
		return rec.agent
	}
	return ""
}

// sessionOf finds the running session the proxy with this pid authorized.
func (s *secrets) sessionOf(pid int) *proxySession {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ps := range s.running {
		if ps.pid == pid {
			return ps
		}
	}
	return nil
}

// readCA reads the run's CA certificate from the folder the signed proxy made; it is public.
func readCA(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a path the signed berth-proxy reported, under berthd's own TMPDIR
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxCAFile))
}
