package machine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"berth/internal/core/machine"
	"berth/internal/core/ports"
	"berth/internal/testutil"
)

type light string

const (
	off light = "Off"
	on  light = "On"
)

type press struct{ kind string }

func (p press) Name() string { return p.kind }

type blink struct{}

func lightTransition(s light, e press) (light, []blink, error) {
	switch {
	case e.kind == "Press" && s == off:
		return on, []blink{{}}, nil
	case e.kind == "Press" && s == on:
		return off, nil, nil
	}
	return s, nil, machine.Illegal(s, e)
}

func newActor(t *testing.T, store ports.Store, run machine.Runner[press, blink], rejected func(error)) *machine.Actor[light, press, blink] {
	t.Helper()
	a, err := machine.NewActor(context.Background(), machine.Config[light, press, blink]{
		Entity: "lamp", Kind: "light", Initial: off, Transition: lightTransition,
		Run: run, Store: store, Clock: testutil.NewClock(), Rejected: rejected,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runFor(t *testing.T, a *machine.Actor[light, press, blink], events ...press) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for _, e := range events {
		if err := a.Send(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return a.Run(ctx)
}

func TestTransitionIsPersistedBeforeItsEffectRuns(t *testing.T) {
	store := testutil.NewStore()
	var savedWhenEffectRan int
	a := newActor(t, store, func(context.Context, blink) ([]press, error) {
		savedWhenEffectRan = len(store.Events())
		return nil, nil
	}, nil)
	if err := runFor(t, a, press{"Press"}); err != nil {
		t.Fatal(err)
	}
	if savedWhenEffectRan != 1 {
		t.Fatalf("effect ran with %d saved transitions, want 1", savedWhenEffectRan)
	}
}

func TestFollowUpEventsAreApplied(t *testing.T) {
	store := testutil.NewStore()
	a := newActor(t, store, func(context.Context, blink) ([]press, error) { return []press{{"Press"}}, nil }, nil)
	if err := runFor(t, a, press{"Press"}); err != nil {
		t.Fatal(err)
	}
	ev := store.Events()
	if len(ev) != 2 || ev[0].To != "On" || ev[1].To != "Off" || ev[1].Version != 2 {
		t.Fatalf("events: %+v", ev)
	}
}

func TestIllegalEventIsRejectedAndNotPersisted(t *testing.T) {
	store := testutil.NewStore()
	var rejected []error
	a := newActor(t, store, func(context.Context, blink) ([]press, error) { return nil, nil }, func(err error) { rejected = append(rejected, err) })
	if err := runFor(t, a, press{"Kick"}); err != nil {
		t.Fatal(err)
	}
	if len(store.Events()) != 0 || len(rejected) != 1 || !errors.Is(rejected[0], machine.ErrIllegal) {
		t.Fatalf("events %d, rejected %v", len(store.Events()), rejected)
	}
}

func TestFailedSaveStopsTheActorWithoutRunningEffects(t *testing.T) {
	store := testutil.NewStore()
	store.FailSave = errors.New("disk full")
	ran := false
	a := newActor(t, store, func(context.Context, blink) ([]press, error) { ran = true; return nil, nil }, nil)
	if err := runFor(t, a, press{"Press"}); err == nil || ran {
		t.Fatalf("err %v, effect ran %v", err, ran)
	}
	if a.State() != off {
		t.Fatalf("state moved to %s although it was not saved", a.State())
	}
}

func TestActorResumesFromPersistedState(t *testing.T) {
	store := testutil.NewStore()
	first := newActor(t, store, func(context.Context, blink) ([]press, error) { return nil, nil }, nil)
	if err := runFor(t, first, press{"Press"}); err != nil {
		t.Fatal(err)
	}
	second := newActor(t, store, func(context.Context, blink) ([]press, error) { return nil, nil }, nil)
	if second.State() != on {
		t.Fatalf("resumed in %s, want On", second.State())
	}
}

func TestAppliedIsToldAfterEffectsRun(t *testing.T) {
	store := testutil.NewStore()
	var order []string
	a, err := machine.NewActor(context.Background(), machine.Config[light, press, blink]{
		Entity: "lamp", Kind: "light", Initial: off, Transition: lightTransition,
		Run:   func(context.Context, blink) ([]press, error) { order = append(order, "effect"); return nil, nil },
		Store: store, Clock: testutil.NewClock(),
		Applied: func(rec ports.TransitionRecord) { order = append(order, "applied "+rec.Event+" to "+rec.To) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runFor(t, a, press{"Press"}, press{"Nope"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ", "); got != "effect, applied Press to On" {
		t.Fatalf("order: %s", got)
	}
}
