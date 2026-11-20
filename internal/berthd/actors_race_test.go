package berthd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
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
		if !testutil.Eventually(func() bool { return kit.count() == 0 }) {
			t.Fatalf("call %d left %d actors for an ended entity", i, kit.count())
		}
	}
	if st, _, _ := store.LoadState(t.Context(), entity); st.State != string(agentsession.Revoked) || st.Version != 1 {
		t.Fatalf("state = %+v, want Revoked v1", st)
	}
}
