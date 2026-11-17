<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo/svg/berth-lockup-dark-bg.svg">
    <img src="brand/logo/svg/berth-lockup-light-bg.svg" width="240" alt="berth">
  </picture>
</p>

# Berth

Berth is a local app for macOS that lists every browser your agents, tests and scrapers have open, lets you watch and take over any of them, and decides which Keychain passwords and which runbook each one gets.

Status: pre release, in active development, with no releases yet.
M0 (spike) and M1 (skeleton) are done; E2a (terminal secrets) is being integrated; M2 (see every browser) is next.
The full status is in [ARCHITECTURE.md](ARCHITECTURE.md#milestones).

- Architecture, what is built and what is planned: [ARCHITECTURE.md](ARCHITECTURE.md)
- Decisions: [docs/adr/](docs/adr)
- Milestone plans: [docs/plans/](docs/plans)
- Agent safety: [docs/agent-safety.md](docs/agent-safety.md)
- How several agents split the work: [docs/agentic-work.md](docs/agentic-work.md)
- Setup: [docs/setup.md](docs/setup.md)
- Rules for coding agents: [AGENTS.md](AGENTS.md)
- Brand: [brand/README.md](brand/README.md)

The maintainer keeps extra design notes privately; everything needed to understand and change the code is in this repository.

## Shape

- `berthd`: a Go daemon that holds all state, runs explicit state machines and serves one Connect API on a Unix socket.
- `berth`: one shot commands today, and a full screen terminal view later.
- `berth-native`: a small signed Swift helper, the only Berth process that touches secret values (Keychain, Touch ID).
- `Berth.app`: a native SwiftUI window and menu bar item (planned).
- `berth-mcp`: a thin MCP shim for agents without a shell (planned).

## Development

- macOS 26 or later ([ADR 0010](docs/adr/0010-macos-26-and-later-only.md)).
- The toolchain in [docs/setup.md](docs/setup.md): Go, Swift and Xcode, `buf`, `golangci-lint`, `prek`.
- `make hooks` once, then `make check` before every commit; it must end with `check: PASS`.
- This is pre release software: there are no releases or tags, interfaces change without notice, and signing still uses the maintainer's own Apple Development identity.

## Scope

- Local environments only.
- macOS first.
- Developed in public; personal names, paths and profiles belong in local config, and the few still in the code move there over time ([ADR 0017](docs/adr/0017-public-development-repository.md)).
