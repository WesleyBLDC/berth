package berthd

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"berth/internal/adapters/api"
	"berth/internal/codetrust"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/core/proxysession"
	"berth/internal/core/secret"
	"berth/internal/proxyproc"
)

const (
	// ticketLife is how long a use ticket stays redeemable.
	ticketLife = 30 * time.Second
	// promptTimeout bounds a dialog or Touch ID prompt the person walked away from.
	promptTimeout = 5 * time.Minute
	// noteSecretRemoved ends a grant whose secret was removed before it became Active.
	noteSecretRemoved = "secret removed"
)

type secretsDeps struct {
	Log      *slog.Logger
	Store    ports.Store
	Records  ports.SecretRecords
	Clock    ports.Clock
	Approver ports.Approver
	Execs    ports.ExecIdentity
	Peers    ports.PeerFacts
	Tickets  ports.TicketSource
	Sessions ports.ProxySessionRecords
	// CLI, Helper and Proxy are the signed berth, berth-native and berth-proxy beside berthd.
	CLI    *codetrust.File
	Helper *codetrust.File
	Proxy  *codetrust.File
	// PID is berthd's own; rotate and remove tickets are bound to it, since berthd starts the
	// helper that redeems them.
	PID int
	// Exits reports when a process exits (kqueue NOTE_EXIT in berthd).
	Exits func(ctx context.Context, pid int) (<-chan struct{}, error)
	// AllowLocalTargets accepts localhost and IP literal proxy grants; only berth_e2e builds.
	AllowLocalTargets bool
	// StartProxyProcess starts the berth-proxy beside berthd for StartProxy.
	StartProxyProcess func(ctx context.Context) (*proxyproc.Process, error)
	// AgentLive reports whether an agent session still serves calls. berthd sets it once both
	// secrets and agents exist, before serving.
	AgentLive func(ctx context.Context, id string) bool
	// Changed is told as soon as each secret or grant transition is saved, before a
	// prompt it opens is answered; it must not block.
	Changed func()
}

// secrets runs one actor per secret and per grant and performs their effects. It implements
// api.Secrets; every decision about states is in the core machines.
type secrets struct {
	secretsDeps
	// ctx outlives any one call, so a dialog keeps going when the caller that opened it leaves.
	ctx context.Context
	// stopping closes when the daemon stops, which cancels every open prompt.
	stopping chan struct{}
	stopOnce sync.Once
	secrets  *actors[secret.State, secret.Event, secret.Effect]
	grants   *actors[grant.State, grant.Event, grant.Effect]
	sessions *actors[proxysession.State, proxysession.Event, proxysession.Effect]
	mu       sync.Mutex
	flights  map[string]*flight
	live     map[ports.Ticket]liveTicket
	running  map[string]*proxySession
	// started maps the pid of each berth-proxy berthd started to the call that started it and
	// its agent session ("" for the owner); starting and startTimes count each agent session's
	// running starts and its starts in the last hour, for its limits. All are in memory.
	started    map[int]*startRecord
	starting   map[string]int
	startTimes map[string][]time.Time
}

var _ api.Secrets = (*secrets)(nil)

func newSecrets(ctx context.Context, d secretsDeps) *secrets {
	s := &secrets{
		secretsDeps: d, ctx: ctx, stopping: make(chan struct{}),
		flights: map[string]*flight{}, live: map[ports.Ticket]liveTicket{}, running: map[string]*proxySession{},
		started: map[int]*startRecord{}, starting: map[string]int{}, startTimes: map[string][]time.Time{},
	}
	s.secrets = &actors[secret.State, secret.Event, secret.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: secret.Kind, initial: secret.Initial, transition: secret.Transition, run: s.runSecret,
		final:     []secret.State{secret.Denied, secret.Failed, secret.Removed},
		live:      map[string]*slot[secret.State, secret.Event, secret.Effect]{},
		persisted: func(ports.TransitionRecord) { s.Changed() },
	}
	s.grants = &actors[grant.State, grant.Event, grant.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: grant.Kind, initial: grant.Initial, transition: grant.Transition, run: s.runGrant,
		final:     []grant.State{grant.Denied, grant.Stale, grant.Revoked},
		live:      map[string]*slot[grant.State, grant.Event, grant.Effect]{},
		persisted: func(ports.TransitionRecord) { s.Changed() },
	}
	s.sessions = &actors[proxysession.State, proxysession.Event, proxysession.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: proxysession.Kind, initial: proxysession.Initial, transition: proxysession.Transition, run: s.runSession,
		final: []proxysession.State{proxysession.Ended, proxysession.Lost},
		live:  map[string]*slot[proxysession.State, proxysession.Event, proxysession.Effect]{},
	}
	return s
}

