# M2: See every browser

Goal: `berth ps` lists every Chromium family browser running on this Mac, says what it is (Chrome, Chrome for Testing, Chromium, Edge, Brave, a headless shell), whether Playwright or Puppeteer drives it, whether it is headless and how its DevTools protocol is exposed, and who started it (an agent, a terminal, an app or the person); `berth inspect` shows one in detail, `berth stop` ends one, and `berth leftovers` lists and stops the browsers whose owner has gone.
M2 only observes: Berth launches nothing and speaks no CDP until M4.
The maintainer was away when this plan was written and asked not to be blocked; every choice that is his to make is listed under [Decisions for the maintainer](#decisions-for-the-maintainer), with the default taken.

## What the kernel lets Berth see (checked on 2026-11-17, macOS 26.5)

| Fact | Owner's processes | Another user's processes | Source |
|---|---|---|---|
| pid, parent pid, uid, start time, short name | Yes | Yes | `sysctl kern.proc.all`, one call for the whole table |
| Executable path | Yes | Yes (261 of 262 other user processes on this Mac) | `proc_pidpath` (raw `proc_info`, as `internal/adapters/peer` already does) |
| Arguments (`argv`) | Yes | **No**, `EINVAL` | `sysctl kern.procargs2` |
| CPU time | Yes | Not relied on | `proc_pidinfo(PROC_PIDTASKINFO)` |
| Exit notification | Yes | Not relied on | kqueue `EVFILT_PROC` `NOTE_EXIT` (already used for `berth-proxy` in `internal/berthd/procexit.go`) |

So a browser run by another user (a future E3 agent user) can be recognised by its executable path, but its flags, profile and driver are opaque.
M2 lists such a browser as **unmanaged, other user**: it shows the kind its path implies and nothing from its arguments, never tries to read them, and refuses to stop it.

The M0 spike ([ADR 0015](../adr/0015-m0-spike-results.md)) proved the parts M4 builds on (Chrome over `--remote-debugging-pipe` in its own process group, the token gated CDP proxy, `berthd` stopping every Chrome process group it owns on SIGTERM); it did not test discovery of browsers Berth did not start, which is why this milestone starts with the kernel facts above.
One spike finding binds M2: tokens must not travel as command line flags, and other tools may still put credentials in a browser's `argv` (for example `--proxy-server` with a password), so Berth never returns, stores or logs raw arguments, only the facts it parsed from them.

## Design

```mermaid
%%{init: {"theme":"dark","flowchart":{"nodeSpacing":50,"rankSpacing":60}}}%%
flowchart LR
  K["kernel<br/>process table"]
  PS["ProcessSource<br/>darwin adapter"]
  DISC["discovery<br/>pure, in core"]
  SIG["signature<br/>registry"]
  OWN["owner<br/>chain"]
  SM["session<br/>machine"]
  API["SessionsService"]
  K --> PS --> DISC
  SIG --> DISC
  OWN --> DISC
  DISC -->|events| SM
  SM --> API
```

Observe, then reconcile, as `ARCHITECTURE.md` planned for sessions: the scan loop only reports what it saw as events, and every scan sends corrective events, so a missed kqueue note or a daemon restart heals itself at the next scan.

### ProcessSource (port) and its darwin adapter

- `internal/core/ports/process.go`: `ProcessRef{PID, Start}` names one process instance, since a pid alone is reused; `ProcessInfo{Ref, PPID, UID, Executable}`; `ProcessSource` with `List`, `Args`, `CPUTime`, `WatchExit` and `Signal`.
- `Args` returns `ErrOpaqueProcess` for a process the kernel will not describe (another user's), and `ErrProcessGone` when the pid now names another start time, so a reused pid never answers for the old one.
- `Signal` re-reads the start time immediately before `kill(2)` and refuses a mismatch; the window between the two calls is an honest limit below.
- Adapter `internal/adapters/procscan` (darwin): `kern.proc.all` for the table, `proc_pidpath` per process, `kern.procargs2` parsed by a pure function with its own fixture tests, `PROC_PIDTASKINFO` for CPU, kqueue for exits.
- The table comes from `kern.proc.all` rather than `proc_listpids` plus one `PROC_PIDTBSDINFO` call per pid: one call returns a consistent snapshot with parent, uid and start time, and `x/sys/unix` already wraps it.
- No audit token or cdhash: an audit token needs a task port or a socket connection Berth does not have for a browser it did not start, and nothing in M2 is granted to a browser's code. M4 launches its own browsers and M6 pins grants to profiles, not binaries.

### Signature registry

`internal/core/browsersig`, pure: `Classify(executable, args, argsKnown)` returns a `Signature` with the browser kind, the driver, headless, the CDP exposure, the profile folder and the role.

| Recognised by | Result |
|---|---|
| `Google Chrome.app/Contents/MacOS/Google Chrome` | Chrome |
| `Google Chrome for Testing.app/.../Google Chrome for Testing` | Chrome for Testing |
| `Chromium.app/Contents/MacOS/Chromium` | Chromium |
| `Microsoft Edge.app/Contents/MacOS/Microsoft Edge` | Edge |
| `Brave Browser.app/Contents/MacOS/Brave Browser` | Brave |
| An executable named `chrome-headless-shell` or `headless_shell` | Headless shell, always headless |
| A `* Helper*.app` inside one of those bundles, or `--type=` in its arguments | A helper of that browser, never a session root |
| An executable under `ms-playwright/`, or a profile named `playwright_chromiumdev_profile-*` or under `ms-playwright/mcp-*` | Driver Playwright |
| An executable under `.cache/puppeteer/`, or a profile named `puppeteer_dev_chrome_profile-*` | Driver Puppeteer |
| `--enable-automation` with neither of the above | Driver unknown automation |
| `--headless`, `--headless=new` or `--headless=old` | Headless |
| `--remote-debugging-pipe`; `--remote-debugging-port=N` | CDP over a pipe; CDP on port N (0 means Chrome picked one) |
| `--user-data-dir=PATH` | Profile folder |

- Electron apps (Slack, Cursor, Beeper) are not browsers: their `chrome_crashpad_handler` and helpers sit inside their own bundles and match nothing.
- With `argsKnown` false (another user's process) only the path rules apply, and the signature says so.
- The registry is a table of rules, tested by a table of real paths and argument lists copied from this Mac.

### Owner chain

`internal/core/ownerchain`, pure: given a session root's ancestors (parent first, up to launchd), each with its executable and, for the owner's processes, its arguments, it returns the chain and the owner.

- The owner is the nearest ancestor recognised as an **agent** (`claude`, `codex`, `cursor-agent`, `gemini`, `aider`, `opencode`, `amp`, `goose`, including those run through `node`, `bun` or `deno`); else the nearest **terminal** (Terminal, iTerm2, Ghostty, WezTerm, kitty, Alacritty, Warp, or `tmux` and `screen` servers); else the nearest **app** (any other `.app` bundle executable, for example Cursor or VS Code); else the **person**, when a browser app's main executable was started by launchd (from the Dock, Finder or `open`); else **none**, when a browser with real driver evidence (a Playwright or Puppeteer install or profile, headless, or a CDP pipe) already hangs from launchd, which means its owner has exited; a CDP port alone is not evidence, since a person can open their own browser with one; else **process**, the browser's parent, when no rule recognises any ancestor (a launchd job, say).
- The chain stops at the first ancestor the kernel will not describe, as the peer adapter's does.
- The owner is a process instance (`ProcessRef`), so its exit is detectable later by its start time.

### Discovery (the pure part of the scan)

`internal/core/discovery`, pure: given the process table, the arguments it read, the CPU times and the sessions it already knows, it returns the observed sessions and the events to send.

- A **session root** is a process the registry recognises as a browser and whose parent is not a process of the same browser; every descendant of a root belongs to its session.
- The session id is `<pid>-<start time in microseconds>`, the entity `session/<id>`; the same browser keeps its id across daemon restarts, and a reused pid is a new session.
- Events per scan: `Busy` when the session's CPU time grew by at least 20 ms since the last scan and the session is `Discovered` or `Idle`; `Quiet` when it has not grown for 60 seconds and the session is `Discovered` or `Active`; `OwnerExited` when the owner's `ProcessRef` is gone from the table, or the owner is none at discovery; `Exited` when the root is gone.
- The shell only sends an event the table accepts in the current state, so a steady browser writes no rows.

### Session machine (observed states)

`internal/core/session`, entity `session/<id>`, kind `session`.

```mermaid
%%{init: {"theme":"dark"}}%%
stateDiagram-v2
  [*] --> Discovered
  Discovered --> Active: Busy
  Idle --> Active: Busy
  Discovered --> Idle: Quiet
  Active --> Idle: Quiet
  Discovered --> Leftover: OwnerExited
  Active --> Leftover: OwnerExited
  Idle --> Leftover: OwnerExited
  Discovered --> Stopping: StopRequested / SignalTerm
  Active --> Stopping: StopRequested / SignalTerm
  Idle --> Stopping: StopRequested / SignalTerm
  Leftover --> Stopping: StopRequested / SignalTerm
  Stopping --> Stopping: StopTimedOut / SignalKill
  Discovered --> Stopped: Exited
  Active --> Stopped: Exited
  Idle --> Stopped: Exited
  Leftover --> Stopped: Exited
  Stopping --> Stopped: Exited
```

- 17 legal pairs, pinned by `machinetest.Walk`.
- `Leftover` is sticky: a browser whose owner has gone stays a leftover however busy it is, until it exits or is stopped.
- `StopTimedOut` comes from a 5 second timer the shell starts with `SignalTerm`; `SignalKill` kills the root and every process still in the session.
- A signal that fails because the process already exited is not a failure: the next scan or `NOTE_EXIT` sends `Exited`.
- M4 adds `Launching`, `Ready` and `Failed` for browsers Berth starts; the names here are stored data and never change.

### berthd's scan loop

- One goroutine in `internal/berthd/sessions.go`: a full scan every 2 seconds, plus a kqueue `NOTE_EXIT` watch on every session root, so an exit is seen at once and a new browser within 2 seconds.
- Arguments are read only for browser processes and their ancestors, not for the whole table.
- Each session is an actor, like secrets and grants; facts (the signature, owner, chain, processes) live in memory beside it and are rebuilt by every scan, so no new table or migration is needed.
- Boot: the first scan rediscovers live browsers under their old ids, and any session the database holds in a live state that the scan does not find gets `Exited` with the note `gone at boot`.

### RPCs and the CLI

| Command | RPC | Prints |
|---|---|---|
| `berth ps [--all]` | `ListSessions` | One line per session: id, state, kind, driver, headless, CDP, owner, process count; `--all` adds sessions that stopped since `berthd` started |
| `berth inspect <id>` | `InspectSession` | The session, its processes with their roles, the owner chain, and the recent transitions |
| `berth stop <id> [--force]` | `StopSession` | The final state; waits up to 10 seconds |
| `berth leftovers [--stop]` | `ListSessions` filtered to `Leftover`, then `StopSession` for each | The leftovers, or what was stopped |

- **Stop needs no Touch ID.** It only ends a process the person could end with `kill`, and stopping lowers what an agent can do, like a revoke. To keep a person's own browser safe from a careless call, a session whose owner is the person or an app refuses a stop without `force` (`ERROR_REASON_SESSION_NOT_STOPPABLE`), and so does every session of another user, with or without `force`.
- **No M2 method is on the agent surface.** Listing shows every browser on the Mac, including the person's own profile folders and other agents' sessions, which ADR 0006 and ADR 0009 keep from an agent; and stopping another session is a cross session action. M4 adds agent methods that see only the caller's own session, once session tokens exist. Adding a method to the agent surface later is an addition; taking one away would break agents.

## Contract (proto additions)

New file `proto/berth/v1/sessions.proto`, and two values appended to `ErrorReason` in `options.proto`; nothing existing changes, so `buf breaking` stays clean.

```proto
service SessionsService {
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);
  rpc InspectSession(InspectSessionRequest) returns (InspectSessionResponse);
  rpc StopSession(StopSessionRequest) returns (StopSessionResponse);
}
enum SessionState { UNSPECIFIED, DISCOVERED, ACTIVE, IDLE, LEFTOVER, STOPPING, STOPPED }
enum BrowserKind { UNSPECIFIED, CHROME, CHROME_FOR_TESTING, CHROMIUM, EDGE, BRAVE, HEADLESS_SHELL }
enum Driver { UNSPECIFIED, NONE, PLAYWRIGHT, PUPPETEER, AUTOMATION }
enum CdpMode { UNSPECIFIED, NONE, PIPE, PORT }
enum OwnerKind { UNSPECIFIED, AGENT, TERMINAL, APP, PERSON, NONE, PROCESS }
enum ProcessRole { UNSPECIFIED, BROWSER, HELPER }
message SessionProcess { int32 pid; Timestamp started_at; string executable; ProcessRole role; }
message SessionOwner { OwnerKind kind; string name; SessionProcess process; }
message Session {
  string id; SessionState state; BrowserKind kind; Driver driver; bool headless;
  CdpMode cdp; uint32 cdp_port; string user_data_dir; SessionOwner owner;
  SessionProcess browser; uint32 process_count; bool other_user; uint32 uid; Timestamp state_since;
  bool args_unreadable;
}
message ListSessionsRequest { repeated SessionState states; bool include_stopped; }
message ListSessionsResponse { repeated Session sessions; }
message InspectSessionRequest { string id; uint32 history_limit; }
message InspectSessionResponse {
  Session session; repeated SessionProcess processes; repeated SessionProcess owner_chain;
  repeated Transition history;
}
message StopSessionRequest { string request_id; string id; bool force; }
message StopSessionResponse { SessionState state; }
// ErrorReason
ERROR_REASON_SESSION_NOT_FOUND = 14;
ERROR_REASON_SESSION_NOT_STOPPABLE = 15;
```

- Validation: `id` matches `^[0-9]{1,10}-[0-9]{1,20}$`; `request_id` 8 to 128 characters as everywhere; `history_limit` at most 500; `states` items defined only.
- `StopSession` is idempotent through the request log; a stop of a session already `Stopped` answers `STOPPED`, not an error.
- `user_data_dir` and `cdp_port` are empty for another user's session, and `other_user` is set.
- `Transition` is the existing message from `debug.proto`.

## Work items

Each item ends with `make check` passing and its own commit.

| # | Item | Depends on | Owns | Tests |
|---|---|---|---|---|
| 1 | Plan | | `docs/plans/m2-see-every-browser.md` | |
| 2 | Contract | 1 | `proto/berth/v1/sessions.proto`, `options.proto` reasons, `gen/`, `internal/core/ports/process.go` (`ProcessSource`, and `EntityLister` for boot recovery) | `buf breaking` clean; `TestAgentSurface` unchanged |
| 3 | ProcessSource darwin adapter | 2 | `internal/adapters/procscan/` | Parser fixtures for `procargs2`; against the real kernel: the test process appears with its parent and start time, its arguments read back, pid 1's arguments are `ErrOpaqueProcess`, a child started by the test reports `NOTE_EXIT`, `Signal` refuses a stale start time |
| 4 | Signature registry | 2 | `internal/core/browsersig/` | A table of real executables and argument lists from this Mac, including Electron helpers that must not match |
| 5 | Session machine | 2 | `internal/core/session/` | `machinetest.Walk` pinned at 17; Leftover sticky; stop then kill; names stable |
| 6 | Owner chain | 2, 4 | `internal/core/ownerchain/` | Agent through `node`, terminal, app, person, none |
| 7 | Discovery | 3 to 6 | `internal/core/discovery/` | Roots and helpers grouped; reused pid is a new session; Busy and Quiet thresholds; OwnerExited; Exited; other user opaque |
| 8 | Scan loop and RPCs | 5, 7 | `internal/berthd/sessions.go`, `internal/adapters/api/sessions.go`, `internal/client/` | `connectinprocess` tests: list, inspect, not found, stop refused without force for a person's browser and always for another user, repeated `request_id` |
| 9 | CLI | 8 | `internal/cli/sessions.go`, `cmd/berth/` | Golden output of `ps` and `inspect` |
| 10 | End to end and docs | 8, 9 | `test/e2e/sessions_test.go`, `ARCHITECTURE.md` | Below |

Items 3 to 6 can run in parallel after the contract; 7 joins them; 8 to 10 are the integration.

**End to end tests** (`test/e2e/sessions_test.go`) start a real headless browser when one is installed and skip with the reason otherwise.
The test looks for `BERTH_E2E_CHROME`, then the newest `chrome-headless-shell` under `~/Library/Caches/ms-playwright`, then Puppeteer's headless shell and Chrome for Testing under `~/.cache/puppeteer`, then `/Applications/Google Chrome.app`, and installs nothing.
On this Mac all four exist: Chrome, Arc, Playwright's Chromium 1223, 1243 and 1248 with their headless shells, and Puppeteer's Chrome for Testing 131 and 154 with their headless shells.

- `ps` shows a headless shell the test started, with driver Playwright, CDP on a port, the test's profile folder, and the owner being the test's process chain; `inspect` lists its helpers.
- A browser started by a short lived owner program that then exits becomes `Leftover` and is listed by `leftovers`; `stop` ends it, and the session is `Stopped` within 10 seconds. The test never runs `leftovers --stop`, which would end every leftover on the machine, not only its own.
- Killing a browser outside Berth makes its session `Stopped` through `NOTE_EXIT` or the next scan.
- A canary in the browser's arguments appears in no `ps` or `inspect` output, no `berthd` log line and no database byte.
- Tests kill only the pids they started.

## Exit gate

`make check` passes with the e2e suite (and the skip path is shown by running it with `BERTH_E2E_CHROME=/nonexistent` and no browser found); `berth ps` on the maintainer's Mac shows his Chrome as owned by the person and a Playwright MCP browser as owned by its agent; the agent safety checklist is walked for M2; `ARCHITECTURE.md` marks M2 done.

## Honest limits

- **Another user's browsers are opaque.** Their arguments are refused by the kernel, so kind comes from the path alone, and driver, headless, CDP and profile are unknown. M2 does not stop them.
- **Signals race pid reuse by a hair.** `Signal` checks the start time and then calls `kill(2)`; a process that exits and whose pid is reused in between would get the signal. Pids wrap only after about 99,999 starts, so this needs both an exit and a full wrap inside microseconds.
- **Discovery is a poll.** A browser that starts and exits inside one 2 second scan is never seen, and a new browser shows up to 2 seconds late.
- **Recognition is by path and flags,** which a process controls: a renamed or copied Chrome is missed, and any executable placed at a matching path is listed as a browser. M2 only lists and stops, so a false match costs a wrong line, never a grant.
- **The owner is a guess from the process tree.** A browser started through `launchd`, `open`, or a daemon that reparents its children shows the person or none as the owner, and a tool that double forks makes its browser look like a leftover at once.
- **A restart forgets who started a browser.** Sessions recovered at boot start with `Tracked.Seen` false, so parent and owner are recorded afresh, and a parent or owner that changed while berthd was down goes unnoticed (only a browser with driver evidence left hanging from launchd still becomes `Leftover`).
- **Active and Idle come from CPU time,** not from page activity; a page playing a video is Active with no agent driving it, and an agent waiting on a slow site looks Idle.
- **Stopped sessions stay in the database** as machine rows; a test suite (or a hostile process) that launches thousands of browsers adds thousands of `machine_state` and `machine_event` rows until a later pruning pass. Memory stays bounded: a stopped session's actor is stopped at once, berthd keeps at most the 200 newest stopped sessions and none older than an hour (older ones drop out of `ps --all`; a stop of one still answers `STOPPED` from its row), and every string kept about a session is cut to 1,024 bytes with the mark `...(truncated)`.
- **Arguments berthd cannot parse safely are not guessed.** A `procargs2` buffer whose layout does not match the kernel's (argv at the pointer aligned offset after the exec path), or with more than 4,096 arguments, makes the session show `arguments unreadable` with driver, headless, CDP and profile unknown; so a process can hide its CDP port this way, but it cannot make berthd read its environment as arguments.
- **Names come from the process.** Executable paths, profile folders and owner names are whatever the process chose; `berth ps` and `inspect` print any with control or format characters quoted with escapes, so they cannot drive the terminal, but they can still lie.
- **A person's browser is protected by a flag, not Touch ID.** Anything that can reach the socket can pass `force`; until session tokens (M4) the daemon cannot tell an agent from the person.

## Decisions for the maintainer

Defaults taken while he was away; each is cheap to reverse before M2 closes.

1. **Stop has no Touch ID.** Stopping only ends a process the person could `kill`; a person's or an app's browser needs `force`, and another user's cannot be stopped at all.
2. **No M2 method is on the agent surface.** Agents get scoped methods in M4 with session tokens.
3. **Raw arguments never leave `berthd`** and are never stored; the API returns only the parsed facts.
4. **The registry covers Chrome, Chrome for Testing, Chromium, Edge, Brave and headless shells.** Arc (installed on this Mac), Vivaldi, Opera and Electron apps are left out; Arc is one table row if wanted. Firefox and Safari wait for M7.
5. **Leftover means the owner exited after discovery, or a browser with real driver evidence already hangs from launchd.** Evidence is a Playwright or Puppeteer install path or profile, headless, or a CDP pipe. A CDP port alone is not: a non headless browser launchd started (for example `open -a ... --args --remote-debugging-port=9222`) belongs to the person and is never a leftover, and `stop` (and `leftovers --stop`) need `--force` for it. Decided in review on 2026-11-17.
6. **Activity is CPU time:** `Busy` at 20 ms or more of CPU growth between scans, `Quiet` after 60 seconds without it.
7. **Scan every 2 seconds** plus kqueue `NOTE_EXIT` on each session root.
8. **No new table:** facts are rebuilt by every scan, and only machine rows persist; stopped sessions are pruned in a later milestone.
9. **Two new error reasons, 14 and 15.** E3 took 13 (`NOT_AGENT_METHOD`) on `main` while M2 was built, so M2 renumbered when it rebased.
10. **The table comes from `kern.proc.all`,** not `proc_listpids`: one consistent snapshot with parent, uid and start time in one call.

## Exit notes (2026-11-17)

Built on branch `m2a` from `main` at 6de1d7b: items 1 to 10, each with its own commit, and `make check` passing.

- **Contract.** As planned, plus `OWNER_KIND_PROCESS` (a browser whose ancestors match no rule) and the `EntityLister` port, which `sqlite` and the in memory store implement so boot recovery can find live session rows without a table. The reasons are 14 and 15, after E3's 13.
- **ProcessSource** (`internal/adapters/procscan`): `kern.proc.all` plus `proc_pidpath` lists about 1,000 processes in about 4 ms on this Mac (`BenchmarkList`), so a 2 second scan costs about 0.2% of one core. `procargs2` is parsed only up to `argv` and its buffer is cleared, since the environment after it can hold secrets. CPU time converts mach ticks with `hw.tbfrequency` (24 MHz on Apple silicon). The `NOTE_EXIT` watch moved here from `internal/berthd/procexit.go`, and the proxy sessions use it unchanged. `peer` exports `Executable` and `PIDInfo` so the raw `proc_info` call stays in one place.
- **Signature registry** (`internal/core/browsersig`): also recognises the Chrome Beta, Dev and Canary bundles as Chrome, and `chrome-devtools-mcp` profiles as Puppeteer. A CDP port wins over a pipe when both flags are set, since any local process can reach a port.
- **Session machine** (`internal/core/session`): 17 legal pairs, pinned.
- **Owner chain** (`internal/core/ownerchain`): terminals are matched by their app bundle (Warp's executable is called `stable`) and multiplexers by executable.
- **Discovery** (`internal/core/discovery`): a session also becomes `Leftover` when the browser's parent at first sight is gone (it was reparented), not only when the owner is; that is what a crashed test runner looks like.
- **Scan loop** (`internal/berthd/sessions.go`): sessions that never saw an event have no database row, since `Discovered` is the initial state; `debug state session/<id>` shows a session only after its first transition.
- **End to end** (`test/e2e/sessions_test.go`): passes with Playwright's `chrome-headless-shell` 1248 on this Mac, and skips cleanly with `BERTH_E2E_CHROME=/nonexistent`.

### Agent safety checklist (M2)

1. No agent method was added; the agent socket's `AgentOnly` interceptor refuses all three sessions methods, since none is marked.
2. No secret value is touched. A browser's arguments can hold credentials (`--proxy-server` with a password), so raw arguments stay inside `discovery`, are never stored, logged or returned, and the `procargs2` buffer, which also holds the environment, is cleared after parsing.
3. Canary: `discovery` plants one in a browser argument and checks no observed field holds it; the end to end test plants one and checks `ps`, `inspect`, the log and the database.
4. Stopping a browser is not a power action (decision 1); a person's or an app's browser needs `force`, another user's is refused.
5. Event notes are fixed strings. Executable paths, profile folders and owner names are what the kernel reports, but the process chose them, so berthd cuts each to 1,024 bytes and the CLI prints any with control or format characters (ESC, CR, LF, other C0 and C1, DEL, bidi overrides) quoted with escapes; JSON output is escaped by the encoder.
6. `StopSession` takes a `request_id` and replays through the request log.
7. A failed signal is logged and the next scan reconciles; refusals are typed (`SESSION_NOT_FOUND`, `SESSION_NOT_STOPPABLE`).
8. OWASP agentic risks: unchanged; the new limits are listed above.

### What remains for the exit gate

- The maintainer runs `berth ps` on his Mac with his own Chrome and a Playwright MCP browser open, and confirms the owners read as planned.
- `ARCHITECTURE.md` marks M2 done after that check.

### Review fixes (2026-11-17)

- **Bounded memory:** `actors` stops a session's actor when it reaches `Stopped` (see Later fixes); `ended` keeps at most 200 sessions and none older than an hour; kept strings are clipped (`discovery.Clip`). `TestChurnStaysBounded` churns 600 fake browsers and fails without either bound.
- **Terminal safety:** every process chosen string in `ps`, `inspect` and `stop` output goes through `shown`, which quotes it with `strconv.QuoteToGraphic` when it holds anything not graphic; `TestSessionTextIsEscaped` plants ESC, CR, LF, BEL, DEL, U+202E and U+0085.
- **Stop branches:** tests fail if `App` leaves the force rule, or if the SIGKILL fan out drops the uid filter; `StopTimedOut` is driven through to `SignalKill` with a 20 ms grace.
- **Leftover evidence:** a CDP port alone no longer counts (decision 5).
- **procargs2:** argv is read at `4 + roundup(len(exec path) + 1, 8)`, measured on macOS 26.5 for exec paths of every length modulo 8; a buffer that does not fit that layout, or that has more than 4,096 arguments, is refused, and the session carries `args_unreadable`. An empty `argv[0]` from the real kernel reads back as `["", "30"]`; the old parser read the environment as the second argument.
- **kqueue:** `WatchPID` blocks on an `EVFILT_USER` wake instead of a one second poll, so ending its context closes the kqueue at once; `WatchExit` ends a refused watch right away. `TestRefusedWatchesCloseTheirKqueue` fails without it (204 descriptors open after 200 refused watches).
- The agent socket test names the three sessions methods.

### Later fixes (2026-11-17)

- **No actor outlives Stopped:** `actors` takes a `final` state; an actor stops itself once a transition reaches it, and none is started for an entity already in it, so the boot `recover` path, a `StopRequested` racing the exit, and the grace timer racing the exit can no longer leave a goroutine for a Stopped session. `TestBootEndsSessionsThatAreGone` and `TestLateEventsToAStoppedSessionStartNoActor` fail without it.
- **Arguments once per process:** `discovery.Scanner` keeps each browser process's parsed signature, and the arguments of `node`, `bun` and `deno` ancestors (`ownerchain.ReadsArgs`; no other ancestor's arguments are read now), keyed by pid, start time and executable, since exec keeps the pid and start time; each scan keeps only what it listed. `TestScannerReadsArgumentsOncePerProcess` fails without it. `BenchmarkScan` with one headless Chrome for Testing (11 processes) open: before 2.8 to 3.0 ms, 1.32 MB and 4,780 allocations a scan; after 2.1 to 2.4 ms, 1.23 MB and 4,510, about 10 KB and 25 allocations saved per browser process per scan. Most of what remains is the table read.
