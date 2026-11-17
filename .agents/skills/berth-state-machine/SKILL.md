---
name: berth-state-machine
description: "Create a new explicit state machine in Berth, or add a state, event or effect to an existing one, with the transition table, exhaustive walk test, actor wiring and effect runner. Use whenever a task introduces anything stateful (sessions, grants, fills, profile locks, streams) or changes how one moves between states."
metadata:
  author: wesley
---

# Explicit state machines

Berth has no implicit state: no flags whose combinations mean something, no timestamps compared ad hoc.
`internal/core/daemon` is the reference implementation; copy its shape.

## A new machine

1. Package `internal/core/<name>/` with:
   - `Entity` naming rule (the id each instance uses, for example `session/<id>`) and a `Kind` constant.
   - `type State string` constants, and `var States = []State{...}` listing every one.
   - `type EventKind string` constants, `type Event struct { Kind EventKind; ... }` with `Name()` and, when useful, `Detail()` (short, secret free text for the audit row), and `var AllEvents` with one sample per kind.
   - `type EffectKind string` constants and `type Effect struct { Kind EffectKind; ... }` carrying what the shell needs.
   - `func Transition(s State, e Event) (State, []Effect, error)`: a `switch e.Kind` with a nested `switch s` where needed; no `default:` (the `exhaustive` linter must see every case); end with `return s, nil, machine.Illegal(s, e)`.
   - `var _ machine.Transition[State, Event, Effect] = Transition`.
2. Test file: `machinetest.Walk(t, States, AllEvents, Transition)` with the legal pair count pinned in the assertion, plus one test per rule that matters (the happy path, each guard, each effect's fields).
3. Shell: create the actor with `machine.NewActor` in the shell that owns it (today `internal/berthd`), with a `Run` function whose `switch effect.Kind` covers every kind and returns follow up events instead of mutating state.
4. Timeouts are events: a scheduler sends `IdleTimeout`; never compare times inside a transition.

## Changing a machine

- Add states and events; do not rename or delete ones that may be stored (they are data in `machine_state` and `machine_event`). Retire one by making it illegal everywhere and leaving the name.
- Update `AllEvents`, `States` and the pinned legal pair count in the same change; a changed count is a deliberate table change, so say why in the commit.
- Update the wiki diagram for the machine on the architecture page, or name it in your report.

## Rules the reviewer checks

- Transitions are pure: no I/O, logging, `time.Now`, randomness or map iteration order.
- Effects run only after the transition is saved (the actor does this; never call effect code from a transition).
- An effect that can fail returns a failure event (for example `Failed` with a note), so failure is a state, not a silent retry.
- No secret ever appears in an event, note, effect field or state name.
