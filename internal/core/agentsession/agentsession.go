// Package agentsession is the state machine for one agent session, and the pure rules that bind
// a session to a process tree and limit its store dialogs.
package agentsession

import (
	"time"

	"berth/internal/core/machine"
	"berth/internal/core/ports"
)

const Kind = "agent"

// Entity names the machine instance for a session, for example agent/<32 hex characters>.
func Entity(id string) string { return "agent/" + id }

type State string

const (
	// Issued is a session whose token no process has presented yet, and the state of a new one.
	Issued State = "Issued"
	// Bound is a session tied to a process tree root, recorded on the first accepted call.
	Bound State = "Bound"
	// Expired is a session whose token outlived its time to live.
	Expired State = "Expired"
	// Revoked is a session the owner ended.
	Revoked State = "Revoked"
	// Ended is a bound session whose root process exited.
	Ended State = "Ended"
)

var States = []State{Issued, Bound, Expired, Revoked, Ended}

// Initial is the state of a session CreateAgentSession just created.
const Initial = Issued

type EventKind string

const (
	// Bind comes with the first accepted call, once the root is recorded on the session's row.
	Bind EventKind = "Bind"
	// Expire comes when a call or a listing finds the session past its expiry.
	Expire EventKind = "Expire"
	// Revoke comes from RevokeAgentSession.
	Revoke EventKind = "Revoke"
	// RootExited comes when a call or a listing finds the bound root process gone.
	RootExited EventKind = "RootExited"
)

type Event struct {
	Kind EventKind
	// Note is a short, secret free detail for the audit row, such as the root pid; never a token.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{{Kind: Bind}, {Kind: Expire}, {Kind: Revoke}, {Kind: RootExited}}

// Effect is empty: the machine only records where a session stands, and every check reads that
// state, so nothing has to be done when it changes.
type Effect struct{}

// Transition is the agent session's whole transition table. Expired, Revoked and Ended are
// terminal, so a token never comes back once refused.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case Bind:
		if s == Issued {
			return Bound, nil, nil
		}
	case Expire:
		switch s {
		case Issued, Bound:
			return Expired, nil, nil
		case Expired, Revoked, Ended:
		}
	case Revoke:
		switch s {
		case Issued, Bound:
			return Revoked, nil, nil
		case Expired, Revoked, Ended:
		}
	case RootExited:
		if s == Bound {
			return Ended, nil, nil
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition

// Live reports whether a session in state s may still serve calls.
func Live(s State) bool { return s == Issued || s == Bound }

const launchdPID = 1

// Root picks the process tree a session binds to from the first caller's lineage (the caller
// first, then each parent): the highest ancestor that still runs as the agent user, stopping
// below launchd and below the first process of another user. For an agent started by a launch
// helper that is the harness; for a process launchd adopted it is the process itself. It is false
// when the caller itself is not the agent user.
func Root(lineage []ports.ProcessInfo, agentUID uint32) (ports.ProcessRef, bool) {
	if len(lineage) == 0 || lineage[0].UID != agentUID || lineage[0].PID == launchdPID {
		return ports.ProcessRef{}, false
	}
	root := lineage[0].ProcessRef
	for _, p := range lineage[1:] {
		if p.PID == launchdPID || p.UID != agentUID {
			break
		}
		root = p.ProcessRef
	}
	return root, true
}

// Descends reports whether the caller's lineage passes through root, by pid and start time, so
// a reused pid never counts.
func Descends(lineage []ports.ProcessInfo, root ports.ProcessRef) bool {
	for _, p := range lineage {
		if p.Same(root) {
			return true
		}
	}
	return false
}

// Store dialog limits for one session (provisional): at most MaxOpenStores waiting for the
// person at once, and at most MaxStoresPerHour started in any hour.
const (
	MaxOpenStores    = 3
	MaxStoresPerHour = 10
)

// AdmitStore decides whether a session may open one more store dialog, given how many it has
// open and when it started the ones it was admitted for.
func AdmitStore(open int, started []time.Time, now time.Time) (bool, []time.Time) {
	return Limit{Open: MaxOpenStores, PerHour: MaxStoresPerHour}.Admit(open, started, now)
}

// Limit bounds what one agent session may have berthd hold for it at once and start in any
// rolling hour: store dialogs, proxies, browsers.
type Limit struct{ Open, PerHour int }

// Admit decides whether one more may start, given how many are open and when the admitted ones
// started. It returns the start times still inside the last hour, with now added when admitted.
func (l Limit) Admit(open int, started []time.Time, now time.Time) (bool, []time.Time) {
	kept := make([]time.Time, 0, len(started)+1)
	for _, t := range started {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if open >= l.Open || len(kept) >= l.PerHour {
		return false, kept
	}
	return true, append(kept, now)
}
