package berthd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/codetrust"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/proxyproc"
	"berth/internal/testutil"
)

const (
	agentPID   = 100
	berthPID   = 200
	berth2PID  = 210
	helperPID  = 300
	helper2PID = 310
	otherPID   = 400
	proxyPID   = 500
	proxyHelp  = 510
	berthdPID  = 600
	daemonHelp = 610
	tool       = "/usr/local/bin/tool"
	toolReal   = "/opt/tool/bin/tool"
	helperExe  = "/Applications/Berth.app/Contents/MacOS/berth-native"
)

var (
	cliHash    = ports.CDHash{0xc1}
	helperHash = ports.CDHash{0xe1}
	proxyHash  = ports.CDHash{0xf1}
	signedLive = ports.CSValid | ports.CSRuntime
	toolCDHash = ports.CDHash{0x7c}
	shCDHash   = ports.CDHash{0x5c}
)

func peerOf(pid int) ports.Peer { return ports.Peer{PID: pid, AuditToken: [32]byte{byte(pid)}} }

// liveCode is a fake ports.CodeInspector: what the kernel would report per pid.
type liveCode struct {
	mu   sync.Mutex
	byID map[int]ports.LiveCode
}

func (l *liveCode) set(pid int, c ports.LiveCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID[pid] = c
}

func (l *liveCode) Live(_ context.Context, p ports.Peer) (ports.LiveCode, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.byID[p.PID]
	if !ok {
		return ports.LiveCode{}, errors.New("no such process")
	}
	return c, nil
}

type fixture struct {
	s        *secrets
	store    *sqlite.Store
	approver *testutil.Approver
	execs    *testutil.ExecIdentity
	clock    *testutil.Clock
	code     *liveCode
	exits    *exits
	pins     map[string]ports.CDHash
	pinsMu   sync.Mutex
}

// exits is a fake process exit watcher: a test closes a pid's channel to make it exit, sets
// refuse to make every watch fail, or sets watched to act just as the watch starts.
type exits struct {
	mu      sync.Mutex
	byID    map[int]chan struct{}
	refuse  error
	watched func(pid int)
}

func (e *exits) watch(_ context.Context, pid int) (<-chan struct{}, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.watched != nil {
		e.watched(pid)
	}
	if e.refuse != nil {
		return nil, e.refuse
	}
	if _, ok := e.byID[pid]; !ok {
		e.byID[pid] = make(chan struct{})
	}
	return e.byID[pid], nil
}

func (e *exits) exit(pid int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.byID[pid]; !ok {
		e.byID[pid] = make(chan struct{})
	}
	// exitAll may have ended it already, when a test is over before its fake process.
	select {
	case <-e.byID[pid]:
	default:
		close(e.byID[pid])
	}
}

// settle, registered before a fixture's other cleanups so it runs after them, lets the grace
// timers a test leaves pending fire in the bubble's fake time, so the goroutines waiting on them
// end before the bubble does. synctest.Wait panics outside a bubble, where the sleep would be real.
func settle(t *testing.T) {
	t.Cleanup(func() {
		synctest.Wait()
		time.Sleep(time.Minute)
	})
}

