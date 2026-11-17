package daemon_test

import (
	"errors"
	"testing"

	"berth/internal/core/daemon"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// 5 Boot pairs + Recovered + 2 StopRequested + Drained + 3 Failed.
	if got := machinetest.Walk(t, daemon.States, daemon.AllEvents, daemon.Transition); got != 12 {
		t.Fatalf("legal pairs = %d, want 12; update this number only with a deliberate table change", got)
	}
}

func TestBootReportsUncleanShutdown(t *testing.T) {
	for _, tc := range []struct {
		from    daemon.State
		unclean bool
	}{
		{daemon.Starting, false},
		{daemon.Stopped, false},
		{daemon.Serving, true},
		{daemon.Recovering, true},
		{daemon.Draining, true},
	} {
		next, effects, err := daemon.Transition(tc.from, daemon.Event{Kind: daemon.Boot})
		if err != nil || next != daemon.Recovering || len(effects) != 1 {
			t.Fatalf("Boot from %s: %s %v %v", tc.from, next, effects, err)
		}
		if effects[0].Unclean != tc.unclean || effects[0].Previous != tc.from {
			t.Errorf("Boot from %s: effect %+v, want unclean %v", tc.from, effects[0], tc.unclean)
		}
	}
}

func TestHappyPath(t *testing.T) {
	s := daemon.Starting
	for _, step := range []struct {
		event daemon.EventKind
		want  daemon.State
	}{
		{daemon.Boot, daemon.Recovering},
		{daemon.Recovered, daemon.Serving},
		{daemon.StopRequested, daemon.Draining},
		{daemon.Drained, daemon.Stopped},
	} {
		next, _, err := daemon.Transition(s, daemon.Event{Kind: step.event})
		if err != nil || next != step.want {
			t.Fatalf("%s in %s: got %s, %v; want %s", step.event, s, next, err, step.want)
		}
		s = next
	}
}

func TestServingIgnoresSecondRecovered(t *testing.T) {
	_, _, err := daemon.Transition(daemon.Serving, daemon.Event{Kind: daemon.Recovered})
	if !errors.Is(err, machine.ErrIllegal) {
		t.Fatalf("want ErrIllegal, got %v", err)
	}
}
