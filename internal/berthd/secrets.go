package berthd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/core/secret"
)

const (
	// ticketLife is how long a use ticket stays redeemable.
	ticketLife = 30 * time.Second
	// promptTimeout bounds a dialog or Touch ID prompt the person walked away from.
	promptTimeout = 5 * time.Minute
)

// Helper identity: berthd answers RedeemUseTicket only for this signed berth-native.
const (
	HelperTeamID    = "XPJ7XA42J4"
	HelperSigningID = "com.wesleybldc.berth.native"
	// helperRequirement pins Apple's certificate chain as well as the team and identifier,
	// which codesign -d alone would print for a self signed copy too.
	helperRequirement = `anchor apple generic and certificate leaf[subject.OU] = "` + HelperTeamID +
		`" and identifier "` + HelperSigningID + `"`
)

// TrustHelper decides whether an identified executable is berth-native.
type TrustHelper func(ctx context.Context, exe ports.Executable) (bool, error)

// Satisfier checks a file against a code requirement (execid.Identity implements it).
type Satisfier interface {
	Satisfies(ctx context.Context, path, requirement string) (bool, error)
}

// SignedHelper is the release trust rule: the team and identifier codesign reports, and a
// requirement check that only Apple's chain for that team can pass.
func SignedHelper(s Satisfier) TrustHelper {
	return func(ctx context.Context, exe ports.Executable) (bool, error) {
		if exe.TeamID != HelperTeamID || exe.SigningID != HelperSigningID {
			return false, nil
		}
		return s.Satisfies(ctx, exe.Path, helperRequirement)
	}
}

type secretsDeps struct {
	Log      *slog.Logger
	Store    ports.Store
	Records  ports.SecretRecords
	Clock    ports.Clock
	Approver ports.Approver
	Execs    ports.ExecIdentity
	Peers    ports.PeerFacts
	Tickets  ports.TicketSource
	Trust    TrustHelper
}

// secrets runs one actor per secret and per grant and performs their effects. It implements
// api.Secrets; every decision about states is in the core machines.
type secrets struct {
	secretsDeps
	// ctx outlives any one call, so a dialog keeps going when the caller that opened it leaves.
	ctx     context.Context
	secrets *actors[secret.State, secret.Event, secret.Effect]
	grants  *actors[grant.State, grant.Event, grant.Effect]

	mu      sync.Mutex
	flights map[string]*flight
	// pending carries what an effect needs and its event cannot hold, keyed by entity; the
	// entity's turn makes sure only one call uses an entry at a time.
	pending map[string]any
	issued  map[string]issued
	live    map[ports.Ticket]liveTicket
}

type flight struct {
	done    chan struct{}
	outcome ports.StoreOutcome
	err     error
}

type useRequest struct {
	grantID int64
	name    string
	sha256  [32]byte
	pid     int
}

type issued struct {
	ticket  ports.Ticket
	expires time.Time
	err     error
}

type liveTicket struct {
	name    string
	sha256  [32]byte
	pid     int
	expires time.Time
}

var _ api.Secrets = (*secrets)(nil)

func newSecrets(ctx context.Context, d secretsDeps) *secrets {
	s := &secrets{
		secretsDeps: d, ctx: ctx,
		flights: map[string]*flight{}, pending: map[string]any{}, issued: map[string]issued{}, live: map[ports.Ticket]liveTicket{},
	}
	s.secrets = &actors[secret.State, secret.Event, secret.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: secret.Kind, initial: secret.Initial, transition: secret.Transition, run: s.runSecret,
		live: map[string]*slot[secret.State, secret.Event, secret.Effect]{},
	}
	s.grants = &actors[grant.State, grant.Event, grant.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: grant.Kind, initial: grant.Initial, transition: grant.Transition, run: s.runGrant,
		live: map[string]*slot[grant.State, grant.Event, grant.Effect]{},
	}
	return s
}

