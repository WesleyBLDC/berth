// Package grant is the state machine for one consumer pinned to a secret. Item 3 of E2a adds
// Transition.
package grant

import "strconv"

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
