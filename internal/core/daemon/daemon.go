// Package daemon is the state machine for berthd itself.
package daemon

import (
	"berth/internal/core/machine"
)

const (
	Entity = "daemon"
	Kind   = "daemon"
)

type State string

const (
	// Starting is the state of a daemon that has never run.
	Starting   State = "Starting"
	Recovering State = "Recovering"
	Serving    State = "Serving"
	Draining   State = "Draining"
	Stopped    State = "Stopped"
)

var States = []State{Starting, Recovering, Serving, Draining, Stopped}

type EventKind string

const (
	// Boot is sent once when the process starts. It is legal from every state because a
	// process start is a fact; booting from anything but Stopped or Starting means the last
	// run did not shut down cleanly.
	Boot          EventKind = "Boot"
	Recovered     EventKind = "Recovered"
	StopRequested EventKind = "StopRequested"
	Drained       EventKind = "Drained"
	Failed        EventKind = "Failed"
)

type Event struct {
	Kind EventKind
	// Note is a short, secret free detail for the audit row.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{{Kind: Boot}, {Kind: Recovered}, {Kind: StopRequested}, {Kind: Drained}, {Kind: Failed}}

type EffectKind string

const (
	Recover      EffectKind = "Recover"
	StartServing EffectKind = "StartServing"
	StopServing  EffectKind = "StopServing"
	Exit         EffectKind = "Exit"
)

type Effect struct {
	Kind EffectKind
	// Unclean is set on Recover when the previous run crashed.
	Unclean bool
	// Previous is the state the last run ended in.
	Previous State
	Code     int
}

// Transition is the daemon's whole transition table.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case Boot:
		return Recovering, []Effect{{Kind: Recover, Previous: s, Unclean: s != Stopped && s != Starting}}, nil
	case Recovered:
		if s == Recovering {
			return Serving, []Effect{{Kind: StartServing}}, nil
		}
	case StopRequested:
		switch s {
		case Recovering, Serving:
			return Draining, []Effect{{Kind: StopServing}}, nil
		case Starting, Draining, Stopped:
		}
	case Drained:
		if s == Draining {
			return Stopped, []Effect{{Kind: Exit, Code: 0}}, nil
		}
	case Failed:
		switch s {
		case Recovering, Serving, Draining:
			return Stopped, []Effect{{Kind: Exit, Code: 1}}, nil
		case Starting, Stopped:
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
