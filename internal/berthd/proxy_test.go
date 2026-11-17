package berthd

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

const proxyHost = "api.example.com"

func (f *fixture) grantProxy(t *testing.T) *berthv1.Consumer {
	t.Helper()
	resp, err := f.s.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{Name: "gh-token", Target: &berthv1.ProxyTarget{
		Host: proxyHost, Port: 443, HeaderName: "Authorization", ValuePrefix: "Bearer ", PlaceholderEnv: "API_KEY",
	}}, peerOf(berthPID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED {
		t.Fatalf("proxy grant %v", resp)
	}
	return resp.GetConsumer()
}

func (f *fixture) authorizeProxy(t *testing.T, names ...string) *berthv1.AuthorizeProxyResponse {
	t.Helper()
	resp, err := f.s.AuthorizeProxy(t.Context(), &berthv1.AuthorizeProxyRequest{Names: names}, peerOf(proxyPID))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) state(t *testing.T, entity string) string {
	t.Helper()
	st, _, err := f.store.LoadState(t.Context(), entity)
	if err != nil {
		t.Fatal(err)
	}
	return st.State
}

// eventually waits for an entity to reach a state the shell moves it to in the background.
func (f *fixture) eventually(t *testing.T, entity, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.state(t, entity) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s is %s, want %s", entity, f.state(t, entity), want)
}

// watcher runs WatchProxySession as the proxy would, collecting what berthd sends.
type watcher struct {
	events chan *berthv1.ProxySessionEvent
	done   chan error
	cancel context.CancelFunc
}

func (f *fixture) watch(t *testing.T, id string) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	w := &watcher{events: make(chan *berthv1.ProxySessionEvent, 4), done: make(chan error, 1), cancel: cancel}
	go func() {
		w.done <- f.s.WatchProxySession(ctx, id, peerOf(proxyPID), func(e *berthv1.ProxySessionEvent) error {
			w.events <- e
			return nil
		})
	}()
	return w
}

func (w *watcher) next(t *testing.T) *berthv1.ProxySessionEvent {
	t.Helper()
	select {
	case e := <-w.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event from the watch stream")
		return nil
	}
}

func serving(t *testing.T, w *watcher) {
	t.Helper()
	if e := w.next(t); e.GetKind() != berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_SERVING {
		t.Fatalf("first event %v", e)
	}
}

func stopped(t *testing.T, w *watcher, want berthv1.ProxyStopReason) {
	t.Helper()
	if e := w.next(t); e.GetKind() != berthv1.ProxySessionEventKind_PROXY_SESSION_EVENT_KIND_STOP || e.GetStopReason() != want {
		t.Fatalf("event %v, want STOP %v", e, want)
	}
}

func TestGrantProxyTargets(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	for _, host := range []string{"localhost", "api.localhost", "127.0.0.1", "127.1", "10.0.0.8"} {
		req := &berthv1.GrantProxyRequest{Name: "gh-token", Target: &berthv1.ProxyTarget{Host: host, Port: 443, HeaderName: "Authorization"}}
		if _, err := f.s.GrantProxy(t.Context(), req, peerOf(berthPID)); !errors.Is(err, api.ErrBadProxyTarget) {
			t.Fatalf("%s: %v", host, err)
		}
	}
	if len(f.approver.PowerPrompts()) != 0 {
		t.Fatal("a refused target reached Touch ID")
	}
	c := f.grantProxy(t)
	if c.GetKind() != berthv1.ConsumerKind_CONSUMER_KIND_PROXY || c.GetProxy().GetHost() != proxyHost || c.GetSha256() != "" || c.GetExePath() != "" {
		t.Fatalf("proxy consumer %v", c)
	}
	p := f.approver.PowerPrompts()
	if len(p) != 1 || p[0].Action != ports.PowerProxyGrant || p[0].Proxy.Host != proxyHost || p[0].Proxy.ValuePrefix != "Bearer " {
		t.Fatalf("power prompts %+v", p)
	}
	f.grant(t, "")
	list, err := f.s.ListSecrets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []berthv1.ConsumerKind
	for _, c := range list.GetSecrets()[0].GetConsumers() {
		kinds = append(kinds, c.GetKind())
	}
	if !slices.Equal(kinds, []berthv1.ConsumerKind{berthv1.ConsumerKind_CONSUMER_KIND_EXECUTABLE, berthv1.ConsumerKind_CONSUMER_KIND_PROXY}) {
		t.Fatalf("consumer kinds %v", kinds)
	}

	local := newFixture(t, &testutil.Approver{})
	local.s.AllowLocalTargets = true
	local.request(t)
	req := &berthv1.GrantProxyRequest{Name: "gh-token", Target: &berthv1.ProxyTarget{Host: "localhost", Port: 8443, HeaderName: "Authorization"}}
	if resp, err := local.s.GrantProxy(t.Context(), req, peerOf(berthPID)); err != nil || resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED {
		t.Fatalf("e2e build grant on localhost: %v %v", resp, err)
	}
}

