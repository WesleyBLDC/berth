# Berth

Berth is a local app for macOS that lists every browser your agents, tests and scrapers have open, lets you watch and take over any of them, and decides which Keychain passwords and which runbook each one gets.

Status: planned and ready for M0 (the spike). No code yet.

- Design and plan: https://wiki.wesleybldc.com/wiki-artifacts/berth
- Decisions: `docs/adr/`
- Next milestones: `docs/plans/m0-spike.md`, `docs/plans/m1-skeleton.md`
- Setup: `docs/setup.md`
- Rules for coding agents: `AGENTS.md`

## Shape

- `berthd`: a Go daemon that scans processes, holds browser connections and serves one API on a Unix socket.
- `berth`: one shot commands and a full screen terminal view.
- `Berth.app`: a native SwiftUI window and menu bar item.
- `berth-mcp`: a thin MCP shim for agents without a shell.

## Scope

- Local environments only.
- macOS first.
- Personal first, open source later: personal names, paths and profiles live in config, never in this repository.
