# E3: Isolated agents

Goal: protect secrets and your files from a hostile agent that can run arbitrary code, which E2a explicitly does not (see the threat model in `ARCHITECTURE.md`).
Added 2026-11-16 at Wesley's request, after the E2a reviews showed that a broker cannot fully defend against code running as the same user.

## Idea

Coding agents run as a different identity than you, so your dotfiles, Keychain, `berth.db` and home folder are out of their reach, and they can only ask `berthd` for what their grants allow.

## Options to evaluate first (a scout compares them before the plan is written)

| Option | Isolation | Cost |
|---|---|---|
| A dedicated standard macOS user (for example `berthagent`) per machine | Strong: separate home, Keychain, dotfiles; the socket sees a different uid | Workspace sharing through a group owned folder; harness login and tool setup per user |
| `sandbox-exec` profiles around the harness | Medium: file and network rules, same uid, deprecated API | Low setup; fragile profiles |
| A Linux VM per agent through Apple's `container` tooling on macOS 26 | Strongest | Tools run in Linux, not macOS; browsers and Keychain need bridging |

## What changes in Berth

- The socket accepts the agent identity's uid, and only on the agent surface (methods marked `(berth.agent) = true`); every other method stays owner only. This is where the agent surface becomes enforced, not just marked.
- Session tokens (ADR 0006) bind each agent session to its own grants.
- `berth run` for agents executes the consumer as the agent identity, so dotfiles, `PATH` and `HOME` belong to that identity, which closes the E2a dotfiles limit for env injection.
- A launcher (`berth agent start <harness>`) starts Claude Code, Cursor agents and others in the isolated identity with the workspace mounted or shared.

## Acceptance tests

- From the agent identity: reading your home, Keychain items, `berth.db` and dotfiles fails.
- A hostile agent script that tries every E2a review attack (driving signed binaries, fake daemon, wrapper helpers, tool configuration) gets no secret value.
- Calling a non agent method from the agent identity is refused at the socket.

## Order

After E2b and before or alongside M6, since M6 browser credentials face the same threat.
