package berthd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/adapters/sqlite"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

const (
	agentPID  = 100
	berthPID  = 200
	helperPID = 300
	otherPID  = 400
	tool      = "/usr/local/bin/tool"
	toolReal  = "/opt/tool/bin/tool"
	helperExe = "/Applications/Berth.app/Contents/MacOS/berth-native"
)

type fixture struct {
	s        *secrets
	store    *sqlite.Store
	approver *testutil.Approver
	execs    *testutil.ExecIdentity
	clock    *testutil.Clock
}

func newFixture(t *testing.T, approver ports.Approver) *fixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake, _ := approver.(*testutil.Approver)
	execs := testutil.NewExecIdentity()
	execs.Set(tool, ports.Executable{Path: toolReal, SHA256: [32]byte{1}})
	execs.Set(toolReal, ports.Executable{Path: toolReal, SHA256: [32]byte{1}})
	execs.Set(helperExe, ports.Executable{Path: helperExe, SHA256: [32]byte{9}, TeamID: HelperTeamID, SigningID: HelperSigningID})
	peers := testutil.NewPeerFacts()
	peers.Set(ports.Requester{PID: agentPID, Executable: "/usr/local/bin/claude", WorkingDir: "/w"})
	peers.Set(ports.Requester{PID: berthPID, Executable: "/usr/local/bin/berth", Tree: []ports.Process{{PID: agentPID}}})
	peers.Set(ports.Requester{PID: helperPID, Executable: helperExe, Tree: []ports.Process{{PID: berthPID}}})
	peers.Set(ports.Requester{PID: otherPID, Executable: helperExe, Tree: []ports.Process{{PID: agentPID}}})
	clock := testutil.NewClock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newSecrets(ctx, secretsDeps{
		Log: slog.New(slog.DiscardHandler), Store: store, Records: store, Clock: clock,
		Approver: approver, Execs: execs, Peers: peers, Tickets: &testutil.TicketSource{},
		Trust: func(_ context.Context, e ports.Executable) (bool, error) { return e.TeamID == HelperTeamID, nil },
	})
	return &fixture{s: s, store: store, approver: fake, execs: execs, clock: clock}
}

func (f *fixture) request(t *testing.T) berthv1.StoreOutcome {
	t.Helper()
	resp, err := f.s.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{Name: "gh-token", Reason: "deploy"}, agentPID)
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetOutcome()
}

func (f *fixture) grant(t *testing.T, glob string) *berthv1.GrantSecretResponse {
	t.Helper()
	resp, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: tool, ArgsGlob: glob}, berthPID)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) use(t *testing.T, argv ...string) *berthv1.AuthorizeUseResponse {
	t.Helper()
	resp, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "gh-token", ExePath: tool, Argv: argv}, berthPID)
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
	f := newFixture(t, &testutil.Approver{})
	if got := f.request(t); got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
		t.Fatalf("store outcome %v", got)
	}
	prompts := f.approver.StorePrompts()
	if len(prompts) != 1 || prompts[0].Requester.Executable != "/usr/local/bin/claude" || prompts[0].Reason != "deploy" {
		t.Fatalf("store prompts %+v", prompts)
	}
	if _, err := f.s.RequestSecret(t.Context(), &berthv1.RequestSecretRequest{Name: "gh-token", Reason: "again"}, agentPID); !errors.Is(err, api.ErrSecretExists) {
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
	if err := f.s.TrustRedeemer(t.Context(), helperPID); err != nil {
		t.Fatalf("helper not trusted: %v", err)
	}
	redeemed, err := f.s.RedeemUseTicket(t.Context(), ticket, helperPID)
	if err != nil || redeemed.GetName() != "gh-token" {
		t.Fatalf("redeem %v, %v", redeemed, err)
	}
	if _, err := f.s.RedeemUseTicket(t.Context(), ticket, helperPID); !errors.Is(err, api.ErrTicketInvalid) {
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
}

func TestTicketChecks(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	f.grant(t, "")

	// A helper berth did not start (its parent is the agent) cannot spend berth's ticket.
	if _, err := f.s.RedeemUseTicket(t.Context(), ticketOf(f.use(t)), otherPID); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("foreign helper: %v", err)
	}
	expired := ticketOf(f.use(t))
	f.clock.Advance(ticketLife + time.Second)
	if _, err := f.s.RedeemUseTicket(t.Context(), expired, helperPID); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("expired ticket: %v", err)
	}
	if _, err := f.s.RedeemUseTicket(t.Context(), ports.Ticket{0xee}, helperPID); !errors.Is(err, api.ErrTicketInvalid) {
		t.Fatalf("unknown ticket: %v", err)
	}
	for _, pid := range []int{0, agentPID, 999} {
		if err := f.s.TrustRedeemer(t.Context(), pid); !errors.Is(err, api.ErrPeerNotTrusted) {
			t.Fatalf("peer %d: %v", pid, err)
		}
	}
}