func TestProxySessionServesUntilRevoked(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	grantID := f.grantProxy(t).GetGrantId()
	ctx := t.Context()

	if err := f.s.TrustProxy(ctx, peerOf(berthPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
		t.Fatalf("berth as the proxy: %v", err)
	}
	if err := f.s.TrustProxy(ctx, peerOf(proxyPID)); err != nil {
		t.Fatal(err)
	}
	if resp := f.authorizeProxy(t, "gh-token", "other"); resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT || resp.GetSessionId() != "" || len(resp.GetTickets()) != 0 {
		t.Fatalf("a name without a grant: %v", resp)
	}
	if open, _ := f.store.OpenProxySessions(ctx); len(open) != 0 {
		t.Fatalf("a refused authorize made sessions %v", open)
	}

	resp := f.authorizeProxy(t, "gh-token")
	id := resp.GetSessionId()
	if resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED || len(id) != 32 || len(resp.GetTickets()) != 1 ||
		resp.GetTickets()[0].GetTarget().GetHost() != proxyHost {
		t.Fatalf("authorize %v", resp)
	}
	rec, ok, err := f.store.ProxySession(ctx, id)
	if err != nil || !ok || rec.PID != proxyPID || !slices.Equal(rec.Grants, []int64{grantID}) {
		t.Fatalf("session row %+v %v %v", rec, ok, err)
	}
	var ticket ports.Ticket
	copy(ticket[:], resp.GetTickets()[0].GetTicket())
	// The helper berth started cannot spend the proxy's ticket; the one the proxy started can.
	if _, err := f.s.RedeemUseTicket(ctx, ticket, peerOf(helperPID)); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("redeem by berth's helper: %v", err)
	}
	copy(ticket[:], f.authorizeProxy(t, "gh-token").GetTickets()[0].GetTicket())
	red, err := f.s.RedeemUseTicket(ctx, ticket, peerOf(proxyHelp))
	if err != nil || red.GetConsumerKind() != berthv1.ConsumerKind_CONSUMER_KIND_PROXY || red.GetAction() != berthv1.TicketAction_TICKET_ACTION_USE {
		t.Fatalf("redeem by the proxy's helper: %v %v", red, err)
	}

	err = f.s.WatchProxySession(ctx, id, ports.Peer{PID: proxyPID, AuditToken: [32]byte{9}}, func(*berthv1.ProxySessionEvent) error { return nil })
	if !errors.Is(err, api.ErrPeerNotTrusted) {
		t.Fatalf("watch by another audit token: %v", err)
	}
	if _, err := f.s.EndProxySession(ctx, &berthv1.EndProxySessionRequest{SessionId: "0123456789abcdef0123456789abcdef"}, peerOf(proxyPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
		t.Fatalf("end of an unknown session: %v", err)
	}
	w := f.watch(t, id)
	serving(t, w)
	if got := f.state(t, "proxy/"+id); got != "Serving" {
		t.Fatalf("session %s", got)
	}

	if r, err := f.s.RevokeGrant(ctx, &berthv1.RevokeGrantRequest{GrantId: grantID}, peerOf(agentPID)); err != nil || r.GetState() != berthv1.GrantState_GRANT_STATE_REVOKED {
		t.Fatalf("revoke %v %v", r, err)
	}
	stopped(t, w, berthv1.ProxyStopReason_PROXY_STOP_REASON_GRANT_REVOKED)
	if _, err := f.s.EndProxySession(ctx, &berthv1.EndProxySessionRequest{SessionId: id, Injected: 3, Refused: 1}, peerOf(proxyPID)); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "proxy/"+id); got != "Ended" {
		t.Fatalf("session %s", got)
	}
	if rec, _, _ := f.store.ProxySession(ctx, id); rec.Injected != 3 || rec.Refused != 1 || rec.EndedAt.IsZero() {
		t.Fatalf("counts %+v", rec)
	}
	if _, err := f.s.EndProxySession(ctx, &berthv1.EndProxySessionRequest{SessionId: id, Injected: 9}, peerOf(proxyPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
		t.Fatalf("second end: %v", err)
	}
	if rec, _, _ := f.store.ProxySession(ctx, id); rec.Injected != 3 {
		t.Fatalf("the second end changed the counts: %+v", rec)
	}
	if resp := f.authorizeProxy(t, "gh-token"); resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
		t.Fatalf("authorize after revoke %v", resp)
	}
	if r, err := f.s.RevokeGrant(ctx, &berthv1.RevokeGrantRequest{GrantId: grantID}, peerOf(agentPID)); err != nil || r.GetState() != berthv1.GrantState_GRANT_STATE_REVOKED {
		t.Fatalf("second revoke %v %v", r, err)
	}
	if _, err := f.s.RevokeGrant(ctx, &berthv1.RevokeGrantRequest{GrantId: 99}, peerOf(agentPID)); !errors.Is(err, api.ErrGrantNotFound) {
		t.Fatalf("unknown grant: %v", err)
	}
}

