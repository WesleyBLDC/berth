package berthd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

const agentUID = 600

// anyLive is a ports.CodeInspector for which every process in the table still runs.
type anyLive struct{ procs *testutil.Processes }

func (l anyLive) Live(ctx context.Context, p ports.Peer) (ports.LiveCode, error) {
	if _, err := l.procs.Lineage(ctx, p.PID); err != nil {
		return ports.LiveCode{}, errors.New("no such process")
	}
	return ports.LiveCode{}, nil
}

type agentsFixture struct {
	a       *agents
	stopped chan string
	timers  *fakeTimers
	exits   *fakeExits
	store   *sqlite.Store
	clock   *testutil.Clock
	procs   *testutil.Processes
	log     *bytes.Buffer
	dir     string
}

func fp(pid, ppid int, uid uint32) testutil.FakeProcess {
	return testutil.FakeProcess{ProcessInfo: ports.ProcessInfo{
		ProcessRef: ports.ProcessRef{PID: pid, Start: time.Unix(int64(pid), 0)}, PPID: ppid, UID: uid,
	}}
}

// Two harnesses, each started by a root launch helper, plus a process launchd adopted.
var agentTable = []testutil.FakeProcess{
	fp(1, 0, 0),
	fp(10, 1, 0), fp(20, 10, agentUID), fp(21, 20, agentUID), fp(22, 21, agentUID), fp(23, 20, agentUID),
	fp(11, 1, 0), fp(30, 11, agentUID), fp(31, 30, agentUID),
	fp(40, 1, agentUID),
	fp(50, 20, 501),
}

func newAgentsFixture(t *testing.T) *agentsFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(t.Context(), filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	procs := &testutil.Processes{}
	procs.Set(agentTable...)
	var log bytes.Buffer
	clock := testutil.NewClock()
	f := &agentsFixture{
		store: store, clock: clock, procs: procs, log: &log, dir: dir, stopped: make(chan string, 8),
		timers: &fakeTimers{}, exits: &fakeExits{byPID: map[int]chan struct{}{}},
	}
	f.a = f.newAgents(t, store)
	return f
}

// newAgents builds an agents over store, as a fresh berthd would at boot.
func (f *agentsFixture) newAgents(t *testing.T, store ports.Store) *agents {
	t.Helper()
	records := f.store
	return newAgents(t.Context(), agentsDeps{
		Log: slog.New(slog.NewTextHandler(f.log, nil)), Store: store, Records: records, Clock: f.clock,
		Lineage: f.procs, Code: anyLive{f.procs}, AgentUID: agentUID,
		// A stop must run on berthd's context, never a caller's that may be cancelled.
		StopProxies: func(ctx context.Context, id string) {
			if ctx.Err() != nil {
				id = "cancelled context: " + id
			}
			f.stopped <- id
		},
		CloseBrowsers: func(context.Context, string) {},
		WatchExit:     f.exits.watch, After: f.timers.after,
	})
}

// waitStopped waits for the proxies of id to be stopped.
func (f *agentsFixture) waitStopped(t *testing.T, id string) {
	t.Helper()
	select {
	case got := <-f.stopped:
		if got != id {
			t.Fatalf("stopped %q, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session's proxies were never stopped")
	}
}

// fakeTimers records each timer, so a test fires it instead of waiting.
type fakeTimers struct {
	mu  sync.Mutex
	fns []func()
	ds  []time.Duration
}

func (ft *fakeTimers) after(d time.Duration, f func()) func() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	cancelled := false
	ft.ds = append(ft.ds, d)
	ft.fns = append(ft.fns, func() {
		ft.mu.Lock()
		c := cancelled
		ft.mu.Unlock()
		if !c {
			f()
		}
	})
	return func() { ft.mu.Lock(); cancelled = true; ft.mu.Unlock() }
}

func (ft *fakeTimers) fire(i int) {
	ft.mu.Lock()
	f := ft.fns[i]
	ft.mu.Unlock()
	f()
}

// fakeExits is a WatchExit whose processes exit when the test says so.
type fakeExits struct {
	mu    sync.Mutex
	byPID map[int]chan struct{}
	// gone pids exited before the watch, which then answers ErrProcessGone.
	gone map[int]bool
	// err, when set, is what every watch answers, as when berthd is out of descriptors.
	err error
}

