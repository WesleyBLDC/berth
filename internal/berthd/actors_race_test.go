package berthd

import (
	"errors"
	"testing"

	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
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