func TestProxySessionLost(t *testing.T) {
	t.Run("the proxy exits", func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grantProxy(t)
		id := f.authorizeProxy(t, "gh-token").GetSessionId()
		w := f.watch(t, id)
		serving(t, w)
		f.exits.exit(proxyPID)
		f.eventually(t, "proxy/"+id, "Lost")
		// The stream ends too, so a proxy berthd lost track of stops serving.
		select {
		case err := <-w.done:
			if err == nil {
				t.Fatal("the stream ended cleanly")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream outlived its session")
		}
	})
	t.Run("no ready in time", func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.s.readyTimeout = 50 * time.Millisecond
		f.request(t)
		f.grantProxy(t)
		id := f.authorizeProxy(t, "gh-token").GetSessionId()
		f.eventually(t, "proxy/"+id, "Lost")
		if h := f.history(t, "proxy/"+id); h[0].Detail != "no ready within 30 seconds" {
			t.Fatalf("history %+v", h)
		}
	})
	t.Run("the stream drops without an end", func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grantProxy(t)
		id := f.authorizeProxy(t, "gh-token").GetSessionId()
		w := f.watch(t, id)
		serving(t, w)
		w.cancel()
		f.eventually(t, "proxy/"+id, "Lost")
		if rec, _, _ := f.store.ProxySession(t.Context(), id); rec.EndedAt.IsZero() || rec.Injected != 0 {
			t.Fatalf("lost session row %+v", rec)
		}
	})
}

func TestDaemonStopSendsStop(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	f.grantProxy(t)
	id := f.authorizeProxy(t, "gh-token").GetSessionId()
	w := f.watch(t, id)
	serving(t, w)
	go func() {
		stopped(t, w, berthv1.ProxyStopReason_PROXY_STOP_REASON_DAEMON_STOPPING)
		_, _ = f.s.EndProxySession(context.Background(), &berthv1.EndProxySessionRequest{SessionId: id}, peerOf(proxyPID))
	}()
	if !f.s.stop(5 * time.Second) {
		t.Fatal("stop timed out")
	}
	if got := f.state(t, "proxy/"+id); got != "Ended" {
		t.Fatalf("session %s", got)
	}
}

