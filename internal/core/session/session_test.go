package session_test

import (
	"errors"
	"slices"
	"testing"

	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/session"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// 1 Seen + 2 Busy + 2 Quiet + 3 OwnerExited + 4 StopRequested + 1 StopTimedOut + 5 Exited.
	if got := machinetest.Walk(t, session.States, session.AllEvents, session.Transition); got != 18 {
		t.Fatalf("legal pairs = %d, want 18; update this number only with a deliberate table change", got)
	}
}

func step(t *testing.T, from session.State, kind session.EventKind, want session.State, effects ...session.EffectKind) {
	t.Helper()
	next, got, err := session.Transition(from, session.Event{Kind: kind})
	var kinds []session.EffectKind
	for _, f := range got {
		kinds = append(kinds, f.Kind)
	}
	if err != nil || next != want || !slices.Equal(kinds, effects) {
		t.Fatalf("%s in %s: %s %v %v; want %s %v", kind, from, next, kinds, err, want, effects)
	}
}

func TestObservedLife(t *testing.T) {
	step(t, session.Initial, session.Busy, session.Active)
	step(t, session.Active, session.Quiet, session.Idle)
	step(t, session.Idle, session.Busy, session.Active)
	step(t, session.Active, session.OwnerExited, session.Leftover)
	step(t, session.Leftover, session.Exited, session.Stopped)
}

// Seen only records a session's first sight: it changes nothing and is legal only before any
// other event.
func TestSeenOnlyInDiscovered(t *testing.T) {
	step(t, session.Discovered, session.Seen, session.Discovered)
	for _, from := range []session.State{session.Active, session.Idle, session.Leftover, session.Stopping, session.Stopped} {
		if _, _, err := session.Transition(from, session.Event{Kind: session.Seen}); !errors.Is(err, machine.ErrIllegal) {
			t.Fatalf("Seen in %s: want ErrIllegal, got %v", from, err)
		}
	}
}

func TestStopThenKill(t *testing.T) {
	for _, from := range []session.State{session.Discovered, session.Active, session.Idle, session.Leftover} {
		step(t, from, session.StopRequested, session.Stopping, session.SignalTerm)
	}
	step(t, session.Stopping, session.StopTimedOut, session.Stopping, session.SignalKill)
	step(t, session.Stopping, session.Exited, session.Stopped)
}

func TestLeftoverIsSticky(t *testing.T) {
	for _, kind := range []session.EventKind{session.Busy, session.Quiet, session.OwnerExited} {
		next, _, err := session.Transition(session.Leftover, session.Event{Kind: kind})
		if !errors.Is(err, machine.ErrIllegal) || next != session.Leftover {
			t.Fatalf("%s in Leftover: want ErrIllegal, got %s %v", kind, next, err)
		}
	}
}

func TestStoppedIsTerminalAndStoppingIgnoresObservations(t *testing.T) {
	for _, e := range session.AllEvents {
		if next, effects, err := session.Transition(session.Stopped, e); !errors.Is(err, machine.ErrIllegal) || next != session.Stopped || effects != nil {
			t.Fatalf("%s in Stopped: want ErrIllegal, got %s %v %v", e.Name(), next, effects, err)
		}
	}
	for _, kind := range []session.EventKind{session.Busy, session.Quiet, session.OwnerExited, session.StopRequested} {
		if next, _, err := session.Transition(session.Stopping, session.Event{Kind: kind}); !errors.Is(err, machine.ErrIllegal) || next != session.Stopping {
			t.Fatalf("%s in Stopping: want ErrIllegal, got %s %v", kind, next, err)
		}
	}
}

// State and event names are stored data, so a rename must fail here first.
func TestNamesAreStable(t *testing.T) {
	var got []string
	for _, s := range session.States {
		got = append(got, string(s))
	}
	for _, e := range session.AllEvents {
		got = append(got, e.Name())
	}
	want := []string{
		"Discovered", "Active", "Idle", "Leftover", "Stopping", "Stopped",
		"Seen", "Busy", "Quiet", "OwnerExited", "StopRequested", "StopTimedOut", "Exited",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if session.Entity("12-34") != "session/12-34" || session.Initial != session.Discovered {
		t.Fatal("entity or initial state changed")
	}
}
