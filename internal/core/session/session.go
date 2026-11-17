// Package session is the state machine for one browser session: a browser process tree and
// whoever started it. In M2 every event is an observation from berthd's scan or a stop request;
// M4 adds the states of a browser Berth launches itself.
package session

import "berth/internal/core/machine"

const Kind = "session"

// Entity names the machine instance for a session id, <pid>-<start time in microseconds>.
func Entity(id string) string { return "session/" + id }

type State string

const (
	// Discovered is a browser the scan has just found, and the state of a new session.
	Discovered State = "Discovered"
	// Active is a browser whose processes used CPU since the last scan.
	Active State = "Active"
	// Idle is a browser that used no CPU for the quiet window.
	Idle State = "Idle"
	// Leftover is a browser whose owner exited; it stays one until it exits or is stopped.
	Leftover State = "Leftover"
	// Stopping is a browser berthd sent SIGTERM, waiting for it to exit.
	Stopping State = "Stopping"
	// Stopped is a browser that exited.
	Stopped State = "Stopped"
)

var States = []State{Discovered, Active, Idle, Leftover, Stopping, Stopped}

const Initial = Discovered

type EventKind string

const (
	// Seen comes on a session's first observation when it has no row yet, so every session has a
	// row, and a stream built on the event table sees it appear.
	Seen EventKind = "Seen"
	// Busy comes when the session's CPU time grew since the last scan.
	Busy EventKind = "Busy"
	// Quiet comes when the session's CPU time has not grown for the quiet window.
	Quiet EventKind = "Quiet"
	// OwnerExited comes when the owner process is gone, or the browser had no owner when found.
	OwnerExited EventKind = "OwnerExited"
	// StopRequested comes from StopSession.
	StopRequested EventKind = "StopRequested"
	// StopTimedOut comes when a stopping browser is still running 5 seconds after SIGTERM.
	StopTimedOut EventKind = "StopTimedOut"
	// Exited comes from kqueue NOTE_EXIT, from a scan that no longer finds the browser, or from
	// boot recovery.
	Exited EventKind = "Exited"
)

type Event struct {
	Kind EventKind
	// Note is a short detail for the audit row; never an argument, a URL or a profile path.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{
	{Kind: Seen}, {Kind: Busy}, {Kind: Quiet}, {Kind: OwnerExited}, {Kind: StopRequested}, {Kind: StopTimedOut}, {Kind: Exited},
}

type EffectKind string

const (
	// SignalTerm sends SIGTERM to the browser and starts the 5 second stop timer.
	SignalTerm EffectKind = "SignalTerm"
	// SignalKill sends SIGKILL to the browser and every process still in its session.
	SignalKill EffectKind = "SignalKill"
)

type Effect struct {
	Kind EffectKind
}

// Transition is the session's whole transition table. Stopped is terminal: a pid that comes back
// is a new process, so a new session.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case Seen:
		if s == Discovered {
			return Discovered, nil, nil
		}
	case Busy:
		switch s {
		case Discovered, Idle:
			return Active, nil, nil
		case Active, Leftover, Stopping, Stopped:
		}
	case Quiet:
		switch s {
		case Discovered, Active:
			return Idle, nil, nil
		case Idle, Leftover, Stopping, Stopped:
		}
	case OwnerExited:
		switch s {
		case Discovered, Active, Idle:
			return Leftover, nil, nil
		case Leftover, Stopping, Stopped:
		}
	case StopRequested:
		switch s {
		case Discovered, Active, Idle, Leftover:
			return Stopping, []Effect{{Kind: SignalTerm}}, nil
		case Stopping, Stopped:
		}
	case StopTimedOut:
		if s == Stopping {
			return Stopping, []Effect{{Kind: SignalKill}}, nil
		}
	case Exited:
		switch s {
		case Discovered, Active, Idle, Leftover, Stopping:
			return Stopped, nil, nil
		case Stopped:
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
