package secret_test

import (
	"errors"
	"testing"

	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/secret"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// 3 StoreRequested + StoreSucceeded + StoreDenied + StoreFailed + UseRefused.
	if got := machinetest.Walk(t, secret.States, secret.AllEvents, secret.Transition); got != 7 {
		t.Fatalf("legal pairs = %d, want 7; update this number only with a deliberate table change", got)
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
