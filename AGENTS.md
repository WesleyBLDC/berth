# Berth: agent instructions

Berth is a local macOS app that lists every browser agents, tests and scrapers have open, lets a person watch and take over any of them, and brokers Keychain secrets to browsers and terminals without agents ever reading them.
The full design is in the wiki (start at the reading order on https://wiki.wesleybldc.com/wiki-artifacts/berth); the decisions that bind code are in `docs/adr/`, and the current milestone's plan is in `docs/plans/`.
A lead session reads `lead.toml`; workers can ignore it.

## Done means

`make check` ends with `check: PASS`.
It runs every linter and hook, `go mod verify`, `buf breaking`, unit tests with the race detector, the end to end tests with the real binaries, and `govulncheck`.
Paste its last lines in your report; a failure is investigated to its root cause, never rerun and hoped.

| Command | Use |
|---|---|
| `make test` | Unit tests, seconds; run after every change |
| `make e2e` | Real `berthd` and `berth` binaries in a temporary `BERTH_HOME` |
| `make lint` | Every linter, including untracked files |
| `make fmt` | Format Go, Protobuf, Swift, Python |
| `make gen` | Regenerate `gen/` after editing `proto/` |
| `make build` | `bin/berthd` and `bin/berth` |

## Where things live

| Path | Holds | May import |
|---|---|---|
| `proto/berth/v1/` | The API contract: services, messages, validation rules, the `agent` method option, typed error reasons | |
| `gen/` | Generated code, committed; never edit by hand | |
| `internal/core/ports/` | Interfaces the core needs (Store, Clock, RequestLog) | stdlib only |
| `internal/core/machine/` | The state machine kit: `Transition`, `Actor`, `machinetest.Walk` | ports |
| `internal/core/<machine>/` | One package per state machine (today `daemon`) | machine, ports |
| `internal/adapters/<name>/` | Implementations of ports and the API (`sqlite`, `socket`, `api`, `clock`) | core, gen |
| `internal/berthd/` | The daemon shell: wires adapters, runs machines, performs effects | everything |
| `internal/client/` | The Go client every Go tool uses to reach the daemon | gen, api |
| `internal/testutil/` | Fakes for ports (in memory store, fake clock) | ports |
| `cmd/` | `berthd` and `berth` entry points, thin | internal |
| `test/e2e/` | End to end tests that build and run the binaries | internal, gen |
| `docs/adr/` | One decision record per decision | |

## Invariants (reviewers block on these)

1. **Explicit state machines.** Every stateful thing has one `state` field, a closed set of events, and one transition function returning next state and effects. No booleans whose combinations mean something. Use the `berth-state-machine` skill.
2. **Transitions are pure.** No I/O, no `time.Now`, no logging inside a transition; the shell runs effects.
3. **Persist before acting.** The actor saves state and event in one transaction before any effect runs.
4. **State and event names are stored data.** Renaming one breaks existing databases; add new names and migrate instead.
5. **The core imports no adapter.** `depguard` enforces it; `forbidigo` bans `time.Now` and `fmt.Print` in `internal/core`.
6. **Contract first.** A behaviour change starts in `proto/` or a transition table, then tests, then code.
7. **Secrets never cross the API**, never live in a Go string, never reach logs, errors or agent replies. Only `berth-native` and secret helpers read them.
8. **Agents store and use secrets, never read them.** Power actions need Touch ID. Approval text is built only from facts the daemon verified.
9. **Agent methods are opt in.** Only methods with `(berth.agent) = true` are reachable by agents, and `TestAgentSurface` pins the list.
10. **Mutating calls are idempotent** through a `request_id` and the request log.
11. **No third party code runs inside `berthd`.**
12. **Nothing is pushed** until the open source milestone (M8). Commit locally.

## Making a change

1. Read the plan for the milestone in `docs/plans/` and the ADRs it names.
2. Load the skill for the kind of change: `berth-add-rpc`, `berth-state-machine`, `berth-port-adapter`, `berth-milestone`; reviewers load `berth-review`.
3. Change the contract, write the failing test, then the code. A new branch of logic gets a test that fails without it.
4. `make check`, then commit with a plain message (no `type:` prefix, no co-author trailer).
5. If the change alters a decision, add or supersede an ADR and say which wiki page needs the same change.

## Testing

- Unit tests for transition tables (always with `machinetest.Walk` and a pinned legal pair count), stores and parsers.
- End to end tests drive the real binaries; prefer one more e2e case over mocks.
- Use `t.Context()` and `exec.CommandContext`; socket paths must stay under 104 bytes, so use `os.MkdirTemp("", ...)`, not `t.TempDir()`, for a `BERTH_HOME`.
- Never use real secrets; tests plant a canary value and assert it never appears in output.

## Working beside other agents

- Each worker builds in its own git worktree on its own branch; never edit the main checkout.
- Regenerate `gen/` with `make gen` instead of resolving conflicts in it by hand.
- SQLite migrations are numbered; take the next free number when you rebase, and the contiguity test catches gaps.
- `docs/agentic-work.md` explains how work is split so agents rarely touch the same files.

## Lint and hooks

- Go: `golangci-lint` v2 (`.golangci.yml`): gofumpt (told `module-path: berth`), goimports, exhaustive, depguard, forbidigo, gosec, errorlint, noctx, revive and more. `revive`'s exported comment rule is off on purpose.
- Protobuf: `buf format`, `buf lint` (STANDARD), `buf breaking` (FILE).
- Swift: `swift format` from the Xcode toolchain. Python and shell: `ruff`, `shellcheck`.
- Every file: whitespace, final newline, private keys, `gitleaks`, no em or en dashes in Markdown.
- prek runs these as git hooks and only sees tracked files; `make lint` also lints untracked Go and Protobuf.
- Claude Code hooks (`.claude/settings.json`) format and lint each file after an edit and refuse Bash commands that would print a Keychain secret.

## Brand

The kit lives in `brand/` (logo "Sleeping key", app icon, menu bar template, favicons, `colors/tokens.json` and `tokens.css`); `brand/README.md` is the source of truth.

| Name | Hex | Role |
|---|---|---|
| Navy 800 | `#16263F` | Background, top of the gradient |
| Navy 900 | `#0A1424` | Deepest background, bottom of the gradient |
| Mist | `#F4F7FA` | Logo and main text on navy (secondary `#C9D2DC`) |
| Brass | `#F2C46D` | The key; the one accent |
| Cream | `#FBFAF6` | Light background |
| Ink | `#14213D` | Logo and main text on cream (secondary `#4A5568`) |
| Deep brass | `#B7791F` | The key on light backgrounds |

- Fonts: Space Grotesk (wordmark 700, headings 600), Inter (UI and body), JetBrains Mono or system mono (code, keys).
- Dark first: app backgrounds use the navy gradient `linear-gradient(180deg, #16263F 0%, #0A1424 100%)`.
- Brass is one accent per screen, only for the key or the primary action; deep brass is never small body text on cream.
- The wordmark is always lowercase "berth" and is never re-typeset; use the SVG files.
- The menu bar uses `brand/menubar/berthTemplate.png` as a template image, so macOS tints it.

## Writing style

- No dashes as punctuation in docs, comments or messages; hyphens inside words are fine.
- One sentence per line in long Markdown.
- Comments explain why, never what.
