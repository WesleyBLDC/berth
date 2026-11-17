// Package proxysession is the state machine for one berth-proxy run.
package proxysession

import "berth/internal/core/machine"

const Kind = "proxy"

// Entity names the machine instance for a session, for example proxy/<32 hex characters>.
func Entity(id string) string { return "proxy/" + id }

type State string

const (
	// Authorized is a session AuthorizeProxy created, and the state of a new session.
	Authorized State = "Authorized"
	// Serving is a session whose proxy opened WatchProxySession.
	Serving State = "Serving"
	// Stopping is a session berthd told to stop, waiting for the proxy to end or drop.
	Stopping State = "Stopping"
	// Ended is a session the proxy closed with EndProxySession.
	Ended State = "Ended"
	// Lost is a session whose proxy vanished without ending it.
	Lost State = "Lost"
)

var States = []State{Authorized, Serving, Stopping, Ended, Lost}

// Initial is the state of a session AuthorizeProxy just created.
const Initial = Authorized

type EventKind string

const (
	// ProxyReady comes when the proxy opens WatchProxySession.
	ProxyReady EventKind = "ProxyReady"
	// StopRequested comes from a revoke, a remove or berthd stopping.
	StopRequested EventKind = "StopRequested"
	// ProxyEnded comes from EndProxySession.
	ProxyEnded EventKind = "ProxyEnded"
	// ProxyLost comes when the watch stream drops, the kernel reports the proxy exited, no
	// ProxyReady arrives within 30 seconds, or boot recovery finds the session open.
	ProxyLost EventKind = "ProxyLost"
)

// StopReason says why berthd stops a session; it maps to berth.v1.ProxyStopReason.
type StopReason string

const (
	StopGrantRevoked   StopReason = "GrantRevoked"
	StopSecretRemoved  StopReason = "SecretRemoved"
	StopDaemonStopping StopReason = "DaemonStopping"
)

type Event struct {
	Kind EventKind
	// Reason is set only on StopRequested.
	Reason StopReason
	// Note is a short, secret free detail for the audit row; never a path or a request detail.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{
	{Kind: ProxyReady}, {Kind: StopRequested, Reason: StopGrantRevoked}, {Kind: ProxyEnded}, {Kind: ProxyLost},
}

type EffectKind string

// SendStop sends a STOP event with Reason on the session's watch stream.
const SendStop EffectKind = "SendStop"

type Effect struct {
	Kind   EffectKind
	Reason StopReason
}

// Transition is the proxy session's whole transition table. Ended and Lost are terminal, and a
// stop or a lost stream makes the proxy zero its values and exit, so the session fails closed.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case ProxyReady:
		if s == Authorized {
			return Serving, nil, nil
		}
	case StopRequested:
		switch s {
		case Authorized, Serving:
			return Stopping, []Effect{{Kind: SendStop, Reason: e.Reason}}, nil
		case Stopping, Ended, Lost:
		}
	case ProxyEnded:
		switch s {
		case Authorized, Serving, Stopping:
			return Ended, nil, nil
		case Ended, Lost:
		}
	case ProxyLost:
		switch s {
		case Authorized, Serving, Stopping:
			return Lost, nil, nil
		case Ended, Lost:
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
