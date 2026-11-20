package berthd

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"berth/internal/adapters/api"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
)

// errActorStopped means the actor stopped before the step heard its outcome. step turns it into
// machine.ErrIllegal or api.ErrUnavailable by why the actor stopped, so it never reaches a caller.
var errActorStopped = errors.New("actor stopped")

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
	// final lists the states an entity rests in until a new request, if any, arrives: an actor
	// stops once it reaches one, or once it refuses an event in one, so no entity that ended leaves
	// a goroutine behind. A later event for it starts a new actor, which applies it (a self loop
	// recording a refused use, or a new request that leaves the state) and stops again if it is
	// still in a final state.
	final []S
	// saved, when set, is told about every transition once it is saved and its effects have run.
	// It must not block, since it runs on the actor's goroutine.
	saved func(ports.TransitionRecord)
	// persisted, when set, is told about every transition as soon as it is saved, before its
	// effects run, on the actor's goroutine; it must not block.
	persisted func(ports.TransitionRecord)

	mu   sync.Mutex
	live map[string]*slot[S, E, F]
	// busy counts steps whose event the actor has not finished with, so a stop can wait for them;
	// idle is closed when busy falls to zero, and is nil while it is zero.
	busy int
	idle chan struct{}
}

type slot[S ~string, E machine.Event, F any] struct {
	actor *machine.Actor[S, E, F]
	// turn admits one step at a time, from before its event is sent until the actor has
	// finished with it, so a waiter only ever sees its own outcome and its own data.
	turn    chan struct{}
	stopped chan struct{}
	// cancel stops this actor alone, for drop.
	cancel context.CancelFunc

	mu     sync.Mutex
	waiter chan applied
	// ended is set while the actor rests in the final state it loaded and no step has sent it an
	// event, so no event will come to stop it.
	ended bool
	// refusedIn is the final state the actor stopped in after refusing an event, set before it
	// stops; any other stop (a failed save, berthd stopping) leaves it empty.
	refusedIn S
	// input is what the step's effects need beyond the event; output is what they hand back.
	input  any
	output any
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

func (a *actors[S, E, F]) get(ctx context.Context, entity string) (*slot[S, E, F], error) { //nolint:contextcheck // an actor outlives the call that first needed it
	a.mu.Lock()
	defer a.mu.Unlock()
	if sl, ok := a.live[entity]; ok {
		return sl, nil
	}
	// A stopping berthd starts no actor: it could still save an event whose outcome no caller
	// would hear.
	if a.ctx.Err() != nil {
		return nil, api.ErrUnavailable
	}
	sl := &slot[S, E, F]{turn: make(chan struct{}, 1), stopped: make(chan struct{})}
	// An actor in a final state is forgotten before its waiter hears the outcome, so the next step,
	// which can start only after that, finds no stale actor and loads a fresh one.
	var actor *machine.Actor[S, E, F]
	actor, err := machine.NewActor(ctx, machine.Config[S, E, F]{
		Entity: entity, Kind: a.kind, Initial: a.initial, Transition: a.transition, Run: a.run(entity),
		Store: a.store, Clock: a.clock, Saved: a.persisted,
		Rejected: func(err error) {
			final := slices.Contains(a.final, actor.State())
			if final {
				a.forget(entity, sl)
				sl.mu.Lock()
				sl.refusedIn = actor.State()
				sl.mu.Unlock()
			}
			sl.tell(applied{err: err})
			if final {
				sl.cancel()
			}
		},
		Applied: func(rec ports.TransitionRecord) {
			final := slices.Contains(a.final, S(rec.To))
			if final {
				a.forget(entity, sl)
			}
			sl.tell(applied{rec: rec})
			if a.saved != nil {
				a.saved(rec)
			}
			if final {
				sl.cancel()
			}
		},
	})
	if err != nil {
		return nil, err
	}
	sl.actor = actor
	sl.ended = slices.Contains(a.final, actor.State())
	a.live[entity] = sl
	actorCtx, cancel := context.WithCancel(a.ctx)
	sl.cancel = cancel
	go func() {
		defer cancel()
		if err := actor.Run(actorCtx); err != nil {
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

// forget drops an actor in a final state from the live set; its caller then stops it.
func (a *actors[S, E, F]) forget(entity string, sl *slot[S, E, F]) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live[entity] == sl {
		delete(a.live, entity)
	}
}

// abandon stops an actor that loaded a final state when the step that wanted it gave up before
// sending its event, since no event would come to stop it. It acts only when it can take the
// turn, so no step is between its holds check and its send; a step holding the turn either sends,
// and the actor stops after that event, or abandons in turn.
func (a *actors[S, E, F]) abandon(entity string, sl *slot[S, E, F]) {
	select {
	case sl.turn <- struct{}{}:
	default:
		return
	}
	defer func() { <-sl.turn }()
	sl.mu.Lock()
	ended := sl.ended
	sl.mu.Unlock()
	if ended && a.holds(entity, sl) {
		a.forget(entity, sl)
		sl.cancel()
	}
}

// holds reports whether sl is still the entity's live actor.
func (a *actors[S, E, F]) holds(entity string, sl *slot[S, E, F]) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live[entity] == sl
}

// input is called from an effect to read what its step carried in.
func (a *actors[S, E, F]) input(entity string) any {
	a.mu.Lock()
	sl := a.live[entity]
	a.mu.Unlock()
	if sl == nil {
		return nil
	}
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return sl.input
}

// output is called from an effect to hand a result back to its step.
func (a *actors[S, E, F]) output(entity string, v any) {
	a.mu.Lock()
	sl := a.live[entity]
	a.mu.Unlock()
	if sl == nil {
		return
	}
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.output = v
}

type stepResult struct {
	rec    ports.TransitionRecord
	output any
	err    error
}

// step sends event with input for its effects and waits until one of the until events has been
// applied (saved, with its effects run), returning the record and what the effects handed
// back. A caller that gives up returns at once, but the entity stays held until the actor has
// finished with the event, so the next step never sees this one's data or outcome.
func step[S ~string, E machine.Event, F any, K ~string](ctx context.Context, a *actors[S, E, F], entity string, event E, input any, until ...K) (ports.TransitionRecord, any, error) {
	names := make([]string, 0, len(until))
	for _, u := range until {
		names = append(names, string(u))
	}
	var sl *slot[S, E, F]
	for {
		var err error
		if sl, err = a.get(ctx, entity); err != nil {
			return ports.TransitionRecord{}, nil, err
		}
		select {
		case sl.turn <- struct{}{}:
		case <-ctx.Done():
			a.abandon(entity, sl)
			return ports.TransitionRecord{}, nil, ctx.Err()
		}
		// The step before this one may have left the entity in a final state, which stops its
		// actor; the event goes to a fresh one instead.
		if a.holds(entity, sl) {
			break
		}
		<-sl.turn
	}
	w := make(chan applied, 16)
	sl.mu.Lock()
	sl.waiter, sl.input, sl.output = w, input, nil
	sl.mu.Unlock()
	release := func() stepResult {
		sl.mu.Lock()
		out := sl.output
		sl.waiter, sl.input, sl.output = nil, nil, nil
		sl.mu.Unlock()
		<-sl.turn
		return stepResult{output: out}
	}
	if err := sl.actor.Send(ctx, event); err != nil {
		release()
		a.abandon(entity, sl)
		return ports.TransitionRecord{}, nil, err
	}
	sl.mu.Lock()
	sl.ended = false
	sl.mu.Unlock()
	a.mu.Lock()
	if a.busy++; a.idle == nil {
		a.idle = make(chan struct{})
	}
	a.mu.Unlock()
	// The step stays busy until its caller has the outcome, or, for a caller that gave up, until
	// the actor has finished with the event.
	leave := func() {
		a.mu.Lock()
		if a.busy--; a.busy == 0 {
			close(a.idle)
			a.idle = nil
		}
		a.mu.Unlock()
	}
	done := make(chan stepResult, 1)
	go func() {
		rec, err := waitApplied(sl, w, names)
		r := release()
		r.rec, r.err = rec, err
		done <- r
	}()
	select {
	case r := <-done:
		defer leave()
		if errors.Is(r.err, errActorStopped) {
			r.err = sl.whyStopped(event)
		}
		return r.rec, r.output, r.err
	case <-ctx.Done():
		go func() { <-done; leave() }()
		return ports.TransitionRecord{}, nil, ctx.Err()
	}
}

// whyStopped explains an actor that stopped before its step heard an outcome: one that refused an
// event in a final state answers that it is illegal there; any other stop (a failed save, or
// berthd stopping, either of which may come after the event was saved) is Unavailable, which no
// caller may mistake for an outcome. It never reads the store, where a later step may already
// have moved the entity on. Keep every follow up an effect returns inside its step's until list:
// a follow up outside it that ends the entity would stop the actor before the step heard an
// outcome, and the step would answer Unavailable while berthd runs.
func (sl *slot[S, E, F]) whyStopped(event E) error {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.refusedIn != "" {
		return machine.Illegal(sl.refusedIn, event)
	}
	return api.ErrUnavailable
}

// loadState reads an entity's persisted state, or initial when it has none yet. Only an actor's
// own goroutine may read its state, and actors save before they act, so this is current.
func loadState[S ~string](ctx context.Context, store ports.Store, entity string, initial S) (S, error) {
	st, found, err := store.LoadState(ctx, entity)
	if err != nil || !found {
		return initial, err
	}
	return S(st.State), nil
}

// entityID is the id within entity, for a machine whose entities entityOf names.
func entityID(entity string, entityOf func(string) string) string {
	return strings.TrimPrefix(entity, entityOf(""))
}

func waitApplied[S ~string, E machine.Event, F any](sl *slot[S, E, F], w chan applied, until []string) (ports.TransitionRecord, error) {
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
			// An actor that stopped on reaching its final state, or on refusing the event in one, told
			// the waiter first; a refusal is this step's answer, since a later step may already have
			// moved the entity on by the time stopped reads its state.
			for {
				select {
				case got := <-w:
					if got.err != nil {
						return ports.TransitionRecord{}, got.err
					}
					if slices.Contains(until, got.rec.Event) {
						return got.rec, nil
					}
				default:
					return ports.TransitionRecord{}, errActorStopped
				}
			}
		}
	}
}

// wait blocks until every step has finished or the timeout passes, and reports which.
func (a *actors[S, E, F]) wait(timeout time.Duration) bool {
	a.mu.Lock()
	idle := a.idle
	a.mu.Unlock()
	if idle == nil {
		return true
	}
	done, _ := awaitDone(a.ctx, idle, timeout)
	return done
}

// awaitDone waits until done is closed, wait passes or ctx ends, and reports whether done was
// closed; it returns ctx's error only when ctx ended first.
func awaitDone(ctx context.Context, done <-chan struct{}, wait time.Duration) (bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
