package berthd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
)

// Provisional defaults, awaiting the maintainer (docs/plans/e3-isolated-agents.md, item 4).
const (
	defaultAgentTTL = 12 * time.Hour
	// agentTokenPrefix lets secret scanners and people recognise a leaked token.
	agentTokenPrefix = "berth_at_"
)

type agentsDeps struct {
	Log     *slog.Logger
	Store   ports.Store
	Records ports.AgentSessionRecords
	Clock   ports.Clock
	Lineage ports.ProcessLineage
	// Code confirms the caller's audit token still names the process whose lineage was read.
	Code ports.CodeInspector
	// AgentUID is the agent socket's user; a session binds to that user's processes only.
	AgentUID uint32
	// StopProxies stops the proxy sessions authorized through an agent session that ended. It
	// runs on berthd's own context, never a caller's.
	StopProxies func(ctx context.Context, agentSession string)
	// WatchExit returns a channel closed when the process instance exits (kqueue NOTE_EXIT), or
	// ports.ErrProcessGone when it already has.
	WatchExit func(ctx context.Context, p ports.ProcessRef) (<-chan struct{}, error)
	// After runs f once d has passed and returns a function that cancels it; nil means
	// time.AfterFunc.
	After func(d time.Duration, f func()) (cancel func())
}

// agents issues agent sessions, checks their tokens on every agent socket call and limits their
// store dialogs. It implements api.Agents; the state of each session is in its machine.
type agents struct {
	agentsDeps
	// ctx is berthd's own, so the timers, the root watches and the stops they cause outlive the
	// call that set them up.
	ctx      context.Context
	sessions *actors[agentsession.State, agentsession.Event, agentsession.Effect]

	mu     sync.Mutex
	stores map[string]*storeSlots
	// watches cancels each live session's expiry timer and root watch.
	watches map[string]*sessionWatch
}

type sessionWatch struct {
	stopTimer func()
	// stopRoot ends the root watch; nil until the session is bound.
	stopRoot context.CancelFunc
}

// storeSlots is one session's store dialog use, kept in memory only: a restart of berthd forgets
// it, which at worst grants a session a fresh hour.
type storeSlots struct {
	open    int
	started []time.Time
}

var _ api.Agents = (*agents)(nil)

func newAgents(ctx context.Context, d agentsDeps) *agents {
	a := &agents{agentsDeps: d, ctx: ctx, stores: map[string]*storeSlots{}, watches: map[string]*sessionWatch{}}
	if a.After == nil {
		a.After = func(d time.Duration, f func()) func() { return func() { time.AfterFunc(d, f).Stop() } }
	}
	a.sessions = &actors[agentsession.State, agentsession.Event, agentsession.Effect]{
		ctx: ctx, log: d.Log, store: d.Store, clock: d.Clock,
		kind: agentsession.Kind, initial: agentsession.Initial, transition: agentsession.Transition,
		final: []agentsession.State{agentsession.Expired, agentsession.Revoked, agentsession.Ended},
		run: func(string) machine.Runner[agentsession.Event, agentsession.Effect] {
			return func(context.Context, agentsession.Effect) ([]agentsession.Event, error) { return nil, nil }
		},
		live: map[string]*slot[agentsession.State, agentsession.Event, agentsession.Effect]{},
		saved: func(rec ports.TransitionRecord) {
			// On the actor's goroutine: hand the work off, so the agent actor never waits on the
			// proxy actors, and use berthd's context, so a caller that gave up stops nothing short.
			if !agentsession.Live(agentsession.State(rec.To)) {
				go a.ended(strings.TrimPrefix(rec.Entity, "agent/"))
			}
		},
	}
	return a
}

// ended frees what a session held once its final state is saved: its timer, its root watch, its
// store slots and its proxy sessions. A second call finds nothing left and stops nothing new.
func (a *agents) ended(id string) {
	a.mu.Lock()
	delete(a.stores, id)
	w := a.watches[id]
	delete(a.watches, id)
	a.mu.Unlock()
	if w != nil {
		w.stopTimer()
		if w.stopRoot != nil {
			w.stopRoot()
		}
	}
	if a.StopProxies != nil {
		a.StopProxies(a.ctx, id)
	}
}

