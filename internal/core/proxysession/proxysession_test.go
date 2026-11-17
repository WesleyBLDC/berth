package proxysession_test

import (
	"errors"
	"slices"
	"testing"

	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/proxysession"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// ProxyReady + 2 StopRequested + 3 ProxyEnded + 3 ProxyLost.
	if got := machinetest.Walk(t, proxysession.States, proxysession.AllEvents, proxysession.Transition); got != 9 {
		t.Fatalf("legal pairs = %d, want 9; update this number only with a deliberate table change", got)
	}
}

func TestServeThenEnd(t *testing.T) {
	next, effects, err := proxysession.Transition(proxysession.Initial, proxysession.Event{Kind: proxysession.ProxyReady})
	if err != nil || next != proxysession.Serving || len(effects) != 0 {
		t.Fatalf("ProxyReady: %s %v %v", next, effects, err)
	}
	next, effects, err = proxysession.Transition(next, proxysession.Event{Kind: proxysession.ProxyEnded})
	if err != nil || next != proxysession.Ended || len(effects) != 0 {
		t.Fatalf("ProxyEnded: %s %v %v", next, effects, err)
	}
}

func TestStopSendsTheReason(t *testing.T) {
	for _, from := range []proxysession.State{proxysession.Authorized, proxysession.Serving} {
		for _, reason := range []proxysession.StopReason{proxysession.StopGrantRevoked, proxysession.StopSecretRemoved, proxysession.StopDaemonStopping} {
			next, effects, err := proxysession.Transition(from, proxysession.Event{Kind: proxysession.StopRequested, Reason: reason})
			want := []proxysession.Effect{{Kind: proxysession.SendStop, Reason: reason}}
			if err != nil || next != proxysession.Stopping || !slices.Equal(effects, want) {
				t.Fatalf("StopRequested(%s) in %s: %s %v %v", reason, from, next, effects, err)
			}
		}
	}
}

func TestEndOrLossFromEveryLiveState(t *testing.T) {
	cases := map[proxysession.EventKind]proxysession.State{proxysession.ProxyEnded: proxysession.Ended, proxysession.ProxyLost: proxysession.Lost}
	for kind, want := range cases {
		for _, from := range []proxysession.State{proxysession.Authorized, proxysession.Serving, proxysession.Stopping} {
			next, effects, err := proxysession.Transition(from, proxysession.Event{Kind: kind})
			if err != nil || next != want || len(effects) != 0 {
				t.Fatalf("%s in %s: %s %v %v", kind, from, next, effects, err)
			}
		}
	}
}

func TestEndedAndLostAreTerminal(t *testing.T) {
	for _, from := range []proxysession.State{proxysession.Ended, proxysession.Lost} {
		for _, e := range proxysession.AllEvents {
			next, effects, err := proxysession.Transition(from, e)
			if !errors.Is(err, machine.ErrIllegal) || next != from || effects != nil {
				t.Fatalf("%s in %s: want ErrIllegal, got %s %v %v", e.Name(), from, next, effects, err)
			}
		}
	}
}

func TestReadyOnlyOnceAndStopOnlyOnce(t *testing.T) {
	for _, c := range []struct {
		from proxysession.State
		e    proxysession.Event
	}{
		{proxysession.Serving, proxysession.Event{Kind: proxysession.ProxyReady}},
		{proxysession.Stopping, proxysession.Event{Kind: proxysession.ProxyReady}},
		{proxysession.Stopping, proxysession.Event{Kind: proxysession.StopRequested, Reason: proxysession.StopGrantRevoked}},
	} {
		next, _, err := proxysession.Transition(c.from, c.e)
		if !errors.Is(err, machine.ErrIllegal) || next != c.from {
			t.Fatalf("%s in %s: want ErrIllegal, got %s %v", c.e.Name(), c.from, next, err)
		}
	}
}

// State and event names are stored data, so a rename must fail here first.
func TestNamesAreStable(t *testing.T) {
	var got []string
	for _, s := range proxysession.States {
		got = append(got, string(s))
	}
	for _, e := range proxysession.AllEvents {
		got = append(got, e.Name())
	}
	want := []string{"Authorized", "Serving", "Stopping", "Ended", "Lost", "ProxyReady", "StopRequested", "ProxyEnded", "ProxyLost"}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if proxysession.Entity("ab") != "proxy/ab" || proxysession.Initial != proxysession.Authorized {
		t.Fatal("entity or initial state changed")
	}
}