// recover ends what a previous run left open: proxy sessions become Lost, since their proxies
// lost their watch streams with it; a dialog that died with it fails as "interrupted"; and the
// grants of a removed secret are revoked, in case the run ended before it revoked them all.
func (s *secrets) recover(ctx context.Context) error {
	if err := s.recoverSessions(ctx); err != nil {
		return err
	}
	rows, err := s.Records.Secrets(ctx)
	if err != nil {
		return err
	}
	interrupted := map[secret.State]secret.EventKind{
		secret.Pending:  secret.StoreFailed,
		secret.Rotating: secret.RotateFailed,
		secret.Removing: secret.RemoveFailed,
	}
	for _, r := range rows {
		entity := secret.Entity(r.Name)
		st, err := s.state(ctx, entity, "")
		if err != nil {
			return err
		}
		if secret.State(st) == secret.Removed {
			s.revokeAll(ctx, r.Name, proxysession.StopSecretRemoved)
			continue
		}
		kind, ok := interrupted[secret.State(st)]
		if !ok {
			continue
		}
		if _, _, err := step(ctx, s.secrets, entity, secret.Event{Kind: kind, Note: "interrupted"}, nil, kind); err != nil {
			return fmt.Errorf("end interrupted %s of %s: %w", st, r.Name, err)
		}
	}
	return nil
}

// stop tells every running proxy session to stop and gives the proxies a moment to end their
// sessions, then cancels open prompts, which kills their helper processes, and waits up to
// timeout in all for each to record its ending, so no store is left Pending by a clean stop.
func (s *secrets) stop(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	sessionsDone := s.stopAllSessions(min(timeout, sessionStopWait))
	s.stopOnce.Do(func() { close(s.stopping) })
	secretsDone := s.secrets.wait(max(time.Until(deadline), 0))
	grantsDone := s.grants.wait(max(time.Until(deadline), 0))
	return s.sessions.wait(max(time.Until(deadline), 0)) && grantsDone && secretsDone && sessionsDone
}

func (s *secrets) stopped() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// cancelOnStop ends a prompt's context when the daemon stops, or returns when the prompt ends.
func (s *secrets) cancelOnStop(ctx context.Context, cancel context.CancelFunc) {
	select {
	case <-s.stopping:
		cancel()
	case <-ctx.Done():
	}
}

// promptNote is the audit note for a prompt that did not succeed.
func (s *secrets) promptNote() string {
	if s.stopped() {
		return "berthd stopped while the prompt was open"
	}
	return "the helper reported a failure"
}

func (s *secrets) facts(ctx context.Context, peer ports.Peer) (ports.Requester, error) {
	if peer.PID <= 0 {
		return ports.Requester{}, api.ErrCallerUnknown
	}
	r, err := s.Peers.Facts(ctx, peer.PID)
	if err != nil {
		s.Log.Warn("cannot describe the caller", "pid", peer.PID, "err", err)
		return ports.Requester{}, api.ErrCallerUnknown
	}
	return r, nil
}

func (s *secrets) identify(ctx context.Context, exePath string) (ports.Executable, error) {
	exe, err := s.Execs.Identify(ctx, exePath)
	if err != nil {
		s.Log.Info("cannot identify executable", "path", exePath, "err", err)
		return ports.Executable{}, api.ErrExecutableNotFound
	}
	return exe, nil
}

// requireStored answers BUSY while a dialog is open on the secret and SECRET_NOT_FOUND when it
// holds no value, so a grant, rotate or remove starts only on a Stored secret.
func (s *secrets) requireStored(ctx context.Context, name string) error {
	st, err := s.state(ctx, secret.Entity(name), "")
	if err != nil {
		return err
	}
	switch secret.State(st) {
	case secret.Stored:
		return nil
	case secret.Pending, secret.Rotating, secret.Removing:
		return api.ErrBusy
	case secret.Denied, secret.Failed, secret.Removed:
	}
	return api.ErrSecretNotFound
}

// usable reports whether a secret serves uses: Stored, or Rotating, which keeps the old value
// until the new one is in.
func (s *secrets) usable(ctx context.Context, name string) (bool, error) {
	st, err := s.state(ctx, secret.Entity(name), "")
	if err != nil {
		return false, err
	}
	return secret.State(st) == secret.Stored || secret.State(st) == secret.Rotating, nil
}

// servesUses is usable for an effect, which has no caller to hand an error to: a store that
// cannot be read counts as not usable.
func (s *secrets) servesUses(ctx context.Context, name string) bool {
	ok, err := s.usable(ctx, name)
	if err != nil {
		s.Log.Error("cannot read the secret's state", "secret", name, "err", err)
	}
	return ok
}

// state reads an entity's persisted state; actors save before they act, so this is current.
func (s *secrets) state(ctx context.Context, entity, initial string) (string, error) {
	return loadState(ctx, s.Store, entity, initial)
}
