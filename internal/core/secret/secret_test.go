package secret_test

import (
	"errors"
	"slices"
	"testing"

	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/secret"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// 4 StoreRequested + StoreSucceeded + StoreDenied + StoreFailed + 2 UseRefused
	// + RotateRequested + 3 rotate outcomes + RemoveRequested + RemoveSucceeded + 2 remove outcomes.
	if got := machinetest.Walk(t, secret.States, secret.AllEvents, secret.Transition); got != 17 {
		t.Fatalf("legal pairs = %d, want 17; update this number only with a deliberate table change", got)
	}
}

func TestStoreHappyPath(t *testing.T) {
	next, effects, err := secret.Transition(secret.Initial, secret.Event{Kind: secret.StoreRequested})
	if err != nil || next != secret.Pending || len(effects) != 1 || effects[0].Kind != secret.PromptStore {
		t.Fatalf("StoreRequested: %s %v %v", next, effects, err)
	}
	next, effects, err = secret.Transition(next, secret.Event{Kind: secret.StoreSucceeded})
	if err != nil || next != secret.Stored || len(effects) != 0 {
		t.Fatalf("StoreSucceeded: %s %v %v", next, effects, err)
	}
}

func TestRetryAfterDenialOrFailure(t *testing.T) {
	for _, outcome := range []secret.EventKind{secret.StoreDenied, secret.StoreFailed} {
		s, _, err := secret.Transition(secret.Pending, secret.Event{Kind: outcome})
		if err != nil {
			t.Fatalf("%s: %v", outcome, err)
		}
		next, effects, err := secret.Transition(s, secret.Event{Kind: secret.StoreRequested})
		if err != nil || next != secret.Pending || len(effects) != 1 || effects[0].Kind != secret.PromptStore {
			t.Errorf("retry from %s: %s %v %v", s, next, effects, err)
		}
	}
}

func TestStoredRefusesSecondStore(t *testing.T) {
	next, effects, err := secret.Transition(secret.Stored, secret.Event{Kind: secret.StoreRequested})
	if !errors.Is(err, machine.ErrIllegal) || next != secret.Stored || effects != nil {
		t.Fatalf("want ErrIllegal staying Stored, got %s %v %v", next, effects, err)
	}
}

func TestUseRefusedRecordedOnStored(t *testing.T) {
	next, effects, err := secret.Transition(secret.Stored, secret.Event{Kind: secret.UseRefused})
	if err != nil || next != secret.Stored || len(effects) != 0 {
		t.Fatalf("UseRefused on Stored: %s %v %v", next, effects, err)
	}
}

func step(t *testing.T, from secret.State, kind secret.EventKind, want secret.State, wantEffects ...secret.EffectKind) {
	t.Helper()
	next, effects, err := secret.Transition(from, secret.Event{Kind: kind})
	if err != nil || next != want {
		t.Fatalf("%s in %s: got %s %v, want %s", kind, from, next, err, want)
	}
	var got []secret.EffectKind
	for _, e := range effects {
		got = append(got, e.Kind)
	}
	if !slices.Equal(got, wantEffects) {
		t.Fatalf("%s in %s: effects %v, want %v", kind, from, got, wantEffects)
	}
}

func illegal(t *testing.T, from secret.State, kind secret.EventKind) {
	t.Helper()
	next, effects, err := secret.Transition(from, secret.Event{Kind: kind})
	if !errors.Is(err, machine.ErrIllegal) || next != from || effects != nil {
		t.Fatalf("%s in %s: want ErrIllegal, got %s %v %v", kind, from, next, effects, err)
	}
}

func TestRotateReturnsToStoredWhateverTheOutcome(t *testing.T) {
	for _, outcome := range []secret.EventKind{secret.RotateSucceeded, secret.RotateDenied, secret.RotateFailed} {
		step(t, secret.Stored, secret.RotateRequested, secret.Rotating, secret.PromptRotate)
		step(t, secret.Rotating, outcome, secret.Stored)
	}
}

func TestRotateOnlyFromStored(t *testing.T) {
	for _, s := range []secret.State{secret.Pending, secret.Denied, secret.Failed, secret.Rotating, secret.Removing, secret.Removed} {
		illegal(t, s, secret.RotateRequested)
	}
	illegal(t, secret.Stored, secret.RotateSucceeded)
}

func TestRemoveSucceededRevokesAll(t *testing.T) {
	step(t, secret.Stored, secret.RemoveRequested, secret.Removing, secret.PromptRemove)
	step(t, secret.Removing, secret.RemoveSucceeded, secret.Removed, secret.RevokeAll)
}

func TestRemoveDeniedOrFailedKeepsTheSecret(t *testing.T) {
	for _, outcome := range []secret.EventKind{secret.RemoveDenied, secret.RemoveFailed} {
		step(t, secret.Removing, outcome, secret.Stored)
	}
}

func TestRemoveOnlyFromStored(t *testing.T) {
	// Pending, Rotating and Removing answer BUSY in the API; the table refuses them too.
	for _, s := range []secret.State{secret.Pending, secret.Denied, secret.Failed, secret.Rotating, secret.Removing, secret.Removed} {
		illegal(t, s, secret.RemoveRequested)
	}
	illegal(t, secret.Stored, secret.RemoveSucceeded)
}

func TestRemovedAcceptsANewStore(t *testing.T) {
	step(t, secret.Removed, secret.StoreRequested, secret.Pending, secret.PromptStore)
	step(t, secret.Pending, secret.StoreSucceeded, secret.Stored)
}

func TestRotatingAndRemovingRefuseAStore(t *testing.T) {
	illegal(t, secret.Rotating, secret.StoreRequested)
	illegal(t, secret.Removing, secret.StoreRequested)
}

func TestUseRefusedRecordedWhileRotating(t *testing.T) {
	step(t, secret.Rotating, secret.UseRefused, secret.Rotating)
}

func TestRemovingAndRemovedRefuseUse(t *testing.T) {
	for _, s := range []secret.State{secret.Removing, secret.Removed, secret.Pending} {
		illegal(t, s, secret.UseRefused)
	}
}