func (e *fakeExits) watch(_ context.Context, p ports.ProcessRef) (<-chan struct{}, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone[p.PID] {
		return nil, ports.ErrProcessGone
	}
	if e.err != nil {
		return nil, e.err
	}
	if _, ok := e.byPID[p.PID]; !ok {
		e.byPID[p.PID] = make(chan struct{})
	}
	return e.byPID[p.PID], nil
}

// exitRoot ends pid 20, the root every bound test session in agentTable has.
func (e *fakeExits) exitRoot(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.byPID[20]
	if !ok {
		t.Fatal("nothing watches the root, pid 20")
	}
	close(ch)
}

func (f *agentsFixture) create(t *testing.T, ttl uint32) (string, string) {
	t.Helper()
	resp, err := f.a.CreateAgentSession(t.Context(), &berthv1.CreateAgentSessionRequest{RequestId: "create-0001", Name: "h", TtlSeconds: ttl})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.GetToken(), agentTokenPrefix) || len(resp.GetToken()) != len(agentTokenPrefix)+43 {
		t.Fatalf("token has the wrong shape (length %d)", len(resp.GetToken()))
	}
	return resp.GetSession().GetId(), resp.GetToken()
}

func (f *agentsFixture) auth(t *testing.T, token string, pid int) error {
	t.Helper()
	_, err := f.a.Authenticate(t.Context(), token, ports.Peer{PID: pid})
	return err
}

func (f *agentsFixture) state(t *testing.T, id string) agentsession.State {
	t.Helper()
	st, err := f.a.state(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAgentTokenBindsToTheFirstCallersTree(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if f.state(t, id) != agentsession.Issued {
		t.Fatal("a new session is not Issued")
	}
	// The first call comes from deep inside harness 20's tree; the session binds to 20.
	if err := f.auth(t, token, 22); err != nil {
		t.Fatalf("first call: %v", err)
	}
	rec, _, _ := f.store.AgentSession(t.Context(), id)
	if rec.Root.PID != 20 || f.state(t, id) != agentsession.Bound {
		t.Fatalf("bound to %d in %s, want 20 in Bound", rec.Root.PID, f.state(t, id))
	}
	for _, pid := range []int{20, 21, 23} {
		if err := f.auth(t, token, pid); err != nil {
			t.Errorf("pid %d in the tree refused: %v", pid, err)
		}
	}
	// Another harness of the same agent user, a process launchd adopted, another user, and
	// berthd's unknown caller are all outside the tree.
	for _, pid := range []int{30, 31, 40, 50, 0} {
		if err := f.auth(t, token, pid); !errors.Is(err, api.ErrAgentTokenInvalid) {
			t.Errorf("pid %d outside the tree: %v", pid, err)
		}
	}
	if f.state(t, id) != agentsession.Bound {
		t.Fatalf("refusals changed the session to %s", f.state(t, id))
	}
	if err := f.auth(t, token+"x", 22); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a changed token: %v", err)
	}
	if strings.Contains(f.log.String(), token) {
		t.Fatal("the token reached the log")
	}
	// Only the hash is stored.
	for _, name := range []string{"b.db", "b.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(f.dir, name))
		if bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte(token[len(agentTokenPrefix):])) {
			t.Fatalf("the token is in %s", name)
		}
	}
}

func TestAgentTokenFirstCallerMustBeTheAgentUser(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 50); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("another user's first call: %v", err)
	}
	if f.state(t, id) != agentsession.Issued {
		t.Fatal("a refused first call bound the session")
	}
}

// goneCode is a ports.CodeInspector for which the caller exited after its lineage was read: the
// pid may name a later process by now, which its audit token catches.
type goneCode struct{}

func (goneCode) Live(context.Context, ports.Peer) (ports.LiveCode, error) {
	return ports.LiveCode{}, errors.New("no such process")
}

func TestAgentTokenOfACallerThatExitedIsRefused(t *testing.T) {
	f := newAgentsFixture(t)
	f.a.Code = goneCode{}
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 22); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a caller that exited: %v", err)
	}
	if f.state(t, id) != agentsession.Issued || !strings.Contains(f.log.String(), "caller exited") {
		t.Fatalf("session %s after the refusal; log %q", f.state(t, id), f.log.String())
	}
}

