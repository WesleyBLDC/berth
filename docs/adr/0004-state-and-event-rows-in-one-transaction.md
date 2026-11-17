# 0004: State and event rows in one transaction

Status: accepted, 2026-11-16

## Context

Berth needs history and an audit log.

## Decision

Every transition writes the new state and an event row in the same SQLite transaction. No full event sourcing.

## Why

Audit trail and history for free, without the cost of rebuilding state from events.
