# 0003: Explicit, hand written state machines

Status: accepted, 2026-11-16

## Context

Implicit state (flags and nullable fields) hides illegal combinations.

## Decision

Every entity has one state field and a hand written transition table returning next state and effects. No state machine library.

## Why

Nothing hidden to learn; exhaustive linting and a test over every state and event pair.