func TestAgentTokenExpires(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 60)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	if err := f.auth(t, token, 21); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("expired token: %v", err)
	}
	if f.state(t, id) != agentsession.Expired {
		t.Fatalf("state %s, want Expired", f.state(t, id))
	}
	// An unused session expires too, as the owner's listing shows.
	id2, _ := f.create(t, 60)
	f.clock.Advance(2 * time.Minute)
	list, err := f.a.ListAgentSessions(t.Context())
	if err != nil || len(list.GetSessions()) != 2 || list.GetSessions()[1].GetState() != berthv1.AgentSessionState_AGENT_SESSION_STATE_EXPIRED {
		t.Fatalf("list: %v %v", list, err)
	}
	if f.state(t, id2) != agentsession.Expired {
		t.Fatal("listing did not record the expiry")
	}
}

func TestAgentTokenRevoked(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resp, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: id})
		if err != nil || resp.GetSession().GetState() != berthv1.AgentSessionState_AGENT_SESSION_STATE_REVOKED {
			t.Fatalf("revoke: %v %v", resp, err)
		}
	}
	if err := f.auth(t, token, 21); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("revoked token: %v", err)
	}
	if n := f.a.sessions.count(); n != 0 {
		t.Fatalf("%d actors still run for an ended session", n)
	}
	if _, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0002", Id: strings.Repeat("0", 32)}); !errors.Is(err, api.ErrAgentSessionNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestAgentSessionEndsWithItsRoot(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}
	// Harness 20 exits; launchd adopts 21. The pid 20 comes back as an unrelated process.
	table := []testutil.FakeProcess{fp(1, 0, 0), fp(21, 1, agentUID)}
	reused := fp(20, 1, agentUID)
	reused.Start = time.Unix(999, 0)
	f.procs.Set(append(table, reused)...)
	if err := f.auth(t, token, 21); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("orphan of an exited root: %v", err)
	}
	if f.state(t, id) != agentsession.Ended {
		t.Fatalf("state %s, want Ended", f.state(t, id))
	}
	if err := f.auth(t, token, 20); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a later process on the root's pid: %v", err)
	}
}

func TestAgentStoreLimits(t *testing.T) {
	f := newAgentsFixture(t)
	var releases []func()
	for range agentsession.MaxOpenStores {
		release, err := f.a.AdmitStore(t.Context(), "s1")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := f.a.AdmitStore(t.Context(), "s1"); !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("a fourth open store: %v", err)
	}
	if release, err := f.a.AdmitStore(t.Context(), "s2"); err != nil {
		t.Fatalf("another session is limited by the first: %v", err)
	} else {
		release()
	}
	releases[0]()
	releases[0]() // a second release frees nothing more
	release, err := f.a.AdmitStore(t.Context(), "s1")
	if err != nil {
		t.Fatalf("after one closed: %v", err)
	}
	if _, err := f.a.AdmitStore(t.Context(), "s1"); !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("a double release opened two slots: %v", err)
	}
	release()
	for _, r := range releases[1:] {
		r()
	}
	// Four admitted so far for s1; six more fit in the hour, then none.
	for i := range agentsession.MaxStoresPerHour - 4 {
		r, err := f.a.AdmitStore(t.Context(), "s1")
		if err != nil {
			t.Fatalf("store %d: %v", i+5, err)
		}
		r()
	}
	if _, err := f.a.AdmitStore(t.Context(), "s1"); !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("an eleventh store in the hour: %v", err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.a.AdmitStore(t.Context(), "s1"); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

// revokingLineage revokes the session the first time a caller's lineage is read, which falls
// between Authenticate's refresh and its Bind.
type revokingLineage struct {
	ports.ProcessLineage
	revoke func()
	once   bool
}

func (r *revokingLineage) Lineage(ctx context.Context, pid int) ([]ports.ProcessInfo, error) {
	if !r.once {
		r.once = true
		r.revoke()
	}
	return r.ProcessLineage.Lineage(ctx, pid)
}

func TestAgentBindRacingARevokeRefuses(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	f.a.Lineage = &revokingLineage{ProcessLineage: f.procs, revoke: func() {
		if _, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: id}); err != nil {
			t.Error(err)
		}
	}}
	if err := f.auth(t, token, 21); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a first call racing a revoke was served: %v", err)
	}
	if f.state(t, id) != agentsession.Revoked {
		t.Fatalf("state %s, want Revoked", f.state(t, id))
	}
}

