// Package machinetest checks state machines exhaustively.
package machinetest

import (
	"errors"
	"slices"
	"testing"

	"berth/internal/core/machine"
)

// Walk calls the transition for every state and event pair. Every pair must either move to a
// known state or be rejected with ErrIllegal and leave the state unchanged; anything else, or a
// panic, fails the test. It returns the number of legal pairs so tests can pin the table size.
func Walk[S ~string, E machine.Event, F any](t *testing.T, states []S, events []E, transition machine.Transition[S, E, F]) int {
	t.Helper()
	legal := 0
	for _, s := range states {
		for _, e := range events {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s in %s panicked: %v", e.Name(), s, r)
					}
				}()
				next, _, err := transition(s, e)
				switch {
				case err == nil:
					if !slices.Contains(states, next) {
						t.Errorf("%s in %s moved to unknown state %q", e.Name(), s, next)
					}
					legal++
				case errors.Is(err, machine.ErrIllegal):
					if next != s {
						t.Errorf("illegal %s in %s changed state to %s", e.Name(), s, next)
					}
				default:
					t.Errorf("%s in %s returned a non ErrIllegal error: %v", e.Name(), s, err)
				}
			}()
		}
	}
	return legal
}