func TestRemoveRevokesEveryGrant(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	exeGrant := f.grant(t, "").GetConsumer().GetGrantId()
	proxyGrant := f.grantProxy(t).GetGrantId()
	ctx := t.Context()
	id := f.authorizeProxy(t, "gh-token").GetSessionId()
	w := f.watch(t, id)
	serving(t, w)
	pending := ticketOf(f.use(t))

	// A second proxy grant revoked first: remove must tolerate the session already Stopping.
	second := f.grantProxy(t).GetGrantId()
	if _, err := f.s.RevokeGrant(ctx, &berthv1.RevokeGrantRequest{GrantId: second}, peerOf(agentPID)); err != nil {
		t.Fatal(err)
	}

	f.approver.QueueRemove(ports.RemoveDenied)
	resp, err := f.s.RemoveSecret(ctx, &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID))
	if err != nil || resp.GetOutcome() != berthv1.RemoveOutcome_REMOVE_OUTCOME_DENIED || f.state(t, "secret/gh-token") != "Stored" {
		t.Fatalf("denied remove %v %v", resp, err)
	}
	resp, err = f.s.RemoveSecret(ctx, &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID))
	if err != nil || resp.GetOutcome() != berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED {
		t.Fatalf("remove %v %v", resp, err)
	}
	if !slices.Equal(resp.GetRevokedGrantIds(), []int64{exeGrant, proxyGrant}) {
		t.Fatalf("revoked %v, want %d and %d", resp.GetRevokedGrantIds(), exeGrant, proxyGrant)
	}
	calls := f.approver.LifecycleCalls()
	if len(calls) != 2 || calls[1].Action != ports.TicketRemove || calls[1].Prompt.Name != "gh-token" || calls[1].Prompt.Requester.PID != agentPID {
		t.Fatalf("lifecycle calls %+v", calls)
	}
	// The action ticket was live only while the helper ran.
	if _, err := f.s.RedeemUseTicket(ctx, calls[1].Ticket, peerOf(daemonHelp)); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("action ticket after the prompt: %v", err)
	}
	if _, err := f.s.RedeemUseTicket(ctx, pending, peerOf(helperPID)); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("use ticket after remove: %v", err)
	}
	stopped(t, w, berthv1.ProxyStopReason_PROXY_STOP_REASON_SECRET_REMOVED)
	if got := f.state(t, "secret/gh-token"); got != "Removed" {
		t.Fatalf("secret %s", got)
	}
	if resp := f.authorizeProxy(t, "gh-token"); resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
		t.Fatalf("authorize after remove %v", resp)
	}
	if _, err := f.s.RemoveSecret(ctx, &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID)); !errors.Is(err, api.ErrSecretNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	// The name is free again.
	if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
		t.Fatalf("store after remove %v", got)
	}
}

// actionApprover checks the action ticket the way berth-native does, by redeeming it from a
// helper berthd started, and can hold a rotate open.
type actionApprover struct {
	testutil.Approver
	s        func() *secrets
	redeemed chan *berthv1.RedeemUseTicketResponse
	entered  chan struct{}
	release  chan struct{}
}

func (a *actionApprover) RotateSecret(ctx context.Context, p ports.LifecyclePrompt, t ports.Ticket) (ports.StoreOutcome, error) {
	resp, err := a.s().RedeemUseTicket(ctx, t, peerOf(daemonHelp))
	if err != nil {
		return ports.StoreFailed, err
	}
	a.redeemed <- resp
	if a.entered != nil {
		a.entered <- struct{}{}
		<-a.release
	}
	return a.Approver.RotateSecret(ctx, p, t)
}