func (s *secrets) RequestSecret(ctx context.Context, req *berthv1.RequestSecretRequest, peer int) (*berthv1.RequestSecretResponse, error) {
	name := req.GetName()
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return nil, err
	}
	outcome, err := s.storeOnce(ctx, ports.StorePrompt{Name: name, Reason: req.GetReason(), Requester: facts})
	if err != nil {
		return nil, err
	}
	resp := &berthv1.RequestSecretResponse{Name: name, Outcome: berthv1.StoreOutcome_STORE_OUTCOME_FAILED}
	switch outcome {
	case ports.StoreStored:
		resp.Outcome = berthv1.StoreOutcome_STORE_OUTCOME_STORED
	case ports.StoreDenied:
		resp.Outcome = berthv1.StoreOutcome_STORE_OUTCOME_DENIED
	case ports.StoreFailed:
	}
	return resp, nil
}

// storeOnce holds one store dialog per name at a time: a caller who arrives while one is open
// gets that dialog's outcome instead of opening a second.
func (s *secrets) storeOnce(ctx context.Context, p ports.StorePrompt) (ports.StoreOutcome, error) {
	s.mu.Lock()
	f, joined := s.flights[p.Name]
	if !joined {
		f = &flight{done: make(chan struct{})}
		s.flights[p.Name] = f
		go func() {
			f.outcome, f.err = s.store(p)
			s.mu.Lock()
			delete(s.flights, p.Name)
			s.mu.Unlock()
			close(f.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.outcome, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *secrets) store(p ports.StorePrompt) (ports.StoreOutcome, error) {
	ctx := s.ctx
	entity := secret.Entity(p.Name)
	state, err := s.state(ctx, entity, string(secret.Initial))
	if err != nil {
		return "", err
	}
	if secret.State(state) == secret.Stored {
		return "", api.ErrSecretExists
	}
	if err := s.Records.AddSecret(ctx, p.Name, s.Clock.Now()); err != nil {
		return "", err
	}
	s.setPending(entity, p)
	rec, err := step(ctx, s.secrets, entity, secret.Event{Kind: secret.StoreRequested},
		secret.StoreSucceeded, secret.StoreDenied, secret.StoreFailed)
	if err != nil {
		return "", err
	}
	return map[secret.EventKind]ports.StoreOutcome{
		secret.StoreSucceeded: ports.StoreStored,
		secret.StoreDenied:    ports.StoreDenied,
		secret.StoreFailed:    ports.StoreFailed,
	}[secret.EventKind(rec.Event)], nil
}

func (s *secrets) runSecret(entity string) machine.Runner[secret.Event, secret.Effect] {
	return func(ctx context.Context, effect secret.Effect) ([]secret.Event, error) {
		if effect.Kind == secret.PromptStore {
			p, ok := s.takePending(entity).(ports.StorePrompt)
			if !ok {
				return []secret.Event{{Kind: secret.StoreFailed, Note: "no prompt to show"}}, nil
			}
			ctx, cancel := context.WithTimeout(ctx, promptTimeout)
			defer cancel()
			outcome, err := s.Approver.StoreSecret(ctx, p)
			if err != nil {
				s.Log.Warn("store prompt failed", "secret", p.Name, "err", err)
			}
			switch outcome {
			case ports.StoreStored:
				return []secret.Event{{Kind: secret.StoreSucceeded}}, nil
			case ports.StoreDenied:
				return []secret.Event{{Kind: secret.StoreDenied}}, nil
			case ports.StoreFailed:
			}
			return []secret.Event{{Kind: secret.StoreFailed, Note: "the helper reported a failure"}}, nil
		}
		return nil, fmt.Errorf("unknown secret effect %q", effect.Kind)
	}
}

func (s *secrets) ListSecrets(ctx context.Context) (*berthv1.ListSecretsResponse, error) {
	rows, err := s.Records.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	resp := &berthv1.ListSecretsResponse{}
	for _, r := range rows {
		state, err := s.state(ctx, secret.Entity(r.Name), string(secret.Initial))
		if err != nil {
			return nil, err
		}
		summary := &berthv1.SecretSummary{Name: r.Name, State: secretState(secret.State(state)), CreatedAt: timestamppb.New(r.CreatedAt)}
		grants, err := s.Records.Grants(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			gs, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
			if err != nil {
				return nil, err
			}
			summary.Consumers = append(summary.Consumers, consumer(g, grant.State(gs)))
		}
		resp.Secrets = append(resp.Secrets, summary)
	}
	return resp, nil
}

func (s *secrets) GrantSecret(ctx context.Context, req *berthv1.GrantSecretRequest, peer int) (*berthv1.GrantSecretResponse, error) {
	name, glob := req.GetName(), req.GetArgsGlob()
	if _, err := path.Match(glob, ""); err != nil {
		return nil, api.ErrBadGlob
	}
	if err := s.requireStored(ctx, name); err != nil {
		return nil, err
	}
	exe, err := s.identify(ctx, req.GetExePath())
	if err != nil {
		return nil, err
	}
	facts, err := s.facts(ctx, peer)
	if err != nil {
		return nil, err
	}
	g := ports.GrantRecord{Secret: name, Exe: exe, ArgsGlob: glob, CreatedAt: s.Clock.Now()}
	if g.ID, err = s.Records.AddGrant(ctx, g); err != nil {
		return nil, err
	}
	entity := grant.Entity(g.ID)
	s.setPending(entity, ports.PowerPrompt{Action: ports.PowerGrant, Secret: name, Consumer: exe, ArgsGlob: glob, Requester: facts})
	rec, err := step(ctx, s.grants, entity, grant.Event{Kind: grant.GrantRequested}, grant.GrantApproved, grant.GrantDenied, grant.GrantFailed)
	if err != nil {
		return nil, err
	}
	if grant.EventKind(rec.Event) == grant.GrantApproved {
		return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED, Consumer: consumer(g, grant.Active)}, nil
	}
	if grant.EventKind(rec.Event) == grant.GrantDenied {
		return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_DENIED}, nil
	}
	return &berthv1.GrantSecretResponse{Outcome: berthv1.GrantOutcome_GRANT_OUTCOME_FAILED}, nil
}

func (s *secrets) runGrant(entity string) machine.Runner[grant.Event, grant.Effect] {
	return func(ctx context.Context, effect grant.Effect) ([]grant.Event, error) {
		switch effect.Kind {
		case grant.PromptPower:
			p, ok := s.takePending(entity).(ports.PowerPrompt)
			if !ok {
				return []grant.Event{{Kind: grant.GrantFailed, Note: "no prompt to show"}}, nil
			}
			ctx, cancel := context.WithTimeout(ctx, promptTimeout)
			defer cancel()
			outcome, err := s.Approver.ConfirmPower(ctx, p)
			if err != nil {
				s.Log.Warn("grant prompt failed", "secret", p.Secret, "err", err)
			}
			switch outcome {
			case ports.PowerApproved:
				return []grant.Event{{Kind: grant.GrantApproved}}, nil
			case ports.PowerDenied:
				return []grant.Event{{Kind: grant.GrantDenied}}, nil
			case ports.PowerFailed:
			}
			return []grant.Event{{Kind: grant.GrantFailed, Note: "the helper reported a failure"}}, nil
		case grant.IssueTicket:
			s.issue(ctx, entity)
			return nil, nil
		}
		return nil, fmt.Errorf("unknown grant effect %q", effect.Kind)
	}
}

// issue makes the ticket for the use waiting on entity and counts the use. A failure is handed
// to the waiting call, which answers with an internal error; the grant itself stays Active.
func (s *secrets) issue(ctx context.Context, entity string) {
	u, ok := s.takePending(entity).(useRequest)
	if !ok {
		s.setIssued(entity, issued{err: errors.New("no use request waiting")})
		return
	}
	t, err := s.Tickets.NewTicket(ctx)
	if err != nil {
		s.setIssued(entity, issued{err: fmt.Errorf("new ticket: %w", err)})
		return
	}
	now := s.Clock.Now()
	if err := s.Records.CountUse(ctx, u.grantID, now); err != nil {
		s.setIssued(entity, issued{err: fmt.Errorf("count use: %w", err)})
		return
	}
	expires := now.Add(ticketLife)
	s.mu.Lock()
	s.live[t] = liveTicket{name: u.name, sha256: u.sha256, pid: u.pid, expires: expires}
	s.issued[entity] = issued{ticket: t, expires: expires}
	s.mu.Unlock()
}

func (s *secrets) AuthorizeUse(ctx context.Context, req *berthv1.AuthorizeUseRequest, peer int) (*berthv1.AuthorizeUseResponse, error) {
	name := req.GetName()
	if peer <= 0 {
		return nil, api.ErrCallerUnknown
	}
	if err := s.requireStored(ctx, name); err != nil {
		return nil, err
	}
	// Identified here, never taken from the client, and matched by the resolved path.
	exe, err := s.identify(ctx, req.GetExePath())
	if err != nil {
		return nil, err
	}
	grants, err := s.Records.Grants(ctx, name)
	if err != nil {
		return nil, err
	}
	args := strings.Join(req.GetArgv(), " ")
	var match *ports.GrantRecord
	var refusing *ports.GrantRecord
	stale, argsRefused := false, false
	for i := range grants {
		g := &grants[i]
		if g.Exe.Path != exe.Path {
			continue
		}
		st, err := s.state(ctx, grant.Entity(g.ID), string(grant.Initial))
		if err != nil {
			return nil, err
		}
		switch grant.State(st) {
		case grant.Active:
			if g.Exe.SHA256 != exe.SHA256 {
				if _, err := step(ctx, s.grants, grant.Entity(g.ID), grant.Event{Kind: grant.HashChanged, Note: "sha256 now " + shortHex(exe.SHA256)}, grant.HashChanged); err != nil {
					return nil, err
				}
				stale = true
				continue
			}
			if match == nil && argsMatch(g.ArgsGlob, args) {
				match = g
			} else if match == nil {
				argsRefused = true
			}
		case grant.Stale:
			stale = true
			if refusing == nil {
				refusing = g
			}
		case grant.Denied, grant.Revoked:
			if refusing == nil {
				refusing = g
			}
		case grant.Requested:
		}
	}
	if match != nil {
		return s.authorize(ctx, *match, exe, peer)
	}
	outcome := berthv1.UseOutcome_USE_OUTCOME_NO_GRANT
	if stale {
		outcome = berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT
	}
	switch {
	case refusing != nil:
		_, err = step(ctx, s.grants, grant.Entity(refusing.ID), grant.Event{Kind: grant.UseRefused}, grant.UseRefused)
	case stale:
		// The HashChanged row already records this refusal.
	default:
		note := "no grant for " + exe.Path
		if argsRefused {
			note = "arguments do not match any grant for " + exe.Path
		}
		_, err = step(ctx, s.secrets, secret.Entity(name), secret.Event{Kind: secret.UseRefused, Note: note}, secret.UseRefused)
	}
	if err != nil {
		return nil, err
	}
	return &berthv1.AuthorizeUseResponse{Outcome: outcome}, nil
}

func (s *secrets) authorize(ctx context.Context, g ports.GrantRecord, exe ports.Executable, peer int) (*berthv1.AuthorizeUseResponse, error) {
	entity := grant.Entity(g.ID)
	s.setPending(entity, useRequest{grantID: g.ID, name: g.Secret, sha256: exe.SHA256, pid: peer})
	if _, err := step(ctx, s.grants, entity, grant.Event{Kind: grant.UseAuthorized}, grant.UseAuthorized); err != nil {
		return nil, err
	}
	s.mu.Lock()
	got, ok := s.issued[entity]
	delete(s.issued, entity)
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("no ticket was issued")
	}
	if got.err != nil {
		s.Log.Error("ticket not issued", "entity", entity, "err", got.err)
		return nil, got.err
	}
	return &berthv1.AuthorizeUseResponse{
		Outcome: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED, Ticket: got.ticket[:], ExpiresAt: timestamppb.New(got.expires),
	}, nil
}

