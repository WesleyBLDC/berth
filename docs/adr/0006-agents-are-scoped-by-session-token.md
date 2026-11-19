# 0006: Agents are scoped by session token

Status: accepted, 2026-11-16; built for the agent socket on 2026-11-17 (E3 item 4), with provisional picks awaiting the maintainer

## Context

Any process running as the user can reach the socket, including a hijacked agent.

## Decision

Agents get a session token that works only for their own session, only from their own process tree, and only for methods marked for agents in the schema.

## Why

Hiding calls is not security; scope plus Touch ID for power actions is.

## Status note (2026-11-17)

E3 item 4 built this for the agent socket: the owner issues a session, `berthd` keeps only the token's hash, and every agent method there needs the token in a header.
"Their own process tree" became, provisionally, the tree under the first caller's highest ancestor that runs as the agent user, bound on first use; the picks and limits are in [the E3 plan](../plans/e3-isolated-agents.md#item-4-session-tokens-bound-to-the-agent-uid-and-its-process-tree).
M4 item 7 extended the scope to browsers: an agent session sees and closes only the managed browsers it opened, each behind its own CDP token.

The token is held as a Go string in `berthd` and `berth` (HTTP headers and proto fields require strings).
It is a revocable, scoped bearer credential that never grants reading a value, so it is not a secret value under invariant 7; it is still kept out of logs, event notes, errors and the request log, and only its SHA-256 is stored.
