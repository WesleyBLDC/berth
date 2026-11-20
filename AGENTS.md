# Berth: agent instructions

Berth is a local macOS app that lists every browser agents, tests and scrapers have open, lets a person watch and take over any of them, and brokers Keychain secrets to browsers and terminals without agents ever reading them.
The architecture, with what is built and what is planned, is in `ARCHITECTURE.md`; the decisions that bind code are in `docs/adr/`, the current milestone's plan is in `docs/plans/`, and the agent safety checklist is in `docs/agent-safety.md`.
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
| `make fmt` | Format Go, Protobuf, Swift and shell |
| `make gen` | Regenerate `gen/` after editing `proto/` |
| `make build` | `bin/berthd`, `bin/berth` and `bin/berth-proxy`, signed, and `bin/berth-mcp`, unsigned (it holds nothing and nothing trusts it) |
| `make test-app` | `Berth.app`'s Swift tests, and its bundle built and checked signed ad hoc; opens no window |
| `make app` | `bin/Berth.app`, signed, with `berthd` and its helpers in `Contents/Helpers` |
| `make native` | `bin/berth-native`, the Swift helper, built in release mode and signed |
| `make test-native` | Swift unit tests for the helper's core |
| `make breaking` | `buf breaking` against the local `main` branch |
| `make gate` | `scripts/gate.sh check`: `make check`, failing unless it exits 0 and ends with `check: PASS` |
| `scripts/gate.sh merge <branch>` | From the main checkout: rebases the branch onto `main` in its worktree, checks it, fast forwards `main` and checks again; never pushes |
| `scripts/gate.sh push` | gitleaks and a case insensitive personal data scan over every new commit, then a fast forward push of `main`, only of a tree whose clean checkout passed `make check` through the gate; never forces |
| `make gitleaks-full` | gitleaks over the whole history |

The gate prints every step and its exit code, holds a lock so a merge and a push never overlap, stops at the first failure with nothing merged or pushed, and `scripts/test-gate.sh` (in `make check`) proves it; never hand chain `git rebase`, `make check`, `git merge` and `git push` instead, since one pipe such as `| tail -1` can hide a failed step.
Planned by [the test lab](docs/plans/test-lab.md), not built yet: `make lab-image` (builds the tart base image) and `make lab-test` (the unattended VM suites).

Binaries: `berthd` (daemon), `berth` (CLI and the `berth top` TUI), `berth-proxy` (one per `berth run --proxy`), `berth-native` (Swift, the only reader of Keychain values), and `berth-mcp` (an MCP server over stdio for agents without a shell).
Build tags: `berth_lab` (planned, ADR 0020) will compile a lab only certificate authority and scripted approvals for the VM tests, and nothing else. `berth_e2e` compiles the test only pin rule, the test root of `berth-proxy`, loopback proxy targets and the `BERTH_NATIVE`, `BERTH_PROXY` and swap hooks; `test/e2e` builds its binaries with it, release builds never, and no environment variable turns it on.

## Where things live