func (s *secrets) TrustRedeemer(ctx context.Context, peer int) error {
	if peer <= 0 {
		return api.ErrPeerNotTrusted
	}
	facts, err := s.Peers.Facts(ctx, peer)
	if err != nil {
		s.Log.Warn("redeem from an unknown peer", "pid", peer, "err", err)
		return api.ErrPeerNotTrusted
	}
	exe, err := s.Execs.Identify(ctx, facts.Executable)
	if err != nil {
		s.Log.Warn("redeem from an unidentified peer", "pid", peer, "err", err)
		return api.ErrPeerNotTrusted
	}
	ok, err := s.Trust(ctx, exe)
	if err != nil || !ok {
		s.Log.Warn("redeem refused: peer is not berth-native", "pid", peer, "executable", exe.Path, "err", err)
		return api.ErrPeerNotTrusted
	}
	return nil
}

// RedeemUseTicket spends a ticket. It is gone after the first attempt, even one that fails a
// later check, so a ticket is never tried twice.
func (s *secrets) RedeemUseTicket(ctx context.Context, ticket ports.Ticket, peer int) (*berthv1.RedeemUseTicketResponse, error) {
	now := s.Clock.Now()
	s.mu.Lock()
	t, ok := s.live[ticket]
	delete(s.live, ticket)
	for k, v := range s.live {
		if now.After(v.expires) {
			delete(s.live, k)
		}
	}
	s.mu.Unlock()
	if !ok || now.After(t.expires) {
		return nil, api.ErrTicketInvalid
	}
	// The helper redeeming must be a child of the process the ticket was issued to, which is
	// how berth run starts it.
	facts, err := s.Peers.Facts(ctx, peer)
	if err != nil || len(facts.Tree) == 0 || facts.Tree[0].PID != t.pid {
		s.Log.Warn("ticket redeemed by a helper the caller did not start", "pid", peer)
		return nil, api.ErrTicketInvalid
	}
	return &berthv1.RedeemUseTicketResponse{Name: t.name}, nil
}

