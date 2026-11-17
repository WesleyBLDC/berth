# 0009: CDP proxy refuses cross session methods from day one

Status: accepted, 2026-11-16

## Context

An agent's CDP connection could reach other targets or browser wide state.

## Decision

Methods outside the agent's session are refused; everything else is logged.

## Why

Cheap to enforce early; hard to add once agents depend on them.