| Path | Holds | May import |
|---|---|---|
| `proto/berth/v1/` | The API contract: services, messages, validation rules, the `agent` method option, typed error reasons | |
| `gen/` | Generated code, committed; never edit by hand | |
| `internal/core/ports/` | Interfaces the core needs (`Store`, `Clock`, `RequestLog`, `EventLog`, `ProcessSource`, `BrowserDriver` and the rest) and their domain types | stdlib only |
| `internal/core/machine/` | The state machine kit: `Transition`, `Actor`, `machinetest.Walk` | ports |
| `internal/core/<machine>/` | One package per state machine (`daemon`, `secret`, `grant`, `proxysession`, `agentsession`, `session`, `browser`) | machine, ports |
| `internal/core/<policy>/` | Pure rules shared by the daemon and its clients (`envpolicy`, `hostpolicy`, `cdppolicy`) and browser discovery (`browsersig`, `ownerchain`, `discovery`) | stdlib, ports, session |
| `internal/adapters/<name>/` | Implementations of ports and the API (`sqlite`, `socket`, `api`, `clock`, `native`, `execid`, `peer`, `ticket`, `procscan`, `cdppipe`, `cdpproxy`) | core, gen |
| `internal/berthd/` | The daemon shell: wires adapters, runs machines, performs effects | everything |
| `internal/client/` | The Go client every Go tool uses to reach the daemon | gen, api |
| `internal/cli/` | The `berth` command logic and the `berth top` TUI, testable against an in process server | client, gen, api |
| `internal/proxy/`, `internal/proxyctl/`, `internal/proxyproc/` | The `berth-proxy` logic; the two JSON lines it exchanges with its starter and the rules every end checks (standard library only); the starter `berth` and `berthd` share | gen, api, adapters; never a UI module |
| `internal/peertrust/`, `internal/codetrust/`, `internal/spawn/`, `internal/mask/`, `internal/paths/`, `internal/logging/`, `internal/token/`, `internal/strictjson/` | Trust in signed Berth peers, the suspended start for `berth run --env`, output masking, default paths, the redacting log handler, random ids and tokens with the hashes berthd keeps, the one strict reader for JSON another program wrote | |
| `internal/testutil/` | Fakes for ports (in memory store, fake clock, scriptable secrets, process table and browser driver) | ports |
| `internal/mcp/` | `berth-mcp`: MCP over stdio for agents without a shell; its tools are fixed text in `tools.json`, and its test maps every agent method to a tool or a reason | client, gen; never a UI module |
| `cmd/` | `berthd`, `berth`, `berth-proxy` and `berth-mcp` entry points, thin | internal |
| `native/` | `berth-native`, the Swift helper that alone touches secret values, and `BerthSocket`, the socket and peer check code it shares with the app | |
| `macos/` | `Berth.app`: `BerthAppCore` (the Connect JSON client, models, session store), the SwiftUI shell, `berth-app-probe` for the end to end tests, `Bundle/` plists | `BerthSocket` only, never Keychain code |
| `test/e2e/` | End to end tests that build and run the binaries | internal, gen |
| `ARCHITECTURE.md` | The canonical architecture: processes, layers, state machines, storage, security model, milestone status | |
| `docs/adr/` | One decision record per decision | |
| `docs/plans/` | One plan per milestone, with acceptance tests | |

## Invariants (reviewers block on these)

1. **Explicit state machines.** Every stateful thing has one `state` field, a closed set of events, and one transition function returning next state and effects. No booleans whose combinations mean something. Use the `berth-state-machine` skill.
2. **Transitions are pure.** No I/O, no `time.Now`, no logging inside a transition; the shell runs effects.
3. **Persist before acting.** The actor saves state and event in one transaction before any effect runs.
4. **State and event names are stored data.** Renaming one breaks existing databases; add new names and migrate instead.
5. **The core imports no adapter.** `depguard` enforces it; `forbidigo` bans `time.Now` and `fmt.Print` in `internal/core`.
6. **Contract first.** A behaviour change starts in `proto/` or a transition table, then tests, then code.
7. **Secrets never cross the API**, never live in a Go string, never reach logs, errors or agent replies. Only `berth-native`, secret helpers and `berth-proxy` (through `berth-native`, to write a granted header) read them.
8. **Agents store and use secrets, never read them.** Power actions need Touch ID. Approval text is built only from facts the daemon verified.
9. **Agent methods are opt in.** Only methods with `(berth.v1.agent) = true` are reachable by agents, the agent socket serves nothing else and needs a session token on every call, and `TestAgentSurface` pins the list.
10. **Mutating calls are idempotent** through a `request_id` and the request log, which is keyed by principal (socket, agent session, peer uid and audit token), so no caller ever replays another's reply.
11. **No third party code runs inside `berthd`.**
12. **Public repository, no releases yet.** `WesleyBLDC/berth` takes direct pushes to `main` after `make check` passes and the change is reviewed; no releases or tags until M8. Never push secrets, personal paths or private notes.
13. **Tokens appear in one reply, once.** Only a create call (`CreateAgentSession`, `LaunchBrowser`) returns a token, the request log keeps that reply without it (`onceKeeping`), and `berthd` stores only its SHA-256.
14. **Bounded memory.** Every map, queue, goroutine and actor that a process, an agent or a client can make grow has a limit and a test; an actor stops at its machine's final state (`final` in the actors kit).

## Making a change

1. Read the plan for the milestone in `docs/plans/` and the ADRs it names.
2. Load the skill for the kind of change: `berth-add-rpc`, `berth-state-machine`, `berth-port-adapter`, `berth-milestone`; reviewers load `berth-review`. `berth-secrets` is for agents that use Berth, not for building it.
3. Change the contract, write the failing test, then the code. A new branch of logic gets a test that fails without it.
4. `make check`, then commit with a plain message (no `type:` prefix, no co-author trailer). Nothing is merged except by `scripts/gate.sh merge <branch>` and nothing is pushed except by `scripts/gate.sh push`, which refuses a tree that has not passed the gate's check.
5. A change to behaviour or structure updates `ARCHITECTURE.md` in the same commit (state diagrams, layers, storage, security model, milestone status).
6. If the change alters a decision, add or supersede an ADR in `docs/adr/`.