func (s *secrets) facts(ctx context.Context, peer int) (ports.Requester, error) {
	if peer <= 0 {
		return ports.Requester{}, api.ErrCallerUnknown
	}
	r, err := s.Peers.Facts(ctx, peer)
	if err != nil {
		s.Log.Warn("cannot describe the caller", "pid", peer, "err", err)
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

func (s *secrets) requireStored(ctx context.Context, name string) error {
	st, err := s.state(ctx, secret.Entity(name), "")
	if err != nil {
		return err
	}
	if secret.State(st) != secret.Stored {
		return api.ErrSecretNotFound
	}
	return nil
}

// state reads an entity's persisted state; actors save before they act, so this is current.
func (s *secrets) state(ctx context.Context, entity, initial string) (string, error) {
	st, found, err := s.Store.LoadState(ctx, entity)
	if err != nil {
		return "", err
	}
	if !found {
		return initial, nil
	}
	return st.State, nil
}

func (s *secrets) setPending(entity string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[entity] = v
}

func (s *secrets) takePending(entity string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.pending[entity]
	delete(s.pending, entity)
	return v
}

func (s *secrets) setIssued(entity string, v issued) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued[entity] = v
}

// argsMatch applies a grant's glob to the arguments joined by spaces; an empty glob allows any.
func argsMatch(glob, args string) bool {
	if glob == "" {
		return true
	}
	ok, err := path.Match(glob, args)
	return err == nil && ok
}

func shortHex(sum [32]byte) string { return hex.EncodeToString(sum[:6]) }

func consumer(g ports.GrantRecord, st grant.State) *berthv1.Consumer {
	c := &berthv1.Consumer{
		GrantId: g.ID, State: grantState(st), ExePath: g.Exe.Path, Sha256: hex.EncodeToString(g.Exe.SHA256[:]),
		TeamId: g.Exe.TeamID, SigningId: g.Exe.SigningID, ArgsGlob: g.ArgsGlob,
		CreatedAt: timestamppb.New(g.CreatedAt), UseCount: g.UseCount,
	}
	if !g.LastUsedAt.IsZero() {
		c.LastUsedAt = timestamppb.New(g.LastUsedAt)
	}
	return c
}

func secretState(s secret.State) berthv1.SecretState {
	switch s {
	case secret.Pending:
		return berthv1.SecretState_SECRET_STATE_PENDING
	case secret.Stored:
		return berthv1.SecretState_SECRET_STATE_STORED
	case secret.Denied:
		return berthv1.SecretState_SECRET_STATE_DENIED
	case secret.Failed:
		return berthv1.SecretState_SECRET_STATE_FAILED
	}
	return berthv1.SecretState_SECRET_STATE_UNSPECIFIED
}

func grantState(s grant.State) berthv1.GrantState {
	switch s {
	case grant.Requested:
		return berthv1.GrantState_GRANT_STATE_REQUESTED
	case grant.Active:
		return berthv1.GrantState_GRANT_STATE_ACTIVE
	case grant.Denied:
		return berthv1.GrantState_GRANT_STATE_DENIED
	case grant.Stale:
		return berthv1.GrantState_GRANT_STATE_STALE
	case grant.Revoked:
		return berthv1.GrantState_GRANT_STATE_REVOKED
	}
	return berthv1.GrantState_GRANT_STATE_UNSPECIFIED
}

// actors keeps one running actor per entity of one machine, created on first use.
type actors[S ~string, E machine.Event, F any] struct {
	ctx        context.Context
	log        *slog.Logger
	store      ports.Store
	clock      ports.Clock
	kind       string
	initial    S
	transition machine.Transition[S, E, F]
	run        func(entity string) machine.Runner[E, F]

	mu   sync.Mutex
	live map[string]*slot[S, E, F]
}

type slot[S ~string, E machine.Event, F any] struct {
	actor *machine.Actor[S, E, F]
	// turn admits one call at a time, so the waiter sees only its own outcome.
	turn    chan struct{}
	stopped chan struct{}
	mu      sync.Mutex
	waiter  chan applied
}

type applied struct {
	rec ports.TransitionRecord
	err error
}

func (sl *slot[S, E, F]) tell(a applied) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.waiter != nil {
		select {
		case sl.waiter <- a:
		default:
		}
	}
}