// watchExpiry sends Expire at the session's expiry, so a session nobody calls still ends and
// its proxies stop on time.
func (a *agents) watchExpiry(rec ports.AgentSessionRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.watches[rec.ID]; ok {
		return
	}
	a.watches[rec.ID] = &sessionWatch{stopTimer: a.After(rec.ExpiresAt.Sub(a.Clock.Now()), func() {
		a.background(rec.ID, agentsession.Event{Kind: agentsession.Expire})
	})}
}

// watchRoot sends RootExited when the bound root process exits (kqueue NOTE_EXIT, which checks
// the start time, so a reused pid never counts).
func (a *agents) watchRoot(id string, root ports.ProcessRef) {
	if a.WatchExit == nil {
		return
	}
	a.mu.Lock()
	w := a.watches[id]
	if w == nil || w.stopRoot != nil {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	w.stopRoot = cancel
	a.mu.Unlock()
	ev := agentsession.Event{Kind: agentsession.RootExited, Note: "root pid " + strconv.Itoa(root.PID)}
	exited, err := a.WatchExit(ctx, root)
	if err != nil {
		a.background(id, ev)
		return
	}
	go func() {
		select {
		case <-exited:
			a.background(id, ev)
		case <-ctx.Done():
		}
	}()
}

// background applies an event no caller waits for; one the session can no longer take is fine.
func (a *agents) background(id string, ev agentsession.Event) {
	if err := a.step(a.ctx, id, ev); err != nil && !errors.Is(err, machine.ErrIllegal) {
		a.Log.Error("cannot record an agent session event", "session", id, "event", ev.Kind, "err", err)
	}
}

// recover runs at boot: it ends sessions that expired or lost their root while berthd was down,
// re-arms the timers and root watches of the others, and stops any proxy session still open for
// a session that already ended.
func (a *agents) recover(ctx context.Context) error {
	recs, err := a.Records.AgentSessions(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		st, err := a.state(ctx, rec.ID)
		if err != nil {
			return err
		}
		if !agentsession.Live(st) {
			if a.StopProxies != nil {
				a.StopProxies(a.ctx, rec.ID) //nolint:contextcheck // the stop must outlive the boot step, so it uses berthd's context
			}
			continue
		}
		if st, err = a.refresh(ctx, rec, true); err != nil {
			return err
		}
		if !agentsession.Live(st) {
			continue
		}
		a.watchExpiry(rec)
		if st == agentsession.Bound && rec.Root != (ports.ProcessRef{}) {
			a.watchRoot(rec.ID, rec.Root) //nolint:contextcheck // the watch outlives this call and runs on berthd's context
		}
	}
	return nil
}

func (a *agents) CreateAgentSession(ctx context.Context, req *berthv1.CreateAgentSessionRequest) (*berthv1.CreateAgentSessionResponse, error) {
	raw := make([]byte, 16+32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	defer clear(raw)
	token := agentTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[16:])
	ttl := defaultAgentTTL
	if s := req.GetTtlSeconds(); s > 0 {
		ttl = time.Duration(s) * time.Second
	}
	now := a.Clock.Now()
	rec := ports.AgentSessionRecord{
		ID: hex.EncodeToString(raw[:16]), Name: req.GetName(), TokenHash: sha256.Sum256([]byte(token)),
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := a.Records.AddAgentSession(ctx, rec); err != nil {
		return nil, err
	}
	a.watchExpiry(rec)
	return &berthv1.CreateAgentSessionResponse{Session: agentSessionProto(rec, agentsession.Initial), Token: token}, nil
}

func (a *agents) ListAgentSessions(ctx context.Context) (*berthv1.ListAgentSessionsResponse, error) {
	recs, err := a.Records.AgentSessions(ctx)
	if err != nil {
		return nil, err
	}
	resp := &berthv1.ListAgentSessionsResponse{}
	for _, rec := range recs {
		st, err := a.refresh(ctx, rec, true)
		if err != nil {
			return nil, err
		}
		resp.Sessions = append(resp.Sessions, agentSessionProto(rec, st))
	}
	return resp, nil
}

func (a *agents) RevokeAgentSession(ctx context.Context, req *berthv1.RevokeAgentSessionRequest) (*berthv1.RevokeAgentSessionResponse, error) {
	rec, found, err := a.Records.AgentSession(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, api.ErrAgentSessionNotFound
	}
	// A session that already ended stays as it is; revoking it again changes nothing.
	if err := a.step(ctx, rec.ID, agentsession.Event{Kind: agentsession.Revoke, Note: "by the owner"}); err != nil && !errors.Is(err, machine.ErrIllegal) {
		return nil, err
	}
	st, err := a.state(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	return &berthv1.RevokeAgentSessionResponse{Session: agentSessionProto(rec, st)}, nil
}

// Authenticate accepts a token only for a live session, from the agent user, and from inside the
// session's process tree. The first accepted call binds the session to its tree: the highest
// ancestor of the caller that still runs as the agent user (agentsession.Root).
func (a *agents) Authenticate(ctx context.Context, token string, peer ports.Peer) (string, error) {
	rec, found, err := a.Records.AgentSessionByToken(ctx, sha256.Sum256([]byte(token)))
	if err != nil {
		return "", err
	}
	if !found {
		return "", a.refuse("unknown token", "", peer)
	}
	st, err := a.refresh(ctx, rec, false)
	if err != nil {
		return "", err
	}
	if !agentsession.Live(st) {
		return "", a.refuse("session "+string(st), rec.ID, peer)
	}
	lineage, err := a.Lineage.Lineage(ctx, peer.PID)
	if err != nil {
		return "", a.refuse("caller's process tree unreadable", rec.ID, peer)
	}
	// The agent socket already admits only the agent user; this keeps the rule true on its own.
	if lineage[0].UID != a.AgentUID {
		return "", a.refuse("caller is not the agent user", rec.ID, peer)
	}
	// The pid could name a later process by the time its lineage was read; the audit token cannot.
	if _, err := a.Code.Live(ctx, peer); err != nil {
		return "", a.refuse("caller exited", rec.ID, peer)
	}
	root := rec.Root
	if root == (ports.ProcessRef{}) {
		mine, ok := agentsession.Root(lineage, a.AgentUID)
		if !ok {
			return "", a.refuse("caller has no process tree to bind", rec.ID, peer)
		}
		// Of two first calls racing, one tree wins and the other caller is refused below.
		if root, err = a.Records.BindAgentSession(ctx, rec.ID, mine); err != nil {
			return "", err
		}
	}
	if st == agentsession.Issued {
		ev := agentsession.Event{Kind: agentsession.Bind, Note: "root pid " + strconv.Itoa(root.PID)}
		if err := a.step(ctx, rec.ID, ev); err != nil && !errors.Is(err, machine.ErrIllegal) {
			return "", err
		}
		// Bind is illegal when a concurrent first call bound the session, which leaves it Bound,
		// or when a revoke or an expiry landed since the refresh, which must refuse this call.
		now, err := a.state(ctx, rec.ID)
		if err != nil {
			return "", err
		}
		if now != agentsession.Bound {
			return "", a.refuse("session "+string(now), rec.ID, peer)
		}
		a.watchRoot(rec.ID, root) //nolint:contextcheck // the watch outlives this call and runs on berthd's context
	}
	if !agentsession.Descends(lineage, root) {
		rec.Root = root
		if _, err := a.refresh(ctx, rec, true); err != nil {
			return "", err
		}
		return "", a.refuse("caller outside the session's process tree", rec.ID, peer)
	}
	return rec.ID, nil
}

// refuse logs why a token was refused, never the token, and returns the one error every refusal
// shares.
func (a *agents) refuse(why, session string, peer ports.Peer) error {
	a.Log.Warn("refused agent token", "why", why, "session", session, "pid", peer.PID)
	return api.ErrAgentTokenInvalid
}

func (a *agents) AdmitStore(ctx context.Context, sessionID string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Read under a.mu: step deletes an ended session's slots under it after saving the end, so a
	// session either reads as ended here or has its slots deleted after this.
	if !a.live(ctx, sessionID) {
		return nil, api.ErrAgentTokenInvalid
	}
	slots := a.stores[sessionID]
	if slots == nil {
		slots = &storeSlots{}
		a.stores[sessionID] = slots
	}
	ok, kept := agentsession.AdmitStore(slots.open, slots.started, a.Clock.Now())
	slots.started = kept
	if !ok {
		a.Log.Warn("refused a store request over the session's limit", "session", sessionID)
		return nil, api.ErrRateLimited
	}
	slots.open++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			slots.open--
			a.mu.Unlock()
		})
	}, nil
}