func TestRotateKeepsGrantsAndRefusesOverlaps(t *testing.T) {
	a := &actionApprover{redeemed: make(chan *berthv1.RedeemUseTicketResponse, 4), entered: make(chan struct{}, 1), release: make(chan struct{})}
	f := newFixture(t, a)
	a.s = func() *secrets { return f.s }
	f.request(t)
	exeGrant := f.grant(t, "").GetConsumer().GetGrantId()
	ctx := t.Context()

	done := make(chan *berthv1.RotateSecretResponse, 1)
	go func() {
		resp, err := f.s.RotateSecret(ctx, &berthv1.RotateSecretRequest{Name: "gh-token"}, peerOf(agentPID))
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	<-a.entered
	if red := <-a.redeemed; red.GetName() != "gh-token" || red.GetAction() != berthv1.TicketAction_TICKET_ACTION_ROTATE {
		t.Fatalf("action ticket redeemed as %v", red)
	}
	if got := f.state(t, "secret/gh-token"); got != "Rotating" {
		t.Fatalf("secret %s", got)
	}
	// Uses go on with the old value; another rotate, a remove or a store waits its turn.
	if use := f.use(t); use.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		t.Fatalf("use while rotating %v", use)
	}
	if _, err := f.s.RotateSecret(ctx, &berthv1.RotateSecretRequest{Name: "gh-token"}, peerOf(agentPID)); !errors.Is(err, api.ErrBusy) {
		t.Fatalf("second rotate: %v", err)
	}
	if _, err := f.s.RemoveSecret(ctx, &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID)); !errors.Is(err, api.ErrBusy) {
		t.Fatalf("remove while rotating: %v", err)
	}
	if _, err := f.s.RequestSecret(ctx, &berthv1.RequestSecretRequest{Name: "gh-token", Reason: "x"}, peerOf(agentPID)); !errors.Is(err, api.ErrBusy) {
		t.Fatalf("store while rotating: %v", err)
	}
	close(a.release)
	if resp := <-done; resp.GetOutcome() != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
		t.Fatalf("rotate %v", resp)
	}
	if got := f.state(t, "grant/"+itoa(exeGrant)); got != "Active" {
		t.Fatalf("grant after rotate %s", got)
	}

	a.entered = nil
	a.QueueRotate(ports.StoreDenied)
	if resp, err := f.s.RotateSecret(ctx, &berthv1.RotateSecretRequest{Name: "gh-token"}, peerOf(agentPID)); err != nil || resp.GetOutcome() != berthv1.StoreOutcome_STORE_OUTCOME_DENIED {
		t.Fatalf("denied rotate %v %v", resp, err)
	}
	if _, err := f.s.RotateSecret(ctx, &berthv1.RotateSecretRequest{Name: "missing"}, peerOf(agentPID)); !errors.Is(err, api.ErrSecretNotFound) {
		t.Fatalf("rotate of a missing secret: %v", err)
	}
}

func TestLifecycleWaitsForARequestedGrant(t *testing.T) {
	gate := &gatedPower{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f := newFixture(t, gate)
	f.request(t)
	granted := make(chan struct{})
	go func() { f.grantProxy(t); close(granted) }()
	<-gate.entered
	_, err := f.s.RemoveSecret(t.Context(), &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID))
	close(gate.release)
	<-granted
	if !errors.Is(err, api.ErrBusy) {
		t.Fatalf("remove with a grant waiting for Touch ID: %v", err)
	}
}

type gatedPower struct {
	testutil.Approver
	entered chan struct{}
	release chan struct{}
}

func (g *gatedPower) ConfirmPower(ctx context.Context, p ports.PowerPrompt) (ports.PowerOutcome, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.Approver.ConfirmPower(ctx, p)
}

