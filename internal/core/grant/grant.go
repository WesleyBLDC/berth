// Package grant is the state machine for one consumer pinned to a secret.
package grant

import (
	"strconv"

	"berth/internal/core/machine"
)

const Kind = "grant"

// Entity names the machine instance for a grant row, for example grant/7.
func Entity(id int64) string { return "grant/" + strconv.FormatInt(id, 10) }

type State string

const (
	// Requested is a grant waiting for Touch ID, and the state of a new grant.
	Requested State = "Requested"
	Active    State = "Active"
	Denied    State = "Denied"
	// Stale is a grant whose executable hash changed; it refuses use until granted again.
	Stale   State = "Stale"
	Revoked State = "Revoked"
)

var States = []State{Requested, Active, Denied, Stale, Revoked}

// Initial is the state of a new grant row.
const Initial = Requested

type EventKind string

const (
	// GrantRequested comes from GrantSecret.
	GrantRequested EventKind = "GrantRequested"
	// GrantApproved, GrantDenied and GrantFailed are the Approver.ConfirmPower outcomes.
	GrantApproved EventKind = "GrantApproved"
	GrantDenied   EventKind = "GrantDenied"
	GrantFailed   EventKind = "GrantFailed"
	// UseAuthorized comes from AuthorizeUse when the identified executable matches.
	UseAuthorized EventKind = "UseAuthorized"
	// HashChanged comes from AuthorizeUse when the executable no longer hashes to the pin.
	HashChanged EventKind = "HashChanged"
	// UseRefused records an AuthorizeUse refused by this grant (stale, denied or revoked).
	UseRefused EventKind = "UseRefused"
	Revoke     EventKind = "Revoke"
)

type Event struct {
	Kind EventKind
	// Note is a short, secret free detail for the audit row.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{
	{Kind: GrantRequested},
	{Kind: GrantApproved},
	{Kind: GrantDenied},
	{Kind: GrantFailed},
	{Kind: UseAuthorized},
	{Kind: HashChanged},
	{Kind: UseRefused},
	{Kind: Revoke},
}

type EffectKind string

const (
	// PromptPower asks Approver.ConfirmPower for Touch ID; its outcome comes back as an event.
	PromptPower EffectKind = "PromptPower"
	// IssueTicket makes a single use ticket and counts the use on the grant row.
	IssueTicket EffectKind = "IssueTicket"
)

type Effect struct {
	Kind EffectKind
}

// Transition is the grant's whole transition table. A Denied, Stale or Revoked grant is never
// approved again; the person makes a new grant, which is a new entity.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case GrantRequested:
		if s == Requested {
			return Requested, []Effect{{Kind: PromptPower}}, nil
		}
	case GrantApproved:
		if s == Requested {
			return Active, nil, nil
		}
	case GrantDenied:
		if s == Requested {
			return Denied, nil, nil
		}
	case GrantFailed:
		// Denied, since a retry is a new GrantSecret and so a new grant row, never this one.
		if s == Requested {
			return Denied, nil, nil
		}
	case UseAuthorized:
		if s == Active {
			return Active, []Effect{{Kind: IssueTicket}}, nil
		}
	case HashChanged:
		if s == Active {
			return Stale, nil, nil
		}
	case Revoke:
		switch s {
		case Active, Stale:
			return Revoked, nil, nil
		case Requested, Denied, Revoked:
		}
	case UseRefused:
		switch s {
		case Stale, Denied, Revoked:
			return s, nil, nil
		case Requested, Active:
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
