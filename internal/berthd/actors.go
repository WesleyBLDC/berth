package berthd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"berth/internal/core/machine"
	"berth/internal/core/ports"
)

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
	// busy counts steps whose event the actor has not finished with, so a stop can wait for them.
	busy int
}

type slot[S ~string, E machine.Event, F any] struct {
	actor *machine.Actor[S, E, F]
	// turn admits one step at a time, from before its event is sent until the actor has
	// finished with it, so a waiter only ever sees its own outcome and its own data.
	turn    chan struct{}
	stopped chan struct{}
	// cancel stops this actor alone, for evict.
	cancel context.CancelFunc

	mu     sync.Mutex
	waiter chan applied
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

// evict stops an entity's actor and forgets it, so an entity that reached a final state holds no
// goroutine; a later get reloads it from the store.
func (a *actors[S, E, F]) evict(entity string) {
	a.mu.Lock()
	sl := a.live[entity]
	delete(a.live, entity)
	a.mu.Unlock()
	if sl != nil {
		sl.cancel()
	}
}

// count is how many actors are running, for tests that check they stay bounded.
func (a *actors[S, E, F]) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
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
	sl, err := a.get(ctx, entity)
	if err != nil {
		return ports.TransitionRecord{}, nil, err
	}
	select {
	case sl.turn <- struct{}{}:
	case <-ctx.Done():
		return ports.TransitionRecord{}, nil, ctx.Err()
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
		return ports.TransitionRecord{}, nil, err
	}
	a.mu.Lock()
	a.busy++
	a.mu.Unlock()
	done := make(chan stepResult, 1)
	go func() {
		defer func() {
			a.mu.Lock()
			a.busy--
			a.mu.Unlock()
		}()
		rec, err := waitApplied(sl, w, names)
		r := release()
		r.rec, r.err = rec, err
		done <- r
	}()
	select {
	case r := <-done:
		return r.rec, r.output, r.err
	case <-ctx.Done():
		return ports.TransitionRecord{}, nil, ctx.Err()
	}
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
			return ports.TransitionRecord{}, fmt.Errorf("actor stopped")
		}
	}
}

// wait blocks until every step has finished or the timeout passes, and reports which.
func (a *actors[S, E, F]) wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		a.mu.Lock()
		idle := a.busy == 0
		a.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
