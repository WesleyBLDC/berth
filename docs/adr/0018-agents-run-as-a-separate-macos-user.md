# 0018: Agents run as a separate macOS user

Status: Proposed, 2026-11-17; every pick below is provisional, awaiting the maintainer

## Context

E2a and E2b showed that a broker cannot fully defend against an agent that runs arbitrary code as the same user: it can configure the tools that receive a secret through dotfiles, `PATH` and `HOME`, and signal Berth's own processes.
E3 must put a real boundary between the agent and the owner's files, Keychain and `berth.db`.

| Option | Isolation | Cost |
|---|---|---|
| A dedicated macOS user | Strong: separate home, Keychain and dotfiles, a different uid at the socket, no signals across users | Admin rights once; a shared workspace through a group |
| Seatbelt (`sandbox-exec`) | Medium: same uid, so signals, the socket and the Keychain stay reachable; deprecated API | Low |
| A Linux VM (`container`) | Strongest | Tools run in Linux; browsers and Keychain need bridging |
| Endpoint Security | Detection only | Paid program and an entitlement |

## Decision

Agents run as a dedicated standard macOS user (provisional, awaiting the maintainer).

- `berthd` serves the agent user on a second socket that accepts only that uid and only methods marked `(berth.v1.agent) = true`.
- Consumers of `berth run --env` for agents run as a third "runner" user that the agent cannot write to or signal, started by `berthd` with the agent's stdio passed over the socket; `berth-native` and `berth-proxy` are started by `berthd` as the owner.
- One shared agent user for now; a shared workspace at `/Users/Shared/berth-work` owned by a group.
- How processes start as another user (ssh to localhost, a root LaunchDaemon, or sudo) is decided by a spike that needs the maintainer's admin password; a small root launch helper is expected.
- Seatbelt is an optional layer, off by default; the Linux VM is a later backend that always refuses env secrets; no paid Apple program before M8.

## Why

A separate uid is the strongest boundary that keeps macOS tools, the Keychain and browsers working, and every check it adds is a kernel fact (the peer uid, signal permissions, file ownership), never the agent's word.
Seatbelt alone keeps the same uid, so it cannot stop signals or socket access; the VM costs the macOS toolchain; Endpoint Security watches but does not isolate.
The plan with its items is [docs/plans/e3-isolated-agents.md](../plans/e3-isolated-agents.md).
