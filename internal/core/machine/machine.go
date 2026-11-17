// Package machine is the kit every Berth state machine is built on.
//
// A machine is a pure Transition function: given a state and an event it returns the next
// state and the effects to run, and does no I/O. An Actor owns one entity, applies events
// one at a time, persists each transition before running its effects, and feeds any
// follow up events back into its own queue.
package machine

import (
	"context"
	"errors"
	"fmt"

	"berth/internal/core/ports"
)

// ErrIllegal marks an event that is not allowed in the current state.
var ErrIllegal = errors.New("illegal transition")

// Event is anything a machine reacts to. Name is what the audit log records.
type Event interface {
	Name() string
}

// Detailed events add a short, secret free note to their audit row.
type Detailed interface {
	Detail() string
}

// Transition is a pure function. On an illegal event it returns the unchanged state and an
// error wrapping ErrIllegal.
type Transition[S ~string, E Event, F any] func(state S, event E) (S, []F, error)

// Illegal builds the standard illegal transition error.
func Illegal[S ~string, E Event](state S, event E) error {
	return fmt.Errorf("%w: %s in %s", ErrIllegal, event.Name(), state)
}

// Runner performs one effect and may return follow up events for the same entity.
type Runner[E Event, F any] func(ctx context.Context, effect F) ([]E, error)

// Actor owns one entity. Only its goroutine reads or changes the state.
type Actor[S ~string, E Event, F any] struct {
	entity     string
	kind       string
	state      S
	version    int64
	transition Transition[S, E, F]
	run        Runner[E, F]
	store      ports.Store
	clock      ports.Clock
	inbox      chan E
	rejected   func(error)
}

// Config wires an actor to its machine and the outside world.
type Config[S ~string, E Event, F any] struct {
	Entity     string
	Kind       string
	Initial    S
	Transition Transition[S, E, F]
	Run        Runner[E, F]
	Store      ports.Store
	Clock      ports.Clock
	// Rejected is told about illegal events; they never stop the actor.
	Rejected func(error)
}

// NewActor loads the entity's persisted state, or starts it at Initial.
func NewActor[S ~string, E Event, F any](ctx context.Context, cfg Config[S, E, F]) (*Actor[S, E, F], error) {
	a := &Actor[S, E, F]{
		entity:     cfg.Entity,
		kind:       cfg.Kind,
		state:      cfg.Initial,
		transition: cfg.Transition,
		run:        cfg.Run,
		store:      cfg.Store,
		clock:      cfg.Clock,
		inbox:      make(chan E, 64),
		rejected:   cfg.Rejected,
	}
	saved, found, err := cfg.Store.LoadState(ctx, cfg.Entity)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cfg.Entity, err)
	}
	if found {
		if saved.Kind != cfg.Kind {
			return nil, fmt.Errorf("entity %s is a %s, not a %s", cfg.Entity, saved.Kind, cfg.Kind)
		}
		a.state = S(saved.State)
		a.version = saved.Version
	}
	return a, nil
}

// State is only safe to call before Run starts or from inside effects.
func (a *Actor[S, E, F]) State() S { return a.state }

// Send queues an event. It blocks when the inbox is full, which applies back pressure.
func (a *Actor[S, E, F]) Send(ctx context.Context, event E) error {
	select {
	case a.inbox <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run applies events until ctx ends. It returns early only when persisting fails,
// because a state that cannot be recorded must not be acted on.
func (a *Actor[S, E, F]) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-a.inbox:
			if err := a.apply(ctx, event); err != nil {
				return err
			}
		}
	}
}

// apply runs one event: transition, persist, then effects.
func (a *Actor[S, E, F]) apply(ctx context.Context, event E) error {
	next, effects, err := a.transition(a.state, event)
	if err != nil {
		if errors.Is(err, ErrIllegal) {
			if a.rejected != nil {
				a.rejected(err)
			}
			return nil
		}
		return err
	}
	rec := ports.TransitionRecord{
		Entity:  a.entity,
		Kind:    a.kind,
		Version: a.version + 1,
		Event:   event.Name(),
		From:    string(a.state),
		To:      string(next),
		At:      a.clock.Now(),
	}
	if d, ok := any(event).(Detailed); ok {
		rec.Detail = d.Detail()
	}
	if err := a.store.SaveTransition(ctx, rec); err != nil {
		return fmt.Errorf("save %s %s: %w", a.entity, event.Name(), err)
	}
	a.state, a.version = next, rec.Version
	for _, effect := range effects {
		followUps, err := a.run(ctx, effect)
		if err != nil {
			return fmt.Errorf("effect for %s %s: %w", a.entity, event.Name(), err)
		}
		for _, f := range followUps {
			if err := a.Send(ctx, f); err != nil {
				return err
			}
		}
	}
	return nil
}
