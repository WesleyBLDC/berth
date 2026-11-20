package berthd

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"

	"berth/internal/adapters/api"
	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// Found by the model walk, seed 1795143668884585000: a step whose event an actor refused in a
// final state could see the actor stop before reading the refusal, and then answer from the
// state a later step had already moved on (Unavailable), instead of its own refusal.
func TestWaitAppliedAnswersARefusalToldBeforeTheStop(t *testing.T) {
	refusal := machine.Illegal(agentsession.Revoked, agentsession.Event{Kind: agentsession.Bind})
	for range 200 {
		sl := &slot[agentsession.State, agentsession.Event, agentsession.Effect]{stopped: make(chan struct{})}
		close(sl.stopped)
		w := make(chan applied, 1)
		w <- applied{err: refusal}
		if _, err := waitApplied(sl, w, []string{string(agentsession.Bind)}); !errors.Is(err, machine.ErrIllegal) {
			t.Fatalf("waitApplied = %v, want the refusal", err)
		}
	}
}

// Found by the model walk, seed 1795143902571008000: a caller that gave up before its event was
// sent left the fresh actor it started for an ended entity idling until berthd stopped.
func TestStepGivenUpLeavesNoActorForAnEndedEntity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := testutil.NewStore()
		kit := &actors[agentsession.State, agentsession.Event, agentsession.Effect]{
			ctx: t.Context(), log: slog.New(slog.DiscardHandler), store: store, clock: testutil.NewClock(),
			kind: agentsession.Kind, initial: agentsession.Initial, transition: agentsession.Transition,
			final: []agentsession.State{agentsession.Expired, agentsession.Revoked, agentsession.Ended},
			run: func(string) machine.Runner[agentsession.Event, agentsession.Effect] {
				return func(context.Context, agentsession.Effect) ([]agentsession.Event, error) { return nil, nil }
			},
			live: map[string]*slot[agentsession.State, agentsession.Event, agentsession.Effect]{},
		}
		entity := agentsession.Entity("a1")
		if _, _, err := step(t.Context(), kit, entity, agentsession.Event{Kind: agentsession.Revoke}, nil, agentsession.Revoke); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for i := range 50 {
			_, _, err := step(ctx, kit, entity, agentsession.Event{Kind: agentsession.Bind}, nil, agentsession.Bind)
			if !errors.Is(err, context.Canceled) && !errors.Is(err, machine.ErrIllegal) {
				t.Fatalf("call %d: %v", i, err)
			}
			synctest.Wait()
			if n := kit.count(); n != 0 {
				t.Fatalf("call %d left %d actors for an ended entity", i, n)
			}
		}
		if st, _, _ := store.LoadState(t.Context(), entity); st.State != string(agentsession.Revoked) || st.Version != 1 {
			t.Fatalf("state = %+v, want Revoked v1", st)
		}
	})
}

// A tiny machine for the stop tests: X moves A to B and runs an effect; C is final.
type stopState string

type stopEvent struct{ kind string }

func (e stopEvent) Name() string { return e.kind }

func stopTransition(s stopState, e stopEvent) (stopState, []string, error) {
	if e.kind == "X" && s == "A" {
		return "B", []string{"F"}, nil
	}
	return s, nil, machine.Illegal(s, e)
}

// Found in review of the model walk: an actor that saved a step's event and then stopped without
// telling it (here its effect fails; a stopping berthd does the same) answered from the store,
// which a later step had moved to a final state, so the caller heard Illegal for a saved event.
// It must answer Unavailable, and a stopping berthd starts no actor at all.
func TestStepWhoseActorStoppedAfterSavingAnswersUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := testutil.NewStore()
		ctx, stop := context.WithCancel(t.Context())
		entity := "stop/1"
		kit := &actors[stopState, stopEvent, string]{
			ctx: ctx, log: slog.New(slog.DiscardHandler), store: store, clock: testutil.NewClock(),
			kind: "stop", initial: "A", transition: stopTransition, final: []stopState{"C"},
			run: func(string) machine.Runner[stopEvent, string] {
				return func(ctx context.Context, _ string) ([]stopEvent, error) {
					// A later actor ends the entity before this one's failure is explained.
					if err := store.SaveTransition(ctx, ports.TransitionRecord{Entity: entity, Kind: "stop", Version: 2, Event: "Y", From: "B", To: "C"}); err != nil {
						return nil, err
					}
					return nil, errors.New("effect failed")
				}
			},
			live: map[string]*slot[stopState, stopEvent, string]{},
		}
		_, _, err := step(t.Context(), kit, entity, stopEvent{"X"}, nil, "X")
		if !errors.Is(err, api.ErrUnavailable) {
			t.Fatalf("step = %v, want Unavailable for an event that was saved", err)
		}
		if rows := store.Events(); len(rows) != 2 || rows[0].Event != "X" {
			t.Fatalf("rows = %+v, want X saved then Y", rows)
		}
		stop()
		synctest.Wait()
		if _, _, err := step(t.Context(), kit, "stop/2", stopEvent{"X"}, nil, "X"); !errors.Is(err, api.ErrUnavailable) {
			t.Fatalf("step after stop = %v, want Unavailable", err)
		}
		if n := kit.count(); n != 0 {
			t.Fatalf("a stopped kit started %d actors", n)
		}
	})
}