func (sl *slot[S, E, F]) setWaiter(w chan applied) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.waiter = w
}

func (a *actors[S, E, F]) get(ctx context.Context, entity string) (*slot[S, E, F], error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sl, ok := a.live[entity]; ok {
		return sl, nil
	}
	sl := &slot[S, E, F]{turn: make(chan struct{}, 1), stopped: make(chan struct{})}
	actor, err := machine.NewActor(ctx, machine.Config[S, E, F]{
		Entity: entity, Kind: a.kind, Initial: a.initial, Transition: a.transition, Run: a.run(entity),
		Store: a.store, Clock: a.clock,
		Rejected: func(err error) { sl.tell(applied{err: err}) },
		Applied:  func(rec ports.TransitionRecord) { sl.tell(applied{rec: rec}) },
	})
	if err != nil {
		return nil, err
	}
	sl.actor = actor
	a.live[entity] = sl
	go func() {
		if err := actor.Run(a.ctx); err != nil {
			a.log.Error("actor stopped", "entity", entity, "err", err)
		}
		// A stopped actor is dropped, so the next call reloads the entity from the store.
		a.mu.Lock()
		if a.live[entity] == sl {
			delete(a.live, entity)
		}
		a.mu.Unlock()
		close(sl.stopped)
	}()
	return sl, nil
}

