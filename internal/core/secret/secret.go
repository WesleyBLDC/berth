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
)

var States = []State{Pending, Stored, Denied, Failed}

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
	{Kind: StoreRequested}, {Kind: StoreSucceeded}, {Kind: StoreDenied}, {Kind: StoreFailed}, {Kind: UseRefused},
}

type EffectKind string

// PromptStore asks Approver.StoreSecret to show the dialog; its outcome comes back as an event.
const PromptStore EffectKind = "PromptStore"

type Effect struct {
	Kind EffectKind
}

// Transition is the secret's whole transition table.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case StoreRequested:
		switch s {
		// Denied and Failed may be requested again so a person can retry.
		case Pending, Denied, Failed:
			return Pending, []Effect{{Kind: PromptStore}}, nil
		// A Stored secret is replaced only by rotate (E2b); the API answers SECRET_EXISTS.
		case Stored:
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
		// Use of a secret that is not Stored is SECRET_NOT_FOUND, not a refused use.
		if s == Stored {
			return Stored, nil, nil
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