## Testing

- Unit tests for transition tables (always with `machinetest.Walk` and a pinned legal pair count), stores and parsers.
- End to end tests drive the real binaries; prefer one more e2e case over mocks.
- A test that waits on goroutines or timers runs in `synctest.Test` and waits with `synctest.Wait` or `synctest.Sleep`, never by polling; t.Run cannot be called inside a bubble, so each subtest opens its own. A goroutine blocked on a real socket, kqueue or an `os/exec` pipe is never durably blocked, so neither `synctest.Wait` nor fake time can move past it and the test hangs; keep such tests outside bubbles and use `testutil.Eventually`.
- Use `t.Context()` and `exec.CommandContext`; socket paths must stay under 104 bytes, so use `os.MkdirTemp("", ...)`, not `t.TempDir()`, for a `BERTH_HOME`.
- Never use real secrets; tests plant a canary value and assert it never appears in output.
- `Berth.app`'s Swift models are hand written (ADR 0019); a change to a message the app reads reruns `go test ./internal/cli -run TestSwiftFixtures -update-swift` and updates the Swift models and tests. Swift tests never launch the app or ask for a permission.
- `Berth.app`'s views are compared with the images in `macos/Tests/BerthAppSnapshotTests/Snapshots/`, drawn offscreen in a window that is never shown; after an intended UI change, rerun with `BERTH_SNAPSHOT_RECORD=1 make test-app` and look at every changed image, dark ones included. A failure writes the actual image and a diff to `$TMPDIR/berth-snapshots/`.
- Every injected dependency's real default gets its own test against the real thing (the kernel, a real process, real SQLite): a fake only proves the caller.
- Browser tests find an installed Chromium (`BERTH_E2E_CHROME`, then the Playwright and Puppeteer caches) and skip with the reason when there is none; they install nothing and kill only the pids they started.

## Working beside other agents

- Each worker builds in its own git worktree on its own branch; never edit the main checkout.
- Regenerate `gen/` with `make gen` instead of resolving conflicts in it by hand.
- SQLite migrations are numbered; take the next free number when you rebase, and the contiguity test catches gaps.
- `docs/agentic-work.md` explains how work is split so agents rarely touch the same files.
- `scripts/gate.sh merge <branch>` rebases onto `main` and runs `make check` again, `buf breaking` included: parallel branches take the same `ErrorReason` and migration numbers, and only the rebased tree shows it.

## Lessons from the review rounds

What the parallel workers and their reviews learned; the skills carry the detail.

- **Cleanup hangs off the actor, on berthd's context.** Free what an entity held (timers, watches, slots, child processes, other entities) from the actors kit's `saved` hook once the final state is saved, on its own goroutine with `berthd`'s context, never from the call that caused the end: a caller that cancels must not leave work half stopped.
- **A pid is not a process.** Name a process by pid and start time or by its audit token, watch exits with kqueue `NOTE_EXIT`, and reap a child only after everything keyed by its pid is gone.
- **Parsers must agree.** When Berth filters a message another program then parses (CDP JSON, HTTP, `procargs2`), refuse anything the two could read differently instead of guessing.
- **Text a process or agent chose goes through `shown`** (`internal/cli/sessions.go`) before it reaches a terminal, in the CLI and the TUI alike, and is clipped before it is kept.
- **`berth-proxy` links no UI module.** `TestBerthProxyLinksNoUIModules` runs `go list -deps` and fails on any TUI dependency, which is why the control lines and the shared rules (names, placeholders, owned variables, timeouts, trust files) live in `internal/proxyctl`, which imports only the standard library.
- **Docs say what the code does.** A limit, a count or a status in `ARCHITECTURE.md` or a plan that the code does not meet is a review blocker.

## Lint and hooks

- Go: `golangci-lint` v2 (`.golangci.yml`): gofumpt (told `module-path: berth`), goimports, exhaustive, depguard, forbidigo, gosec, errorlint, noctx, revive and more. `revive`'s exported comment rule is off on purpose.
- Protobuf: `buf format`, `buf lint` (STANDARD), `buf breaking` (FILE).
- Swift: `swift format` from the Xcode toolchain. Shell: `shellcheck`, and `shfmt -d` styled by `.editorconfig`'s `[*.sh]` section (two spaces, POSIX, indented `case` items).
- Markdown: `rumdl check` (`.rumdl.toml`: line length and first line heading off), which only reports and never rewraps.
- Every file: whitespace, final newline, private keys, `gitleaks`, no em or en dashes in Markdown. There is no Python, so there is no Python linter.
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