// exitAll ends every process still watched, as their exits would when a test is over, so no
// monitor outlives it.
func (e *exits) exitAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.byID {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func newFixture(t *testing.T, approver ports.Approver) *fixture {
	return newFixtureWith(t, approver, &testutil.TicketSource{})
}

func newFixtureWith(t *testing.T, approver ports.Approver, tickets ports.TicketSource) *fixture {
	t.Helper()
	settle(t)
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake, _ := approver.(*testutil.Approver)
	execs := testutil.NewExecIdentity()
	execs.Set(tool, ports.Executable{Path: toolReal, SHA256: [32]byte{1}, CDHash: toolCDHash})
	execs.Set(toolReal, ports.Executable{Path: toolReal, SHA256: [32]byte{1}, CDHash: toolCDHash})
	execs.Set("/bin/sh", ports.Executable{Path: "/bin/sh", SHA256: [32]byte{3}, CDHash: shCDHash})
	execs.Set("/opt/x/deploy.sh", ports.Executable{Path: "/opt/x/deploy.sh", SHA256: [32]byte{4}, Shebang: true, CDHash: shCDHash})
	peers := testutil.NewPeerFacts()
	peers.Set(ports.Requester{PID: agentPID, Executable: "/usr/local/bin/claude", WorkingDir: "/w"})
	peers.Set(ports.Requester{PID: berthPID, Executable: "/usr/local/bin/berth", Tree: []ports.Process{{PID: agentPID}}})
	peers.Set(ports.Requester{PID: berth2PID, Executable: "/usr/local/bin/berth", Tree: []ports.Process{{PID: agentPID}}})
	peers.Set(ports.Requester{PID: helperPID, Executable: helperExe, Tree: []ports.Process{{PID: berthPID}}})
	peers.Set(ports.Requester{PID: helper2PID, Executable: helperExe, Tree: []ports.Process{{PID: berth2PID}}})
	peers.Set(ports.Requester{PID: otherPID, Executable: helperExe, Tree: []ports.Process{{PID: agentPID}}})
	peers.Set(ports.Requester{PID: proxyPID, Executable: "/usr/local/bin/berth-proxy", Tree: []ports.Process{{PID: berthPID}}})
	peers.Set(ports.Requester{PID: proxyHelp, Executable: helperExe, Tree: []ports.Process{{PID: proxyPID}}})
	peers.Set(ports.Requester{PID: daemonHelp, Executable: helperExe, Tree: []ports.Process{{PID: berthdPID}}})
	code := &liveCode{byID: map[int]ports.LiveCode{
		agentPID:   {CDHash: ports.CDHash{0xaa}, Status: signedLive},
		berthPID:   {CDHash: cliHash, Status: signedLive},
		berth2PID:  {CDHash: cliHash, Status: signedLive},
		helperPID:  {CDHash: helperHash, Status: signedLive},
		helper2PID: {CDHash: helperHash, Status: signedLive},
		otherPID:   {CDHash: helperHash, Status: signedLive},
		proxyPID:   {CDHash: proxyHash, Status: signedLive},
		proxyHelp:  {CDHash: helperHash, Status: signedLive},
		daemonHelp: {CDHash: helperHash, Status: signedLive},
	}}
	clock := testutil.NewClock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &fixture{
		store: store, approver: fake, execs: execs, clock: clock, code: code, exits: &exits{byID: map[int]chan struct{}{}},
		pins: map[string]ports.CDHash{codetrust.CLISigningID: cliHash, codetrust.HelperSigningID: helperHash, codetrust.ProxySigningID: proxyHash},
	}
	t.Cleanup(f.exits.exitAll)
	pin := func(_ context.Context, _, identifier string) ([]ports.CDHash, bool, error) {
		f.pinsMu.Lock()
		defer f.pinsMu.Unlock()
		h, ok := f.pins[identifier]
		return []ports.CDHash{h}, ok, nil
	}
	log := slog.New(slog.DiscardHandler)
	trusted := func(identifier string) *codetrust.File {
		return codetrust.New(ctx, "/bin/"+identifier, identifier, pin, code, clock, log)
	}
	f.s = newSecrets(ctx, secretsDeps{
		Log: log, Store: store, Records: store, Sessions: store, Clock: clock,
		Approver: approver, Execs: execs, Peers: peers, Tickets: tickets,
		CLI: trusted(codetrust.CLISigningID), Helper: trusted(codetrust.HelperSigningID), Proxy: trusted(codetrust.ProxySigningID),
		PID: berthdPID, Exits: f.exits.watch, Changed: func() {},
		// Tests that start a proxy or end an agent session replace these.
		StartProxyProcess: func(context.Context) (*proxyproc.Process, error) {
			return nil, errors.New("no berth-proxy in this test")
		},
		AgentLive: func(context.Context, string) bool { return true },
	})
	return f
}

func (f *fixture) request(t *testing.T) berthv1.StoreOutcome {
	t.Helper()
	resp, err := f.s.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{Name: "gh-token", Reason: "deploy"}, peerOf(agentPID))
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetOutcome()
}