// step sends event to the entity's actor and waits until one of the until events has been
// applied (saved, with its effects run). An illegal event comes back as an error.
func step[S ~string, E machine.Event, F any, K ~string](ctx context.Context, a *actors[S, E, F], entity string, event E, until ...K) (ports.TransitionRecord, error) {
	names := make([]string, 0, len(until))
	for _, u := range until {
		names = append(names, string(u))
	}
	return a.step(ctx, entity, event, names)
}

func (a *actors[S, E, F]) step(ctx context.Context, entity string, event E, until []string) (ports.TransitionRecord, error) {
	sl, err := a.get(ctx, entity)
	if err != nil {
		return ports.TransitionRecord{}, err
	}
	select {
	case sl.turn <- struct{}{}:
	case <-ctx.Done():
		return ports.TransitionRecord{}, ctx.Err()
	}
	defer func() { <-sl.turn }()
	w := make(chan applied, 16)
	sl.setWaiter(w)
	defer sl.setWaiter(nil)
	if err := sl.actor.Send(ctx, event); err != nil {
		return ports.TransitionRecord{}, err
	}
	for {
		select {
		case got := <-w:
			if got.err != nil {
				return ports.TransitionRecord{}, got.err
			}
			if slices.Contains(until, got.rec.Event) {
				return got.rec, nil
			}
		case <-sl.stopped:
			return ports.TransitionRecord{}, fmt.Errorf("actor for %s stopped", entity)
		case <-ctx.Done():
			return ports.TransitionRecord{}, ctx.Err()
		}
	}
}