// refresh records what time and the process table already decided: a session past its expiry
// is Expired, and, when checkRoot is set, a bound session whose root is gone is Ended. It
// returns the state afterwards.
func (a *agents) refresh(ctx context.Context, rec ports.AgentSessionRecord, checkRoot bool) (agentsession.State, error) {
	st, err := a.state(ctx, rec.ID)
	if err != nil || !agentsession.Live(st) {
		return st, err
	}
	var ev agentsession.Event
	switch {
	case !a.Clock.Now().Before(rec.ExpiresAt):
		ev = agentsession.Event{Kind: agentsession.Expire}
	case checkRoot && st == agentsession.Bound && rec.Root != (ports.ProcessRef{}) && !a.running(ctx, rec.Root):
		ev = agentsession.Event{Kind: agentsession.RootExited, Note: "root pid " + strconv.Itoa(rec.Root.PID)}
	default:
		return st, nil
	}
	if err := a.step(ctx, rec.ID, ev); err != nil && !errors.Is(err, machine.ErrIllegal) {
		return st, err
	}
	return a.state(ctx, rec.ID)
}

func (a *agents) running(ctx context.Context, p ports.ProcessRef) bool {
	lineage, err := a.Lineage.Lineage(ctx, p.PID)
	return err == nil && lineage[0].Same(p)
}