func TestHashChangeMakesTheGrantStale(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	f.request(t)
	id := f.grant(t, "").GetConsumer().GetGrantId()
	f.execs.Set(tool, ports.Executable{Path: toolReal, SHA256: [32]byte{2}})
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
}

func TestRefusals(t *testing.T) {
	f := newFixture(t, &testutil.Approver{})
	if _, err := f.s.AuthorizeUse(t.Context(), &berthv1.AuthorizeUseRequest{Name: "missing", ExePath: tool}, berthPID); !errors.Is(err, api.ErrSecretNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "missing", ExePath: tool}, berthPID); !errors.Is(err, api.ErrSecretNotFound) {
		t.Fatalf("grant missing secret: %v", err)
	}
	f.request(t)
	if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: "/nope"}, berthPID); !errors.Is(err, api.ErrExecutableNotFound) {
		t.Fatalf("missing executable: %v", err)
	}
	if _, err := f.s.GrantSecret(t.Context(), &berthv1.GrantSecretRequest{Name: "gh-token", ExePath: tool, ArgsGlob: "["}, berthPID); !errors.Is(err, api.ErrBadGlob) {
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
}

func TestDeniedGrantRefusesUse(t *testing.T) {
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
}

func TestDeniedStoreCanBeRequestedAgain(t *testing.T) {
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
	gate := &gatedApprover{entered: make(chan struct{}, 4), release: make(chan struct{})}
	f := newFixture(t, gate)
	results := make(chan berthv1.StoreOutcome, 2)
	go func() { results <- f.request(t) }()
	<-gate.entered
	go func() { results <- f.request(t) }()
	// Give the second caller time to join; it must not open a dialog of its own.
	time.Sleep(100 * time.Millisecond)
	close(gate.release)
	for range 2 {
		if got := <-results; got != berthv1.StoreOutcome_STORE_OUTCOME_STORED {
			t.Fatalf("outcome %v", got)
		}
	}
	if gate.opened != 1 {
		t.Fatalf("%d dialogs opened, want 1", gate.opened)
	}
}

type satisfier struct {
	calls int
	ok    bool
}

func (s *satisfier) Satisfies(_ context.Context, _, requirement string) (bool, error) {
	s.calls++
	return s.ok && requirement == helperRequirement, nil
}

func TestSignedHelperNeedsTeamIdentifierAndRequirement(t *testing.T) {
	signed := ports.Executable{Path: helperExe, TeamID: HelperTeamID, SigningID: HelperSigningID}
	cases := []struct {
		name string
		exe  ports.Executable
		req  bool
		want bool
	}{
		{"signed and satisfies", signed, true, true},
		{"signed but fails the requirement", signed, false, false},
		{"other team", ports.Executable{Path: helperExe, TeamID: "OTHER", SigningID: HelperSigningID}, true, false},
		{"other identifier", ports.Executable{Path: helperExe, TeamID: HelperTeamID, SigningID: "x"}, true, false},
		{"unsigned", ports.Executable{Path: helperExe}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sat := &satisfier{ok: tc.req}
			got, err := SignedHelper(sat)(t.Context(), tc.exe)
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}
