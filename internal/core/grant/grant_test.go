package grant_test

import (
	"errors"
	"testing"

	"berth/internal/core/grant"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// GrantRequested + Approved + Denied + Failed + UseAuthorized + HashChanged + 2 Revoke
	// + 3 UseRefused.
	if got := machinetest.Walk(t, grant.States, grant.AllEvents, grant.Transition); got != 11 {
		t.Fatalf("legal pairs = %d, want 11; update this number only with a deliberate table change", got)
	}
}

func step(t *testing.T, s grant.State, kind grant.EventKind, want grant.State) []grant.Effect {
	t.Helper()
	next, effects, err := grant.Transition(s, grant.Event{Kind: kind})
	if err != nil || next != want {
		t.Fatalf("%s in %s: got %s, %v; want %s", kind, s, next, err, want)
	}
	return effects
}

func TestGrantHappyPath(t *testing.T) {
	effects := step(t, grant.Initial, grant.GrantRequested, grant.Requested)
	if len(effects) != 1 || effects[0].Kind != grant.PromptPower {
		t.Fatalf("GrantRequested effects = %v, want PromptPower", effects)
	}
	step(t, grant.Requested, grant.GrantApproved, grant.Active)
	effects = step(t, grant.Active, grant.UseAuthorized, grant.Active)
	if len(effects) != 1 || effects[0].Kind != grant.IssueTicket {
		t.Fatalf("UseAuthorized effects = %v, want IssueTicket", effects)
	}
}

func TestDeniedOrFailedPromptEndsDenied(t *testing.T) {
	step(t, grant.Requested, grant.GrantDenied, grant.Denied)
	step(t, grant.Requested, grant.GrantFailed, grant.Denied)
}

func TestHashChangeMakesStaleAndStaleNeedsNewGrant(t *testing.T) {
	step(t, grant.Active, grant.HashChanged, grant.Stale)
	if _, _, err := grant.Transition(grant.Stale, grant.Event{Kind: grant.GrantApproved}); !errors.Is(err, machine.ErrIllegal) {
		t.Fatalf("GrantApproved on Stale: want ErrIllegal, got %v", err)
	}
}

func TestUseRefusedRecordedWithoutChange(t *testing.T) {
	for _, s := range []grant.State{grant.Stale, grant.Denied, grant.Revoked} {
		if effects := step(t, s, grant.UseRefused, s); len(effects) != 0 {
			t.Errorf("UseRefused on %s emitted %v", s, effects)
		}
	}
}

func TestRevoke(t *testing.T) {
	step(t, grant.Active, grant.Revoke, grant.Revoked)
	step(t, grant.Stale, grant.Revoke, grant.Revoked)
}

func TestIssueTicketOnlyFromActive(t *testing.T) {
	for _, s := range grant.States {
		for _, e := range grant.AllEvents {
			_, effects, _ := grant.Transition(s, e)
			for _, f := range effects {
				if f.Kind == grant.IssueTicket && (s != grant.Active || e.Kind != grant.UseAuthorized) {
					t.Errorf("IssueTicket from %s on %s", s, e.Kind)
				}
			}
		}
	}
	for _, s := range []grant.State{grant.Requested, grant.Denied, grant.Stale, grant.Revoked} {
		if _, _, err := grant.Transition(s, grant.Event{Kind: grant.UseAuthorized}); !errors.Is(err, machine.ErrIllegal) {
			t.Errorf("UseAuthorized on %s: want ErrIllegal, got %v", s, err)
		}
	}
}
