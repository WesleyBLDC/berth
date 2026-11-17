package browser_test

import (
	"errors"
	"slices"
	"testing"

	"berth/internal/core/browser"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// 1 Launch + 1 Started + 2 LaunchFailed + 2 CloseRequested + 1 CloseTimedOut + 3 Exited.
	if got := machinetest.Walk(t, browser.States, browser.AllEvents, browser.Transition); got != 10 {
		t.Fatalf("legal pairs = %d, want 10; update this number only with a deliberate table change", got)
	}
}

func step(t *testing.T, from browser.State, kind browser.EventKind, want browser.State, effects ...browser.EffectKind) {
	t.Helper()
	next, got, err := browser.Transition(from, browser.Event{Kind: kind})
	var kinds []browser.EffectKind
	for _, f := range got {
		kinds = append(kinds, f.Kind)
	}
	if err != nil || next != want || !slices.Equal(kinds, effects) {
		t.Fatalf("%s in %s: %s %v %v; want %s %v", kind, from, next, kinds, err, want, effects)
	}
}

func illegal(t *testing.T, from browser.State, kind browser.EventKind) {
	t.Helper()
	if _, _, err := browser.Transition(from, browser.Event{Kind: kind}); !errors.Is(err, machine.ErrIllegal) {
		t.Fatalf("%s in %s: want ErrIllegal, got %v", kind, from, err)
	}
}

func TestLaunchThenClose(t *testing.T) {
	step(t, browser.Initial, browser.Launch, browser.Launching, browser.Spawn)
	step(t, browser.Launching, browser.Started, browser.Ready)
	step(t, browser.Ready, browser.CloseRequested, browser.Closing, browser.CloseBrowser)
	step(t, browser.Closing, browser.CloseTimedOut, browser.Closing, browser.KillGroup)
	step(t, browser.Closing, browser.Exited, browser.Closed)
}

func TestLaunchFailsOrExits(t *testing.T) {
	step(t, browser.Launching, browser.LaunchFailed, browser.Closed)
	step(t, browser.Launching, browser.Exited, browser.Closed)
	step(t, browser.Ready, browser.Exited, browser.Closed)
	illegal(t, browser.Ready, browser.LaunchFailed)
	// Requested is never stored, so boot recovery never sends Exited to it.
	illegal(t, browser.Requested, browser.Exited)
}

// A close sent while Spawn runs is applied before Spawn's own follow up, so the browser goes to
// Closing; a late Started is refused there, and a late LaunchFailed ends it.
func TestCloseWhileLaunching(t *testing.T) {
	step(t, browser.Launching, browser.CloseRequested, browser.Closing, browser.CloseBrowser)
	illegal(t, browser.Closing, browser.Started)
	step(t, browser.Closing, browser.LaunchFailed, browser.Closed)
	illegal(t, browser.Closing, browser.CloseRequested)
}

func TestClosedIsTerminal(t *testing.T) {
	for _, e := range browser.AllEvents {
		illegal(t, browser.Closed, e.Kind)
	}
}

// State and event names are stored data; this pins them.
func TestNamesAreStable(t *testing.T) {
	var names []string
	for _, s := range browser.States {
		names = append(names, string(s))
	}
	for _, e := range browser.AllEvents {
		names = append(names, e.Name())
	}
	want := []string{"Requested", "Launching", "Ready", "Closing", "Closed", "Launch", "Started", "LaunchFailed", "CloseRequested", "CloseTimedOut", "Exited"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}