func TestEndedAgentSessionHoldsNoSlotsAndStopsItsProxies(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}
	release, err := f.a.AdmitStore(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: id}); err != nil {
		t.Fatal(err)
	}
	f.waitStopped(t, id)
	release()
	if _, err := f.a.AdmitStore(t.Context(), id); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a store slot for an ended session: %v", err)
	}
	f.a.mu.Lock()
	n := len(f.a.stores)
	f.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d store entries kept for an ended session", n)
	}
	// A second revoke changes nothing and stops nothing again.
	if _, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0002", Id: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.stopped:
		t.Fatalf("a second revoke stopped %s again", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStepAfterTheEndIsIllegal(t *testing.T) {
	f := newAgentsFixture(t)
	id, _ := f.create(t, 0)
	if err := f.a.step(t.Context(), id, agentsession.Event{Kind: agentsession.Revoke}); err != nil {
		t.Fatal(err)
	}
	if err := f.a.step(t.Context(), id, agentsession.Event{Kind: agentsession.Bind}); !errors.Is(err, machine.ErrIllegal) {
		t.Fatalf("Bind after the end: %v", err)
	}
}

// cancellingStore cancels the caller's context just before it saves an agent session's event,
// as a person who gives up on berth agent session revoke would.
type cancellingStore struct {
	ports.Store
	cancel func()
}

func (c *cancellingStore) SaveTransition(ctx context.Context, rec ports.TransitionRecord) error {
	if strings.HasPrefix(rec.Entity, "agent/") {
		c.cancel()
	}
	return c.Store.SaveTransition(ctx, rec)
}

func TestCancelledRevokeStillStopsTheProxies(t *testing.T) {
	f := newAgentsFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.a = f.newAgents(t, &cancellingStore{Store: f.store, cancel: cancel})
	id, _ := f.create(t, 0)
	_, _ = f.a.RevokeAgentSession(ctx, &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: id})
	f.waitStopped(t, id)
	if f.state(t, id) != agentsession.Revoked {
		t.Fatalf("state %s, want Revoked", f.state(t, id))
	}
}

func TestExpiryWithNoCallsStopsTheProxies(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 60)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}
	if len(f.timers.ds) != 1 || f.timers.ds[0] != time.Minute {
		t.Fatalf("expiry timers %v, want one of 1m", f.timers.ds)
	}
	f.clock.Advance(time.Minute)
	f.timers.fire(0)
	f.waitStopped(t, id)
	if f.state(t, id) != agentsession.Expired {
		t.Fatalf("state %s, want Expired", f.state(t, id))
	}
}

func TestRootExitWithNoCallsStopsTheProxies(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 22); err != nil {
		t.Fatal(err)
	}
	f.exits.exitRoot(t)
	f.waitStopped(t, id)
	if f.state(t, id) != agentsession.Ended {
		t.Fatalf("state %s, want Ended", f.state(t, id))
	}
	// The expiry timer of the ended session is cancelled: firing it changes nothing.
	f.timers.fire(0)
	select {
	case got := <-f.stopped:
		t.Fatalf("a cancelled timer stopped %s", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBootStopsProxiesOfEndedSessionsAndRearmsTheRest(t *testing.T) {
	f := newAgentsFixture(t)
	ended, _ := f.create(t, 0)
	if _, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: ended}); err != nil {
		t.Fatal(err)
	}
	f.waitStopped(t, ended)
	live, token := f.create(t, 0)
	if err := f.auth(t, token, 21); err != nil {
		t.Fatal(err)
	}

	// berthd restarts.
	f.timers = &fakeTimers{}
	f.exits = &fakeExits{byPID: map[int]chan struct{}{}}
	f.a = f.newAgents(t, f.store)
	if err := f.a.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.waitStopped(t, ended)
	if len(f.timers.ds) != 1 {
		t.Fatalf("re-armed %d timers, want 1", len(f.timers.ds))
	}
	f.exits.exitRoot(t)
	f.waitStopped(t, live)
}