func TestBootRecoversLifecycleAndSessions(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	ctx := t.Context()
	f.request(t)
	f.grantProxy(t)
	id := f.authorizeProxy(t, "gh-token").GetSessionId()
	serving(t, f.watch(t, id))
	at := f.clock.Now()
	save := func(entity, kind, event, from, to string, version int64) {
		t.Helper()
		if err := f.store.SaveTransition(ctx, ports.TransitionRecord{Entity: entity, Kind: kind, Version: version, Event: event, From: from, To: to, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"mid-rotate", "mid-remove", "gone"} {
		if err := f.store.AddSecret(ctx, name, at); err != nil {
			t.Fatal(err)
		}
		save("secret/"+name, "secret", "StoreSucceeded", "Pending", "Stored", 1)
	}
	save("secret/mid-rotate", "secret", "RotateRequested", "Stored", "Rotating", 2)
	save("secret/mid-remove", "secret", "RemoveRequested", "Stored", "Removing", 2)
	// A run that crashed between RemoveSucceeded and revoking the grants.
	gone, err := f.store.AddGrant(ctx, ports.GrantRecord{Secret: "gone", Kind: ports.ConsumerProxy, Proxy: ports.ProxyTarget{Host: proxyHost, Port: 443, HeaderName: "X-Key"}, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	save("grant/"+itoa(gone), "grant", "GrantApproved", "Requested", "Active", 1)
	save("secret/gone", "secret", "RemoveSucceeded", "Removing", "Removed", 2)

	next := newSecrets(context.Background(), f.s.secretsDeps)
	if err := next.recover(ctx); err != nil {
		t.Fatal(err)
	}
	for entity, want := range map[string]string{
		"proxy/" + id: "Lost", "secret/mid-rotate": "Stored", "secret/mid-remove": "Stored", "grant/" + itoa(gone): "Revoked",
	} {
		if got := f.state(t, entity); got != want {
			t.Errorf("%s is %s, want %s", entity, got, want)
		}
	}
	for _, entity := range []string{"secret/mid-rotate", "secret/mid-remove"} {
		if h := f.history(t, entity); h[0].Detail != "interrupted" {
			t.Errorf("%s history %+v", entity, h[0])
		}
	}
	if open, _ := f.store.OpenProxySessions(ctx); len(open) != 0 {
		t.Fatalf("sessions still open %v", open)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// gatedPeers holds the next Facts call for an armed pid until the test releases it, so a test can
// stop a call between its first check and its next step.
type gatedPeers struct {
	ports.PeerFacts
	mu    sync.Mutex
	gates map[int]*gate
}

type gate struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gatedPeers) arm(pid int) *gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	gt := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	g.gates[pid] = gt
	return gt
}

func (g *gatedPeers) Facts(ctx context.Context, pid int) (ports.Requester, error) {
	g.mu.Lock()
	gt := g.gates[pid]
	delete(g.gates, pid)
	g.mu.Unlock()
	if gt != nil {
		close(gt.entered)
		<-gt.release
	}
	return g.PeerFacts.Facts(ctx, pid)
}

// TestRemoveRacingAGrantLeavesNoActiveGrant interleaves a grant and an rm at each point where
// one's check could miss the other, and requires that no grant ends Active on a removed secret.
func TestRemoveRacingAGrantLeavesNoActiveGrant(t *testing.T) {
	type outcome struct {
		resp *berthv1.GrantProxyResponse
		err  error
	}
	setup := func(t *testing.T) (*fixture, *gatedPeers, *gatedPower) {
		t.Helper()
		power := &gatedPower{entered: make(chan struct{}, 1), release: make(chan struct{})}
		f := newFixture(t, power)
		close(power.release)
		f.request(t)
		peers := &gatedPeers{PeerFacts: f.s.Peers, gates: map[int]*gate{}}
		f.s.Peers = peers
		return f, peers, power
	}
	grantAsync := func(f *fixture) chan outcome {
		done := make(chan outcome, 1)
		go func() {
			resp, err := f.s.GrantProxy(context.Background(), &berthv1.GrantProxyRequest{Name: "gh-token", Target: &berthv1.ProxyTarget{
				Host: proxyHost, Port: 443, HeaderName: "Authorization", PlaceholderEnv: "API_KEY",
			}}, peerOf(berthPID))
			done <- outcome{resp, err}
		}()
		return done
	}
	removeAsync := func(f *fixture) chan *berthv1.RemoveSecretResponse {
		done := make(chan *berthv1.RemoveSecretResponse, 1)
		go func() {
			resp, err := f.s.RemoveSecret(context.Background(), &berthv1.RemoveSecretRequest{Name: "gh-token"}, peerOf(agentPID))
			if err != nil {
				t.Error(err)
			}
			done <- resp
		}()
		return done
	}
	noActiveAfterReAdd := func(t *testing.T, f *fixture) {
		t.Helper()
		if got := f.state(t, "secret/gh-token"); got != "Removed" {
			t.Fatalf("secret %s", got)
		}
		if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
			t.Fatalf("re-add %v", got)
		}
		list, err := f.s.ListSecrets(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list.GetSecrets()[0].GetConsumers() {
			if c.GetState() == berthv1.GrantState_GRANT_STATE_ACTIVE || c.GetState() == berthv1.GrantState_GRANT_STATE_REQUESTED {
				t.Fatalf("re-added secret has a live grant %v", c)
			}
		}
		if resp := f.authorizeProxy(t, "gh-token"); resp.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
			t.Fatalf("a grant from before the rm serves the re-added value: %v", resp)
		}
	}

	t.Run("rm finishes between the grant's check and its prompt", func(t *testing.T) {
		f, peers, power := setup(t)
		held := peers.arm(berthPID)
		granted := grantAsync(f)
		<-held.entered
		if resp := <-removeAsync(f); resp.GetOutcome() != berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED {
			t.Fatalf("remove %v", resp)
		}
		close(held.release)
		got := <-granted
		if got.err != nil || got.resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_FAILED {
			t.Fatalf("grant after rm %v %v", got.resp, got.err)
		}
		if n := len(power.PowerPrompts()); n != 0 {
			t.Fatalf("%d Touch ID prompts for a removed secret", n)
		}
		noActiveAfterReAdd(t, f)
	})

	t.Run("rm finishes while the grant's Touch ID is open", func(t *testing.T) {
		f, peers, power := setup(t)
		power.release = make(chan struct{})
		heldGrant := peers.arm(berthPID)
		granted := grantAsync(f)
		<-heldGrant.entered
		// The rm passes its check for Requested grants before the grant row exists.
		heldRemove := peers.arm(agentPID)
		removed := removeAsync(f)
		<-heldRemove.entered
		close(heldGrant.release)
		<-power.entered
		close(heldRemove.release)
		f.eventually(t, "secret/gh-token", "Removed")
		close(power.release)
		got := <-granted
		if got.err != nil || got.resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_FAILED {
			t.Fatalf("grant approved during rm %v %v", got.resp, got.err)
		}
		if resp := <-removed; resp.GetOutcome() != berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED || len(resp.GetRevokedGrantIds()) != 0 {
			t.Fatalf("remove %v", resp)
		}
		noActiveAfterReAdd(t, f)
	})

	t.Run("a grant on a Removing secret is busy", func(t *testing.T) {
		a := &gatedRemove{entered: make(chan struct{}, 1), release: make(chan struct{})}
		f := newFixture(t, a)
		f.request(t)
		removed := removeAsync(f)
		<-a.entered
		_, err := f.s.GrantProxy(t.Context(), &berthv1.GrantProxyRequest{Name: "gh-token", Target: &berthv1.ProxyTarget{
			Host: proxyHost, Port: 443, HeaderName: "Authorization",
		}}, peerOf(berthPID))
		close(a.release)
		<-removed
		if !errors.Is(err, api.ErrBusy) {
			t.Fatalf("grant while removing: %v", err)
		}
	})
}

type gatedRemove struct {
	testutil.Approver
	entered chan struct{}
	release chan struct{}
}

func (g *gatedRemove) RemoveSecret(ctx context.Context, p ports.LifecyclePrompt, t ports.Ticket) (ports.RemoveOutcome, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.Approver.RemoveSecret(ctx, p, t)
}

// endRequested is reached when a remove's RevokeAll finds a grant still Requested, such as one
// a crash left waiting for Touch ID; boot recovery ends it too.
func TestRevokeAllEndsRequestedGrants(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	ctx := t.Context()
	at := f.clock.Now()
	if err := f.store.AddSecret(ctx, "gone", at); err != nil {
		t.Fatal(err)
	}
	id, err := f.store.AddGrant(ctx, ports.GrantRecord{Secret: "gone", Kind: ports.ConsumerProxy, Proxy: ports.ProxyTarget{Host: proxyHost, Port: 443, HeaderName: "X-Key"}, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	for v, r := range []ports.TransitionRecord{
		{Entity: "secret/gone", Kind: "secret", Event: "StoreSucceeded", From: "Pending", To: "Stored"},
		{Entity: "secret/gone", Kind: "secret", Event: "RemoveSucceeded", From: "Removing", To: "Removed"},
	} {
		r.Version, r.At = int64(v+1), at
		if err := f.store.SaveTransition(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "grant/"+itoa(id)); got != "Denied" {
		t.Fatalf("requested grant of a removed secret is %s", got)
	}
	if h := f.history(t, "grant/"+itoa(id)); h[0].Event != "GrantFailed" || h[0].Detail != "secret removed" {
		t.Fatalf("history %+v", h[0])
	}
}
