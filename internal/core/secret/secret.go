// Package secret is the state machine for one stored secret.
package secret

import "berth/internal/core/machine"

const Kind = "secret"

// Entity names the machine instance for a secret, for example secret/github-token.
func Entity(name string) string { return "secret/" + name }

type State string

const (
	// Pending is a secret whose store dialog is open, and the state of a new secret.
	Pending State = "Pending"
	Stored  State = "Stored"
	Denied  State = "Denied"
	Failed  State = "Failed"
	// Rotating is a Stored secret whose replace dialog is open; it still serves uses.
	Rotating State = "Rotating"
	// Removing is a secret whose remove confirmation is open; it refuses uses.
	Removing State = "Removing"
	// Removed is a secret whose Keychain item is gone; a new store may reuse the name.
	Removed State = "Removed"
)

var States = []State{Pending, Stored, Denied, Failed, Rotating, Removing, Removed}

// Initial is the state of a secret the daemon has not seen before.
const Initial = Pending

type EventKind string

const (
	// StoreRequested comes from RequestSecret, for both berth secret request and add.
	StoreRequested EventKind = "StoreRequested"
	// StoreSucceeded, StoreDenied and StoreFailed are the Approver.StoreSecret outcomes.
	StoreSucceeded EventKind = "StoreSucceeded"
	StoreDenied    EventKind = "StoreDenied"
	StoreFailed    EventKind = "StoreFailed"
	// UseRefused records an AuthorizeUse that found no grant to record it on.
	UseRefused EventKind = "UseRefused"
	// RotateRequested comes from RotateSecret; RotateSucceeded, RotateDenied and RotateFailed
	// are the Approver.RotateSecret outcomes.
	RotateRequested EventKind = "RotateRequested"
	RotateSucceeded EventKind = "RotateSucceeded"
	RotateDenied    EventKind = "RotateDenied"
	RotateFailed    EventKind = "RotateFailed"
	// RemoveRequested comes from RemoveSecret; RemoveSucceeded, RemoveDenied and RemoveFailed
	// are the Approver.RemoveSecret outcomes.
	RemoveRequested EventKind = "RemoveRequested"
	RemoveSucceeded EventKind = "RemoveSucceeded"
	RemoveDenied    EventKind = "RemoveDenied"
	RemoveFailed    EventKind = "RemoveFailed"
)

type Event struct {
	Kind EventKind
	// Note is a short, secret free detail for the audit row. Never the requester's reason,
	// since an agent could put anything in it.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{
	{Kind: StoreRequested},
	{Kind: StoreSucceeded},
	{Kind: StoreDenied},
	{Kind: StoreFailed},
	{Kind: UseRefused},
	{Kind: RotateRequested},
	{Kind: RotateSucceeded},
	{Kind: RotateDenied},
	{Kind: RotateFailed},
	{Kind: RemoveRequested},
	{Kind: RemoveSucceeded},
	{Kind: RemoveDenied},
	{Kind: RemoveFailed},
}

type EffectKind string

const (
	// PromptStore asks Approver.StoreSecret to show the dialog; its outcome comes back as an event.
	PromptStore EffectKind = "PromptStore"
	// PromptRotate and PromptRemove ask Approver.RotateSecret and RemoveSecret, with an action
	// ticket; the outcome comes back as an event.
	PromptRotate EffectKind = "PromptRotate"
	PromptRemove EffectKind = "PromptRemove"
	// RevokeAll revokes every grant of the secret, drops its live tickets and stops its proxy
	// sessions.
	RevokeAll EffectKind = "RevokeAll"
)

type Effect struct {
	Kind EffectKind
}

// Transition is the secret's whole transition table.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case StoreRequested:
		switch s {
		// Denied and Failed may be requested again so a person can retry; Removed frees the name.
		case Pending, Denied, Failed, Removed:
			return Pending, []Effect{{Kind: PromptStore}}, nil
		// A Stored secret is replaced only by rotate; the API answers SECRET_EXISTS or BUSY.
		case Stored, Rotating, Removing:
		}
	case StoreSucceeded:
		if s == Pending {
			return Stored, nil, nil
		}
	case StoreDenied:
		if s == Pending {
			return Denied, nil, nil
		}
	case StoreFailed:
		if s == Pending {
			return Failed, nil, nil
		}
	case UseRefused:
		// Rotating still serves uses with the old value, so a refused use is recorded there too.
		// Any other state answers SECRET_NOT_FOUND, not a refused use.
		switch s {
		case Stored, Rotating:
			return s, nil, nil
		case Pending, Denied, Failed, Removing, Removed:
		}
	case RotateRequested:
		if s == Stored {
			return Rotating, []Effect{{Kind: PromptRotate}}, nil
		}
	case RotateSucceeded, RotateDenied, RotateFailed:
		// Grants stay Active whatever the outcome; only the Keychain item changes.
		if s == Rotating {
			return Stored, nil, nil
		}
	case RemoveRequested:
		if s == Stored {
			return Removing, []Effect{{Kind: PromptRemove}}, nil
		}
	case RemoveSucceeded:
		if s == Removing {
			return Removed, []Effect{{Kind: RevokeAll}}, nil
		}
	case RemoveDenied, RemoveFailed:
		if s == Removing {
			return Stored, nil, nil
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
