# Working on Berth with several agents

How a milestone is cut so several coding agents build at once without stepping on each other, and how their work is checked and landed.

## Why Berth splits well

The architecture already draws the lines: the core talks only to ports, adapters live in their own packages, and the API is generated from one schema.
Once the contract (proto messages, port interfaces, state and event names) is fixed, the pieces behind it can be built in parallel and meet again at the shell.

## The wave pattern

| Wave | Who | Runs | Owns |
|---|---|---|---|
| 0. Plan | A scout (read only) | Alone | `docs/plans/<id>.md`: acceptance tests, the item list, file plans |
| 1. Contract | One `berth-worker` | Alone, lands first | `proto/`, `gen/`, `internal/core/ports/`, state and event enums, reserved migration numbers, new `go.mod` dependencies |
| 2. Build | Up to three `berth-worker`s | In parallel, each in its own worktree | One package each: a machine's transition table, an adapter, a CLI command, a Swift helper |
| 3. Integrate | One `berth-worker` | After wave 2 lands | `internal/berthd/` wiring, `test/e2e/`, `Makefile` |
| 4. Check | `berth-reviewer` per item, `berth-tester` for behaviour | In parallel, read only | Reports only |

Read only helpers (scouts, reviewers, testers) do not count toward the cap of three builders.

## Files that serialize

Only the item that owns them may edit these; everything else names them as a follow up:

- `proto/` and `gen/` (contract item; regenerate `gen/` with `make gen` after a rebase, never merge it by hand)
- `go.mod`, `go.sum` (contract item adds dependencies up front)
- `internal/berthd/` (integration item)
- `internal/adapters/sqlite/migrations/` (numbers reserved in the plan; the contiguity test catches gaps)
- `Makefile`, `AGENTS.md`, `.golangci.yml`, `lead.toml` (lead or Wesley)

`lead.toml` sets `shared_files = "serialize"`, so two items whose file plans overlap never run at once.

## Landing

- Every item ends with `make check` printing `check: PASS` and a local commit on its branch.
- A reviewer reads the diff against its brief; a tester proves anything user or agent facing.
- The leash is `held`: branches stay local and Wesley merges, or tells the lead to merge, contract first, then build items smallest diff first, then integration.
- Nothing is pushed before M8.

## Example: M2, see every browser

| Item | Wave | Owns | Acceptance tests it carries |
|---|---|---|---|
| Contract | 1 | `SessionsService` (`ListSessions`, `InspectSession`, `StopSession`, `WatchSessions` stub), `ProcessSource` port, session `State` and `EventKind` enums, migration 0002 | `buf breaking` clean; `TestAgentSurface` unchanged |
| Session machine | 2 | `internal/core/session/` transition table and walk test | Leftover when owner gone; stop only on command |
| Process scanner | 2 | `internal/adapters/process/` (`gopsutil`, owner chain, stable session IDs) | Owner chain finds Claude Code, Cursor, a terminal |
| Signatures | 2 | `internal/adapters/signatures/` TOML registry and fixtures | Playwright MCP, Chrome DevTools MCP and a Playwright script are classified |
| CLI | 2 | `cmd/berth` `ps`, `inspect`, `stop`, `leftovers` with golden files | Output at 80 columns; short ID prefixes |
| Integration | 3 | Reconcile loop in `internal/berthd`, handlers, e2e with real browsers | Killing the owner makes a `Leftover`; `berth stop` ends every child |

## Example: E2, terminal secrets

| Item | Wave | Owns |
|---|---|---|
| Contract | 1 | `SecretsService` methods, `SecretStore`, `Approver` and `ConsumerIdentity` ports, grant `State` enums, migration numbers |
| `berth-native` | 2 | Swift helper: Keychain add and read, Touch ID, the secure input dialog, the pipe protocol |
| Grant machine | 2 | `internal/core/grant/` transition table and walk test |
| Identity and native adapter | 2 | Go side of the pipe; code signature identity checks for consumers |
| CLI | 2 | `berth secret request`, `ls`, `run` with output masking |
| Integration | 3 | Wiring, the canary e2e test, the Claude Code hook update |

## Running it

- **Lead mode**: `/lead` in this repository reads `lead.toml` and runs the waves with `berth-worker`, `berth-reviewer` and `berth-tester`.
- **By hand in Claude Code**: plan with a scout, land the contract with one worker, then start up to three `berth-worker` agents with `isolation: "worktree"`, two per message, each with its item's brief; review each with `berth-reviewer` in parallel.
- **A workflow** (when Wesley asks for one): the same waves as a script, with each wave's results gating the next.
