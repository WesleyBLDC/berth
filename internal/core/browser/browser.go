// Package browser is the state machine for one browser Berth launches itself, over a private CDP
// pipe on a profile Berth owns. It is linked to the observed session of the same process (M2's
// session machine) by the session id, not merged with it: a launch starts from a request and its
// events come from the pipe, while every session event is a scan observation.
package browser

import "berth/internal/core/machine"

const Kind = "browser"

// Entity names the machine instance for a browser id, 16 lowercase hex characters.
func Entity(id string) string { return "browser/" + id }

type State string

const (
	// Requested is a browser no event has touched; it is never stored, since the first saved
	// transition is Launch.
	Requested State = "Requested"
	// Launching is a browser whose process is starting, until it answers over the pipe.
	Launching State = "Launching"
	// Ready is a browser that answered over the pipe, with its CDP proxy listening.
	Ready State = "Ready"
	// Closing is a browser asked to close, until its process exits.
	Closing State = "Closing"
	// Closed is a browser whose process exited or never started; it holds no profile.
	Closed State = "Closed"
)

var States = []State{Requested, Launching, Ready, Closing, Closed}

const Initial = Requested

type EventKind string

const (
	// Launch comes from LaunchBrowser once the profile lock is held.
	Launch EventKind = "Launch"
	// Started comes when the browser answered Browser.getVersion over the pipe.
	Started EventKind = "Started"
	// LaunchFailed comes when the browser did not start or did not answer in time; its process
	// group is already killed.
	LaunchFailed EventKind = "LaunchFailed"
	// CloseRequested comes from CloseBrowser and from berthd stopping.
	CloseRequested EventKind = "CloseRequested"
	// CloseTimedOut comes when a closing browser is still running 5 seconds after Browser.close.
	CloseTimedOut EventKind = "CloseTimedOut"
	// Exited comes when the pipe reached EOF and the process was reaped, and at boot.
	Exited EventKind = "Exited"
)

type Event struct {
	Kind EventKind
	// Note is a short, fixed detail for the audit row; never a URL, a token or a profile path.
	Note string
}

func (e Event) Name() string   { return string(e.Kind) }
func (e Event) Detail() string { return e.Note }

// AllEvents has one sample of every kind, for exhaustive walks.
var AllEvents = []Event{
	{Kind: Launch}, {Kind: Started}, {Kind: LaunchFailed}, {Kind: CloseRequested}, {Kind: CloseTimedOut}, {Kind: Exited},
}

type EffectKind string

const (
	// Spawn starts the browser, waits for Browser.getVersion and opens its CDP proxy, then sends
	// Started; on any failure it kills the process group and sends LaunchFailed.
	Spawn EffectKind = "Spawn"
	// CloseBrowser sends Browser.close over the pipe and starts the 5 second close timer.
	CloseBrowser EffectKind = "CloseBrowser"
	// KillGroup sends SIGKILL to the browser's process group.
	KillGroup EffectKind = "KillGroup"
)

type Effect struct {
	Kind EffectKind
}

// Transition is the browser's whole transition table. Closed is terminal: a new launch on the
// same profile is a new browser.
func Transition(s State, e Event) (State, []Effect, error) {
	switch e.Kind {
	case Launch:
		if s == Requested {
			return Launching, []Effect{{Kind: Spawn}}, nil
		}
	case Started:
		if s == Launching {
			return Ready, nil, nil
		}
	case LaunchFailed:
		// A close that arrived while Spawn ran was applied first, so a failure can end Closing.
		switch s {
		case Launching, Closing:
			return Closed, nil, nil
		case Requested, Ready, Closed:
		}
	case CloseRequested:
		switch s {
		case Launching, Ready:
			return Closing, []Effect{{Kind: CloseBrowser}}, nil
		case Requested, Closing, Closed:
		}
	case CloseTimedOut:
		if s == Closing {
			return Closing, []Effect{{Kind: KillGroup}}, nil
		}
	case Exited:
		switch s {
		case Launching, Ready, Closing:
			return Closed, nil, nil
		case Requested, Closed:
		}
	}
	return s, nil, machine.Illegal(s, e)
}

var _ machine.Transition[State, Event, Effect] = Transition