// step applies one event; what an ended session held is freed by the saved hook. An event the
// session can no longer take, including one whose actor stopped because another step ended the
// session first, is machine.ErrIllegal; an actor that stopped for any other reason (a failed
// save, berthd stopping) is api.ErrUnavailable, so no caller mistakes it for an outcome.
func (a *agents) step(ctx context.Context, id string, ev agentsession.Event) error {
	_, _, err := step(ctx, a.sessions, agentsession.Entity(id), ev, nil, ev.Kind)
	if errors.Is(err, errActorStopped) {
		if st, serr := a.state(ctx, id); serr == nil && !agentsession.Live(st) {
			return fmt.Errorf("%w: %s in agent session %s", machine.ErrIllegal, ev.Kind, id)
		}
		return api.ErrUnavailable
	}
	return err
}

// live reports whether the session still serves calls; a store error counts as not live.
func (a *agents) live(ctx context.Context, id string) bool {
	st, err := a.state(ctx, id)
	return err == nil && agentsession.Live(st)
}

func (a *agents) state(ctx context.Context, id string) (agentsession.State, error) {
	st, found, err := a.Store.LoadState(ctx, agentsession.Entity(id))
	if err != nil || !found {
		return agentsession.Initial, err
	}
	return agentsession.State(st.State), nil
}

var agentSessionStates = map[agentsession.State]berthv1.AgentSessionState{
	agentsession.Issued:  berthv1.AgentSessionState_AGENT_SESSION_STATE_ISSUED,
	agentsession.Bound:   berthv1.AgentSessionState_AGENT_SESSION_STATE_BOUND,
	agentsession.Expired: berthv1.AgentSessionState_AGENT_SESSION_STATE_EXPIRED,
	agentsession.Revoked: berthv1.AgentSessionState_AGENT_SESSION_STATE_REVOKED,
	agentsession.Ended:   berthv1.AgentSessionState_AGENT_SESSION_STATE_ENDED,
}

func agentSessionProto(rec ports.AgentSessionRecord, st agentsession.State) *berthv1.AgentSession {
	return &berthv1.AgentSession{
		Id: rec.ID, Name: rec.Name, State: agentSessionStates[st],
		CreatedAt: timestamppb.New(rec.CreatedAt), ExpiresAt: timestamppb.New(rec.ExpiresAt),
		RootPid: int32(rec.Root.PID), //nolint:gosec // macOS process IDs fit in 32 bits
	}
}
