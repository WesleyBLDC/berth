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
3. Shell: run the machine through the actors kit in `internal/berthd/actors.go` (`actors[S, E, F]` and `step`), with a `run` function whose `switch effect.Kind` covers every kind and returns follow up events instead of mutating state.
   - **`final`** lists the states an entity rests in until a new request, if any: an actor stops once a saved transition reaches one or it refuses an event in one, and a later event starts a fresh actor that stops again if the entity is still final, so an ended entity holds no goroutine while self loops (a refused use) and new requests (a store over a removed name) still apply. Every machine with an end state sets it, with a test that churns entities and checks `count()` stays bounded.
   - **`saved`** is told of every saved transition, on the actor's goroutine, so it must not block: hand work to a goroutine. It is where an ended entity's timers, exit watches, slots, child processes and dependants (for example the proxy sessions of an agent session) are freed, with `berthd`'s own context, never the caller's, so a caller that cancels leaves nothing half stopped.
   - **`persisted`** is told of every transition as soon as it is saved, before its effects run (the machine kit's `Saved`); use it to wake a reader of the event table when an effect can wait a long time, such as a prompt (`WatchNeeds` does). It must not block either.
   - A `step` whose actor stopped under it already answers `machine.ErrIllegal` when the entity is final and `api.ErrUnavailable` otherwise; `errActorStopped` never leaves the kit. Callers treat `ErrIllegal` as "someone else got there first".
   - Facts beside the machine (what a scan saw, a process handle) live in memory in the shell, bounded: keep at most a fixed number of ended entities, clip every kept string, and write a churn test.
   - Boot recovery finds live rows by kind (`ports.EntityLister`) and sends each the event that is true now (`Exited` with `gone at boot`, `ProxyLost`, `Expire`), then arms timers and watches again.
4. Timeouts are events: a timer on `berthd`'s context sends them (`Expire`, `StopTimedOut`, `CloseTimedOut`); never compare times inside a transition, and cancel the timer from the `saved` hook.
5. Process exits are events too: watch with kqueue `NOTE_EXIT` (`procscan.WatchExit` by pid and start time, `procscan.WatchPID` for an unreaped child), never by polling a pid.

## Changing a machine

- Add states and events; do not rename or delete ones that may be stored (they are data in `machine_state` and `machine_event`). Retire one by making it illegal everywhere and leaving the name.
- Update `AllEvents`, `States` and the pinned legal pair count in the same change; a changed count is a deliberate table change, so say why in the commit.
- Update the machine's `stateDiagram-v2` in `ARCHITECTURE.md` (and its legal pair count) in the same commit.

## Rules the reviewer checks

- Transitions are pure: no I/O, logging, `time.Now`, randomness or map iteration order.
- Effects run only after the transition is saved (the actor does this; never call effect code from a transition).
- An effect that can fail returns a failure event (for example `Failed` with a note), so failure is a state, not a silent retry.
- No secret ever appears in an event, note, effect field or state name.
- A note is fixed text or a count, never text an agent or a process chose.
- A final state has an actor that stops and a hook that frees what it held; a new machine without `final` is a goroutine leak.
