# 0006: Agents are scoped by session token

Status: accepted, 2026-11-16

## Context

Any process running as the user can reach the socket, including a hijacked agent.

## Decision

Agents get a session token that works only for their own session, only from their own process tree, and only for methods marked for agents in the schema.

## Why

Hiding calls is not security; scope plus Touch ID for power actions is.