// failingStore refuses to save an agent session's event, which stops its actor.
type failingStore struct{ ports.Store }

func (s failingStore) SaveTransition(ctx context.Context, rec ports.TransitionRecord) error {
	if strings.HasPrefix(rec.Entity, "agent/") {
		return errors.New("disk full")
	}
	return s.Store.SaveTransition(ctx, rec)
}

func TestRevokeThatCannotBeSavedIsUnavailable(t *testing.T) {
	f := newAgentsFixture(t)
	f.a = f.newAgents(t, failingStore{f.store})
	id, _ := f.create(t, 0)
	_, err := f.a.RevokeAgentSession(t.Context(), &berthv1.RevokeAgentSessionRequest{RequestId: "revoke-0001", Id: id})
	if !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("a revoke that was not saved answered %v, want ErrUnavailable", err)
	}
	if f.state(t, id) != agentsession.Issued {
		t.Fatalf("state %s", f.state(t, id))
	}
}

func TestDefaultAfterFiresAndCancels(t *testing.T) {
	a := newAgents(t.Context(), agentsDeps{Log: slog.New(slog.DiscardHandler), Clock: testutil.NewClock()})
	fired := make(chan struct{})
	a.After(20*time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the default timer never fired")
	}
	cancelled := make(chan struct{})
	stop := a.After(20*time.Millisecond, func() { close(cancelled) })
	stop()
	select {
	case <-cancelled:
		t.Fatal("a cancelled timer fired")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFirstCallWhoseRootIsAlreadyGoneIsRefused(t *testing.T) {
	f := newAgentsFixture(t)
	f.exits.gone = map[int]bool{20: true}
	id, token := f.create(t, 0)
	if err := f.auth(t, token, 22); !errors.Is(err, api.ErrAgentTokenInvalid) {
		t.Fatalf("a call whose root had already exited: %v", err)
	}
	if f.state(t, id) != agentsession.Ended {
		t.Fatalf("state %s, want Ended", f.state(t, id))
	}
	f.waitStopped(t, id)
}

// A root that cannot be watched, as when berthd is out of descriptors, still runs: the session
// stays Bound, and the next call starts the watch.
func TestAgentRootWatchFailureIsNotAnExit(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	f.exits.mu.Lock()
	f.exits.err = syscall.EMFILE
	f.exits.mu.Unlock()
	if err := f.auth(t, token, 22); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if st := f.state(t, id); st != agentsession.Bound {
		t.Fatalf("a watch that could not start made the session %s", st)
	}
	f.exits.mu.Lock()
	f.exits.err = nil
	f.exits.mu.Unlock()
	if err := f.auth(t, token, 22); err != nil {
		t.Fatalf("second call: %v", err)
	}
	f.exits.exitRoot(t)
	f.waitStopped(t, id)
	if st := f.state(t, id); st != agentsession.Ended {
		t.Fatalf("after the root exited the session is %s", st)
	}
}

// A failed root watch is retried from a timer, so a root that dies with no further calls still
// ends the session.
func TestAgentRootWatchIsRetriedWithoutACall(t *testing.T) {
	f := newAgentsFixture(t)
	id, token := f.create(t, 0)
	f.exits.mu.Lock()
	f.exits.err = syscall.EMFILE
	f.exits.mu.Unlock()
	if err := f.auth(t, token, 22); err != nil {
		t.Fatalf("first call: %v", err)
	}
	f.exits.mu.Lock()
	f.exits.err = nil
	f.exits.mu.Unlock()
	f.timers.mu.Lock()
	retry := len(f.timers.fns) - 1
	d := f.timers.ds[retry]
	f.timers.mu.Unlock()
	if d != rootRetry {
		t.Fatalf("last timer waits %v, want the root retry %v", d, rootRetry)
	}
	f.timers.fire(retry)
	f.exits.exitRoot(t)
	f.waitStopped(t, id)
	if st := f.state(t, id); st != agentsession.Ended {
		t.Fatalf("after the root exited the session is %s", st)
	}
}