func (f *fixture) grant(t *testing.T, glob string) *berthv1.GrantSecretResponse {
	t.Helper()
	resp, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: tool, ArgsGlob: glob}, peerOf(berthPID))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) use(t *testing.T, argv ...string) *berthv1.AuthorizeUseResponse {
	t.Helper()
	resp, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool, Argv: argv}, peerOf(berthPID))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) history(t *testing.T, entity string) []ports.TransitionRecord {
	t.Helper()
	h, err := f.store.History(t.Context(), entity, 50)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func ticketOf(resp *berthv1.AuthorizeUseResponse) ports.Ticket {
	var t ports.Ticket
	copy(t[:], resp.GetTicket())
	return t
}

func TestStoreGrantUseAndRedeem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
			t.Fatalf("store outcome %v", got)
		}
		prompts := f.approver.StorePrompts()
		if len(prompts) != 1 || prompts[0].Requester.Executable != "/usr/local/bin/claude" || prompts[0].Reason != "deploy" {
			t.Fatalf("store prompts %+v", prompts)
		}
		if _, err := f.s.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{Name: "gh-token", Reason: "again"}, peerOf(agentPID)); !errors.Is(err, api.ErrSecretExists) {
			t.Fatalf("second store: %v", err)
		}

		g := f.grant(t, "")
		if g.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED || g.GetConsumer().GetExePath() != toolReal {
			t.Fatalf("grant %v", g)
		}
		if p := f.approver.PowerPrompts(); len(p) != 1 || p[0].Consumer.Path != toolReal || p[0].Requester.PID != berthPID {
			t.Fatalf("power prompts %+v", p)
		}

		use := f.use(t, "deploy", "--prod")
		if use.GetOutcome() != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED || len(use.GetTicket()) != 32 ||
			!use.GetExpiresAt().AsTime().Equal(f.clock.Now().Add(ticketLife)) {
			t.Fatalf("use %v", use)
		}
		ticket := ticketOf(use)
		if err := f.s.TrustRedeemer(t.Context(), peerOf(helperPID)); err != nil {
			t.Fatalf("helper not trusted: %v", err)
		}
		redeemed, err := f.s.RedeemUseTicket(t.Context(), ticket, peerOf(helperPID))
		if err != nil || redeemed.GetName() != "gh-token" ||
			redeemed.GetConsumerKind() != berthv1.ConsumerKind_CONSUMER_KIND_EXECUTABLE ||
			redeemed.GetAction() != berthv1.TicketAction_TICKET_ACTION_USE {
			t.Fatalf("redeem %v, %v", redeemed, err)
		}
		if _, err := f.s.RedeemUseTicket(t.Context(), ticket, peerOf(helperPID)); !errors.Is(err, api.ErrTicketInvalid) {
			t.Fatalf("replayed ticket: %v", err)
		}

		list, err := f.s.ListSecrets(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		sec := list.GetSecrets()
		if len(sec) != 1 || sec[0].GetState() != berthv1.SecretState_SECRET_STATE_STORED || len(sec[0].GetConsumers()) != 1 {
			t.Fatalf("list %v", list)
		}
		c := sec[0].GetConsumers()[0]
		if c.GetUseCount() != 1 || c.GetLastUsedAt() == nil || c.GetState() != berthv1.GrantState_GRANT_STATE_ACTIVE {
			t.Fatalf("consumer %v", c)
		}
	})
}

func TestTicketChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grant(t, "")

		// A helper berth did not start (its parent is the agent) cannot spend berth's ticket.
		if _, err := f.s.RedeemUseTicket(t.Context(), ticketOf(f.use(t)), peerOf(otherPID)); !errors.Is(err, api.ErrTicketInvalid) {
			t.Fatalf("foreign helper: %v", err)
		}
		expired := ticketOf(f.use(t))
		f.clock.Advance(ticketLife + time.Second)
		if _, err := f.s.RedeemUseTicket(t.Context(), expired, peerOf(helperPID)); !errors.Is(err, api.ErrTicketInvalid) {
			t.Fatalf("expired ticket: %v", err)
		}
		if _, err := f.s.RedeemUseTicket(t.Context(), ports.Ticket{0xee}, peerOf(helperPID)); !errors.Is(err, api.ErrTicketInvalid) {
			t.Fatalf("unknown ticket: %v", err)
		}
		for _, pid := range []int{0, agentPID, berthPID, 999} {
			if err := f.s.TrustRedeemer(t.Context(), peerOf(pid)); !errors.Is(err, api.ErrPeerNotTrusted) {
				t.Fatalf("peer %d: %v", pid, err)
			}
		}
	})
}

func TestHashChangeMakesTheGrantStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		id := f.grant(t, "").GetConsumer().GetGrantId()
		f.execs.Set(tool, ports.Executable{Path: toolReal, SHA256: [32]byte{2}, CDHash: toolCDHash})
		for range 2 {
			if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT {
				t.Fatalf("outcome %v", got)
			}
		}
		h := f.history(t, grant.Entity(id))
		if h[1].Event != "HashChanged" || h[1].To != "Stale" || h[0].Event != "UseRefused" {
			t.Fatalf("grant history %+v", h)
		}
		// A new grant for the new hash works while the stale one stays stale.
		f.grant(t, "")
		if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
			t.Fatalf("after regrant %v", got)
		}
	})
}

// TestGrantPinsTheCDHash: berth run compares the suspended child's live cdhash with the one
// AuthorizeUse returns, which berthd read when the person granted the executable.
func TestGrantPinsTheCDHash(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grant(t, "")
		if got := f.use(t).GetCdhash(); string(got) != string(toolCDHash[:]) {
			t.Fatalf("cdhash %x, want %x", got, toolCDHash)
		}
		// The file hashes the same but the kernel would run another image, as when a script's
		// interpreter is replaced: the grant goes stale.
		f.execs.Set(tool, ports.Executable{Path: toolReal, SHA256: [32]byte{1}, CDHash: ports.CDHash{0x99}})
		if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT {
			t.Fatalf("outcome %v", got)
		}
		// With no cdhash to pin, berth run could never start the consumer, so there is no grant.
		f.execs.Set("/opt/x/unsigned", ports.Executable{Path: "/opt/x/unsigned", SHA256: [32]byte{5}})
		if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: "/opt/x/unsigned"}, peerOf(berthPID)); !errors.Is(err, api.ErrExecutableNotFound) {
			t.Fatalf("grant without a cdhash: %v", err)
		}
		// A script pins its interpreter's cdhash, which is what the kernel reports for it.
		if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: "/opt/x/deploy.sh", AllowInterpreter: true}, peerOf(berthPID)); err != nil {
			t.Fatal(err)
		}
		resp, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: "/opt/x/deploy.sh"}, peerOf(berthPID))
		if err != nil || string(resp.GetCdhash()) != string(shCDHash[:]) {
			t.Fatalf("script use %v %v", resp, err)
		}
	})
}

func TestRefusals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		if _, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "missing", ExePath: tool}, peerOf(berthPID)); !errors.Is(err, api.ErrSecretNotFound) {
			t.Fatalf("missing secret: %v", err)
		}
		if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "missing", ExePath: tool}, peerOf(berthPID)); !errors.Is(err, api.ErrSecretNotFound) {
			t.Fatalf("grant missing secret: %v", err)
		}
		f.request(t)
		if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: "/nope"}, peerOf(berthPID)); !errors.Is(err, api.ErrExecutableNotFound) {
			t.Fatalf("missing executable: %v", err)
		}
		if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: tool, ArgsGlob: "["}, peerOf(berthPID)); !errors.Is(err, api.ErrBadGlob) {
			t.Fatalf("bad glob: %v", err)
		}
		if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
			t.Fatalf("no grant: %v", got)
		}
		f.grant(t, "deploy *")
		if got := f.use(t, "deploy", "--prod").GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
			t.Fatalf("matching args: %v", got)
		}
		if got := f.use(t, "destroy").GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
			t.Fatalf("other args: %v", got)
		}
		h := f.history(t, "secret/gh-token")
		if h[0].Event != "UseRefused" || h[0].Detail != "arguments do not match any grant for "+toolReal ||
			h[1].Event != "UseRefused" || h[1].Detail != "no grant for "+toolReal {
			t.Fatalf("secret history %+v", h)
		}
	})
}

func TestDeniedGrantRefusesUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		approver := &testutil.Approver{}
		f := newFixture(t, approver)
		f.request(t)
		approver.QueuePower(ports.PowerDenied)
		g := f.grant(t, "")
		if g.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_DENIED || g.GetConsumer() != nil {
			t.Fatalf("grant %v", g)
		}
		if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_NO_GRANT {
			t.Fatalf("use %v", got)
		}
		if h := f.history(t, "grant/1"); h[0].Event != "UseRefused" || h[0].To != "Denied" {
			t.Fatalf("grant history %+v", h)
		}
		approver.QueuePower(ports.PowerFailed)
		if g := f.grant(t, ""); g.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_FAILED {
			t.Fatalf("failed grant %v", g)
		}
	})
}

func TestDeniedStoreCanBeRequestedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		approver := &testutil.Approver{}
		f := newFixture(t, approver)
		approver.QueueStore(ports.StoreDenied, ports.StoreFailed)
		if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_DENIED {
			t.Fatalf("first %v", got)
		}
		if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_FAILED {
			t.Fatalf("second %v", got)
		}
		if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
			t.Fatalf("third %v", got)
		}
	})
}

// gatedApprover holds every store dialog open until the test releases it.
type gatedApprover struct {
	testutil.Approver
	mu      sync.Mutex
	opened  int
	entered chan struct{}
	release chan struct{}
}

func (g *gatedApprover) StoreSecret(ctx context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	g.mu.Lock()
	g.opened++
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	return g.Approver.StoreSecret(ctx, p)
}

func TestOneStoreDialogPerName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &gatedApprover{entered: make(chan struct{}, 4), release: make(chan struct{})}
		f := newFixture(t, gate)
		results := make(chan berthv1.StoreOutcome, 2)
		go func() { results <- f.request(t) }()
		<-gate.entered
		go func() { results <- f.request(t) }()
		// Let the second caller join; it must not open a dialog of its own.
		synctest.Wait()
		close(gate.release)
		for range 2 {
			if got := <-results; got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
				t.Fatalf("outcome %v", got)
			}
		}
		if gate.opened != 1 {
			t.Fatalf("%d dialogs opened, want 1", gate.opened)
		}
	})
}

// TestOnlyTheSignedCLIGetsTickets is the store-and-use-never-read rule: a process an agent
// wrote can start berth-native itself, so it must never hold a ticket.
func TestOnlyTheSignedCLIGetsTickets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grant(t, "")
		req := &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool}
		if _, err := f.s.AuthorizeUse(t.Context(), req, peerOf(agentPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("agent process: %v", err)
		}
		// berth-native is trusted to redeem, not to be issued tickets.
		if _, err := f.s.AuthorizeUse(t.Context(), req, peerOf(helperPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("helper as caller: %v", err)
		}
		f.code.set(berthPID, ports.LiveCode{CDHash: cliHash, Status: ports.CSValid})
		if _, err := f.s.AuthorizeUse(t.Context(), req, peerOf(berthPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("berth without the hardened runtime: %v", err)
		}
		f.code.set(berthPID, ports.LiveCode{CDHash: cliHash, Status: ports.CSRuntime})
		if _, err := f.s.AuthorizeUse(t.Context(), req, peerOf(berthPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("berth with an invalid signature: %v", err)
		}
		if _, err := f.s.AuthorizeUse(t.Context(), req, ports.Peer{PID: 999}); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("unknown process: %v", err)
		}
		if h := f.history(t, "grant/1"); len(h) != 2 {
			t.Fatalf("a refused caller touched the grant: %+v", h)
		}
	})
}

func TestRebuiltCLIIsPinnedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		f.grant(t, "")
		rebuilt := ports.CDHash{0xc2}
		f.code.set(berthPID, ports.LiveCode{CDHash: rebuilt, Status: signedLive})
		if _, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool}, peerOf(berthPID)); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("before the file changed: %v", err)
		}
		f.pinsMu.Lock()
		f.pins[codetrust.CLISigningID] = rebuilt
		f.pinsMu.Unlock()
		f.clock.Advance(codetrust.RepinInterval)
		if got := f.use(t).GetOutcome(); got != berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
			t.Fatalf("after make build: %v", got)
		}
	})
}

func TestInterpreterGrantsNeedTheFlag(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		for _, exe := range []string{"/bin/sh", "/opt/x/deploy.sh"} {
			req := &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: exe}
			if _, err := f.s.GrantSecret(t.Context(), req, peerOf(berthPID)); !errors.Is(err, api.ErrInterpreter) {
				t.Fatalf("%s without the flag: %v", exe, err)
			}
		}
		if len(f.approver.PowerPrompts()) != 0 {
			t.Fatal("a refused interpreter grant prompted")
		}
		resp, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: "/bin/sh", AllowInterpreter: true}, peerOf(berthPID))
		if err != nil || resp.GetOutcome() != berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED {
			t.Fatalf("with the flag: %v, %v", resp, err)
		}
		if p := f.approver.PowerPrompts(); len(p) != 1 || !p[0].Interpreter {
			t.Fatalf("prompt did not warn about the interpreter: %+v", p)
		}
		f.grant(t, "")
		if p := f.approver.PowerPrompts(); p[1].Interpreter {
			t.Fatalf("a compiled tool was flagged as an interpreter: %+v", p[1])
		}
	})
}

func TestIsInterpreter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for path, want := range map[string]bool{
			"/bin/zsh": true, "/usr/bin/python3.12": true, "/usr/bin/env": true, "/opt/homebrew/bin/node": true,
			"/usr/bin/osascript": true, "/usr/bin/git": true, "/usr/bin/ssh": true, "/usr/bin/awk": true, "/usr/bin/find": true,
			"/usr/local/bin/gh": false, "/usr/bin/curl": false,
		} {
			if got := isInterpreter(ports.Executable{Path: path}); got != want {
				t.Errorf("%s: %v", path, got)
			}
		}
	})
}

// slowTickets makes every ticket take a moment, so later uses queue up behind an earlier one.
type slowTickets struct{ testutil.TicketSource }

func (s *slowTickets) NewTicket(ctx context.Context) (ports.Ticket, error) {
	time.Sleep(30 * time.Millisecond)
	return s.TicketSource.NewTicket(ctx)
}

// TestConcurrentUsesKeepTheirOwnTickets queues two callers behind a use already in its effect,
// the moment where data carried outside the entity's turn gets swapped or lost.
func TestConcurrentUsesKeepTheirOwnTickets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixtureWith(t, &testutil.Approver{}, &slowTickets{})
		f.request(t)
		f.grant(t, "")
		type got struct {
			caller int
			ticket ports.Ticket
			err    error
		}
		results := make(chan got, 3)
		use := func(pid int) {
			resp, err := f.s.AuthorizeUse(context.Background(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool}, peerOf(pid))
			results <- got{pid, ticketOf(resp), err}
		}
		go use(berthPID)
		synctest.Wait()
		go use(berthPID)
		go use(berth2PID)
		helperOf := map[int]int{berthPID: helperPID, berth2PID: helper2PID}
		seen := map[ports.Ticket]bool{}
		for range 3 {
			r := <-results
			if r.err != nil {
				t.Fatalf("use by %d: %v", r.caller, r.err)
			}
			if seen[r.ticket] {
				t.Fatal("two callers got the same ticket")
			}
			seen[r.ticket] = true
			// Each ticket is bound to its own caller, so only that caller's helper can redeem it.
			if _, err := f.s.RedeemUseTicket(t.Context(), r.ticket, peerOf(helperOf[r.caller])); err != nil {
				t.Fatalf("ticket for %d did not redeem in its own helper: %v", r.caller, err)
			}
		}
	})
}

func TestCancelledUseLeavesNothingForTheNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixtureWith(t, &testutil.Approver{}, &slowTickets{})
		f.request(t)
		f.grant(t, "")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
		defer cancel()
		if _, err := f.s.AuthorizeUse(ctx, &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool}, peerOf(berthPID)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled use: %v", err)
		}
		resp, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool}, peerOf(berth2PID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.RedeemUseTicket(t.Context(), ticketOf(resp), peerOf(helper2PID)); err != nil {
			t.Fatalf("the second caller got a ticket bound to someone else: %v", err)
		}
	})
}

// waitingApprover keeps every prompt open until its context ends, like a person away from the Mac.
type waitingApprover struct {
	testutil.Approver
	opened chan struct{}
}

func (w *waitingApprover) StoreSecret(ctx context.Context, _ ports.StorePrompt) (ports.StoreOutcome, error) {
	w.opened <- struct{}{}
	<-ctx.Done()
	return ports.StoreFailed, ctx.Err()
}

func TestStopEndsOpenStorePrompts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		approver := &waitingApprover{opened: make(chan struct{}, 1)}
		f := newFixture(t, approver)
		done := make(chan berthv1.StoreOutcome, 1)
		go func() { done <- f.request(t) }()
		<-approver.opened
		if !f.s.stop(2 * time.Second) {
			t.Fatal("stop timed out")
		}
		// stop waits for the step, which ends as the caller is handed the outcome.
		select {
		case got := <-done:
			if got != berthv1.StoreOutcome_STORE_OUTCOME_FAILED {
				t.Fatalf("outcome %v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("the caller never had its answer")
		}
		h := f.history(t, "secret/gh-token")
		if h[0].Event != "StoreFailed" || h[0].To != "Failed" || h[0].Detail != "berthd stopped while the prompt was open" {
			t.Fatalf("history %+v", h)
		}
	})
}

func TestBootEndsStoresAPreviousRunLeftPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		ctx := t.Context()
		at := f.clock.Now()
		for _, name := range []string{"left-open", "kept"} {
			if err := f.store.AddSecret(ctx, name, at); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SaveTransition(ctx, ports.TransitionRecord{
				Entity: "secret/" + name, Kind: "secret", Version: 1,
				Event: "StoreRequested", From: "Pending", To: "Pending", At: at,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.store.SaveTransition(ctx, ports.TransitionRecord{
			Entity: "secret/kept", Kind: "secret", Version: 2,
			Event: "StoreSucceeded", From: "Pending", To: "Stored", At: at,
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.s.recover(ctx); err != nil {
			t.Fatal(err)
		}
		if h := f.history(t, "secret/left-open"); h[0].To != "Failed" || h[0].Detail != "interrupted" {
			t.Fatalf("pending secret %+v", h)
		}
		if h := f.history(t, "secret/kept"); len(h) != 2 || h[0].To != "Stored" {
			t.Fatalf("stored secret was touched: %+v", h)
		}
	})
}

func TestPassEnvIsStoredPromptedAndReturnedWithoutLoaderVariables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, &testutil.Approver{})
		f.request(t)
		resp, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{
			Name: "gh-token", ExePath: tool, PassEnv: []string{"GH_HOST", "NODE_OPTIONS", "DYLD_INSERT_LIBRARIES"},
		}, peerOf(berthPID))
		if err != nil || strings.Join(resp.GetConsumer().GetPassEnv(), ",") != "GH_HOST" {
			t.Fatalf("grant %v, %v", resp, err)
		}
		if p := f.approver.PowerPrompts(); strings.Join(p[0].PassEnv, ",") != "GH_HOST" {
			t.Fatalf("prompt listed %v", p[0].PassEnv)
		}
		if got := f.use(t).GetPassEnv(); strings.Join(got, ",") != "GH_HOST" {
			t.Fatalf("authorize returned %v", got)
		}
	})
}
