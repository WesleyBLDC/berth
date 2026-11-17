# E3: Isolated agents

Goal: protect secrets and your files from a hostile agent that can run arbitrary code, which E2a explicitly does not (see the threat model in `ARCHITECTURE.md`).
Added 2026-11-16 at Wesley's request, after the E2a reviews showed that a broker cannot fully defend against code running as the same user.
Rewritten as work items on 2026-11-17, after the scout compared the options; the decision is [ADR 0018](../adr/0018-agents-run-as-a-separate-macos-user.md).

## Idea

Coding agents run as a different macOS user than you, so your dotfiles, Keychain, `berth.db` and home folder are out of their reach, and they can only ask `berthd` for what their grants allow.
Three identities take part:

| Identity | Runs | Owns |
|---|---|---|
| Owner (you) | `berthd`, `berth-native`, and the `berth-proxy` of every agent session | Your home, Keychain, `berth.db`, `BERTH_HOME` |
| Agent (for example `berthagent`) | The harness (Claude Code, Cursor agents) and everything it starts | Its own home, dotfiles and `PATH`; group write in the shared workspace |
| Runner (for example `berthrunner`) | The consumers of `berth run --env` for agent sessions, started by `berthd`, never by the agent | Its own home, dotfiles and `PATH`, which the agent cannot write; the agent cannot signal it |

## Provisional picks

The maintainer has not answered these yet; each is **provisional, awaiting the maintainer**, and work goes ahead on them so nothing blocks.

| # | Question | Provisional pick |
|---|---|---|
| Boundary | What isolates the agent | A dedicated standard macOS user runs agents (the scout's recommendation over Seatbelt, a Linux VM or Endpoint Security); [ADR 0018](../adr/0018-agents-run-as-a-separate-macos-user.md) |
| D1 | Who runs env consumers | A third "runner" user that the agent cannot write to or signal; `berthd` starts them, never the agent's process |
| D2 | One agent user or one per session | One shared agent user for now |
| D3 | How agent processes start as another user | Decided after the spike, which needs the maintainer's admin password: ssh to localhost, a root LaunchDaemon, or sudo; expect a small root launch helper |
| D4 | Seatbelt | An optional Seatbelt layer inside the agent user, off by default |
| D5 | Linux VM | A later backend; env secrets are always refused there, only the header proxy works |
| D6 | Where agents work | A shared workspace at `/Users/Shared/berth-work`, owned by a group (`berth`) holding the owner, the agent and the runner |
| D7 | Paid Apple program | None before M8, so no Endpoint Security and no notarization in E3 |

## Options the scout compared

| Option | Isolation | Cost | Verdict |
|---|---|---|---|
| A dedicated standard macOS user | Strong: separate home, Keychain, dotfiles; the socket sees a different uid; it cannot signal or trace your processes | Admin rights once to create users; workspace sharing through a group; harness login and tool setup per user | Chosen, provisional |
| `sandbox-exec` (Seatbelt) profiles around the harness | Medium: file and network rules, same uid, deprecated API, so signals, the socket and the Keychain stay the owner's | Low setup; fragile profiles | Optional extra layer (D4) |
| A Linux VM per agent through Apple's `container` tooling | Strongest | Tools run in Linux, not macOS; browsers and Keychain need bridging | Later backend (D5) |
| Endpoint Security client watching agent processes | Detection, not isolation | Needs the paid program and an entitlement | Later watch item (D7) |

## Spike facts (2026-11-17)

- **Verified:** `kern.procargs2` fails for another user's processes, so `berthd` cannot read the argv (and so the debugging port) of a Chrome the agent user starts. Agent browsers must be launched through Berth (item 8).
- **Unconfirmed, needs the spike:** plain `sudo -u` may leave the agent in the owner's audit session and launchd domain, which would give it the owner's pasteboard and GUI services.

## Fixing the dotfiles limit properly

The first draft of this plan ran `berth run` consumers as the agent identity.
That does not close the E2a dotfiles limit, because the agent owns that `HOME` and `PATH`, so a hostile agent can still configure the tool that receives the secret.
The consumer runs as the runner identity instead, started by `berthd` (through the launch helper of D3), with the agent's stdin, stdout and stderr passed over the socket (`SCM_RIGHTS`).
The agent cannot write the runner's home or `PATH`, and cannot send it signals, so the suspended cdhash check of E2b item 7 also holds against an agent that signals: it can no longer `SIGCONT` the suspended child.
Configuration inside the repository (`.git/config`, git hooks, a project `.npmrc`, `package.json` scripts) stays agent controlled; that is an honest limit, and the header proxy stays the mode for API keys.
`berth-native` and `berth-proxy` are always started by `berthd` as the owner for agent sessions, never by a process the agent controls.

## Work items

"Needs the maintainer" means the item needs admin rights or a real second user and cannot finish without him; every other item can be built and tested now as one user, by setting the agent uid to your own uid in tests.

| # | Item | Needs the maintainer | Owns | Acceptance tests |
|---|---|---|---|---|
| 1 | Spike | Yes: admin password, creates real users | A results section in this plan, ADR 0018's status | Answers every spike question below with a command and its output |
| 2 | `berth agent setup` and `uninstall` | Code now; its e2e runs only by hand with admin rights | `internal/cli/agent*.go`, a setup plan package | Unit: the printed plan (users, group, folders, modes) for a given config; by hand: setup then uninstall leaves no user, group or folder behind |
| 3 | The agent socket | No (built) | `internal/adapters/socket`, `internal/adapters/api` (the agent only interceptor), `internal/berthd`, `cmd/berthd` flags | See [item 3](#item-3-the-agent-socket) |
| 4 | Session tokens bound to the agent uid and its process tree | No for the logic (built); the cross user check needs a real second user | An agent session machine, a migration, `api` | See [item 4](#item-4-session-tokens-bound-to-the-agent-uid-and-its-process-tree) |
| 5 | Spawn on behalf | Code now; the real runner launch needs D3 | `internal/berthd/spawn.go`, the spawn protocol, `berth run` agent mode | Same user: stdio arrives over `SCM_RIGHTS` and the consumer gets the env; by hand: the consumer runs as the runner with the runner's `HOME` and `PATH` |
| 6 | `berthd` starts `berth-proxy` for agent sessions | No | `internal/berthd/proxy.go`, a `StartProxy` agent method | The proxy's parent is `berthd`; an agent run gets the port, credential and CA paths, never a value |
| 7 | `berth agent start`, with the optional Seatbelt profile | Launch as another user needs D3; the Seatbelt profile does not | `internal/cli/agent_start.go`, a Seatbelt profile | Seatbelt on: a write outside the workspace fails (one user, no admin); by hand: the harness runs as the agent user in the workspace |
| 8 | Browser brokering | Launch as another user needs D3; the rest waits on M4 | M4's `BrowserDriver`, a `LaunchBrowser` agent method | The agent gets only a CDP proxy endpoint and token; a Chrome the agent starts itself is listed as not brokered |
| 9 | Honest limits | No | `ARCHITECTURE.md` honest limits | Every limit below is written down with the item or milestone that narrows it |
| 10 | Acceptance tests | Yes for the cross user ones | `test/e2e/agent_*_test.go`, a by hand script | The [acceptance tests](#acceptance-tests) |
| 11 | Later: the Linux VM backend and an Endpoint Security watch item | Yes | New ADRs | Not part of the E3 exit gate |

### Item 1: the spike

Needs the maintainer's admin password and creates a throwaway second user; it decides D3 and moves ADR 0018 from Proposed to accepted or revised.
Questions, each answered with the command run and its output:

1. For each launch path (`sudo -u`, `ssh berthagent@localhost`, a root LaunchDaemon that starts a process as the user, `launchctl asuser`): which audit session id (`asid` in the audit token) and launchd domain does the agent process land in, and can it read the owner's pasteboard (`pbpaste`), take a screenshot, or post events to the owner's windows?
2. Can the agent user read the owner's home? macOS creates homes `0750` with group `staff`, and new standard users are in `staff`, so the answer may be yes until setup tightens it.
3. Can `berthd` (the owner) watch an agent process's exit with kqueue `EVFILT_PROC`, and read its parent pid with `proc_pidinfo`, given that `kern.procargs2` already fails across users?
4. Can a Chrome started as another user show windows on the owner's display, or only run headless?
5. Does Claude Code log in and run as the agent user with its own Keychain, and what does that need?
6. What does `/Users/Shared` look like by default (mode, owner, sticky bit), so setup can create `/Users/Shared/Berth` and `/Users/Shared/berth-work` safely?

### Item 2: `berth agent setup` and `uninstall`

- `setup` creates the agent user and the runner user (hidden, standard, no admin), the `berth` group with the owner, the agent and the runner in it, the workspace `/Users/Shared/berth-work` (group `berth`, setgid, group writable, with an inheriting ACL so new files stay group writable), the socket folders of item 3 (`/Users/Shared/Berth` owned by root with mode `0755`, and `/Users/Shared/Berth/<owner uid>` owned by the owner with the agent group and mode `0710`; `berthd` never creates either), the launch helper of D3, and writes `berthd`'s agent uid and gid.
- `uninstall` removes the users, the group, the helper and the socket folders; it keeps the workspace unless `--purge` is given.
- Both need admin rights, so the code is built and its plan printing is unit tested now, while the real run is a by hand check by the maintainer. Nothing in `make check` runs it.

### Item 3: the agent socket

Built; it is tested as one user.

- `berthd` listens on a second Unix socket when it is given an agent uid (`berthd -agent-uid <uid> -agent-gid <gid> [-agent-socket <path>]`). With no agent uid the agent socket is off; an agent uid without a gid, or uid 0, is refused at start.
- **Path:** `/Users/Shared/Berth/<owner uid>/agent.sock` by default, since `BERTH_HOME` sits inside the owner's `~/Library`, which is `0700` and so unreachable for another user.
- **Folders made by setup, never by `berthd`:** setup (item 2) creates `/Users/Shared/Berth` owned by root with mode `0755`, and the socket folder `<owner uid>/` owned by the owner with group `<agent gid>` (the `berth` group) and mode `0710`, so the agent can reach the socket but not list the folder. The socket is `0660` with the same group, and the owner must be a member of that group, which setup arranges. When a folder is missing, the agent socket does not start and `berthd` says to run `berth agent setup`.
If `/Users/Shared/Berth` or the socket folder already exists and is not owned and moded exactly as above (for example an agent created it before setup ran), setup refuses and names it, and the maintainer removes it by hand; setup never adopts or changes a folder it did not create.
- **No swapping:** `berthd` walks the path as written from `/`, one component at a time, opening each level with `O_NOFOLLOW` and checking the open descriptor. It follows a symlink only when root owns it (so `/tmp` and `/var` work), and refuses any level owned by someone other than root or the owner, or writable by others unless it is a root owned sticky folder like `/Users/Shared`. The socket folder itself must be the owner's, with the agent group and mode `0710`. Go cannot bind relative to the checked descriptor, so `berthd` walks the path again after the bind and serves only if it reaches the same folder; since every level belongs to root or the owner and no one else can write it, only root could change a level in that window.
- **Peer check:** every connection's peer uid is read with `LOCAL_PEERCRED` before a byte is read, as on the owner socket, and must equal the configured agent uid; any other connection is closed. The owner socket keeps refusing every uid but the owner's, so the agent can reach `berthd` only here.
- **Methods:** only methods marked `(berth.v1.agent) = true` are served; every other method answers `PermissionDenied` with the new reason `ERROR_REASON_NOT_AGENT_METHOD`. The check reads the method's schema option, so a newly marked method is served without further wiring, and `TestAgentSurface` pins both the list and the refusal.
- **Before items 5 and 6:** past the session token check of item 4, the agent methods answer exactly as on the owner socket. `AuthorizeUse` and `AuthorizeProxy` hand out tickets that only a `berth-native` or `berth-proxy` redeeming on the owner socket can use, and the owner socket refuses the agent uid, so a ticket an agent obtains here leads to no value.
- **Tests:** an e2e case starts the real `berthd` with the agent uid set to the test's own uid and proves `ListSecrets` works and `Status` is refused on the agent socket; with a different uid every call fails; unit tests cover the folder checks and run the agent only interceptor over every method in the schema.

### Item 4: session tokens bound to the agent uid and its process tree

Built (2026-11-17) and tested as one user, with the agent uid set to the test's own uid; the cross user run waits for a real second user.

- **Issuing:** only the owner, on the owner socket, creates a session: `berth agent session new [--name N] [--ttl D]` (`CreateAgentSession`). The reply carries the token once; `berthd` stores only its SHA-256 in `agent_sessions` (migration 0009), and the request log keeps the reply without the token, so a retry gets the session but no token. `berth agent session ls` and `berth agent session revoke <id>` list and end sessions; none of `AgentService` is on the agent surface.
- **Machine:** `agent/<id>`, states `Issued`, `Bound`, `Expired`, `Revoked` and `Ended`, 6 legal pairs, no effects. Each live session has a timer that sends `Expire` at its expiry, and a bound session's root is watched with kqueue `NOTE_EXIT` (procscan's `WatchExit`, which checks the start time) to send `RootExited`; both run on `berthd`'s own context, are cancelled when the session ends, and are armed again at boot for live sessions. A call or a listing that finds a session past its expiry or its root gone records it too.
- **Transport:** the token travels in the `Berth-Agent-Token` request header, never in a message, a URL, an argument, a log or an event note. The `berth` command uses the agent socket when `BERTH_AGENT_SOCKET` is set and reads the token from the file `BERTH_AGENT_TOKEN_FILE` names, which must be a regular file of the caller's own with no group or other bits.
- **Binding:** every agent method on the agent socket, `Handshake` included, needs a token of a live session. The method check runs first, so a method closed to agents is refused as closed, token or not. The first accepted call binds the session to the caller's process tree root: the highest ancestor of the caller that still runs as the agent uid, stopping below launchd and below the first process of another user (the harness, under a launch helper; the process itself, when launchd adopted it). The root is written once with a compare and set on the row, so of two racing first calls one tree wins. Every later call must come from the agent uid and have the root, by pid and start time, in its lineage.
- **Process facts:** the lineage is read one parent at a time with `kern.proc.pid` (procscan's `Lineage`, beside M2's process table code), which the kernel answers for every user. The walk stops at a parent that started after its child, since that is a later process on a reused pid, so a race can only shorten a lineage. The caller's audit token is then checked to still name a live process (`csops_audittoken`, the same check the signed binary trust uses), so a caller pid reused while its lineage was read is refused.
- **Refusals:** one reason, `ERROR_REASON_AGENT_TOKEN_INVALID` with `Unauthenticated`, for a missing, unknown, expired or revoked token, an ended session, and a caller outside the tree, so a copied token learns nothing; `berthd` logs which it was, with the session id and pid, never the token.
- **Scoping:** the request log principal on the agent socket now names the session (`agent/<session>:<uid>:<audit token>`), so two sessions never replay each other's replies. `ListSecrets` stays global and names only, since an agent must see that a name exists before it asks for it again; no other agent method lists anything today, and proxy sessions were already scoped to the audit token that authorized them.
- **Rate limit:** `RequestSecret` from one session may hold at most 3 store dialogs open and start at most 10 in any rolling hour; beyond that it answers `ResourceExhausted` with `ERROR_REASON_RATE_LIMITED` and opens nothing. A retry that replays a stored reply costs nothing, and the owner socket is never limited.
- **Tests:** unit tests for the table, the root and descendant rules, a lineage walk through a reused parent pid, a Bind racing a revoke, the store limits, the bind once row, the proxy stop on an agent session's end, a FIFO or symlink as the token file, and the api interceptor; end to end with the real binaries: no token and a forged token refused, a valid token accepted from two processes of the tree, the same token from a process launchd adopted refused, revoked and expired tokens refused, the rate limit, and the token absent from `berthd`'s log, every database file and every output but the one that printed it.

#### Provisional decisions of item 4

Taken while the maintainer was away; each is **provisional, awaiting the maintainer**.

| # | Question | Provisional pick |
|---|---|---|
| T1 | Which process a session binds to | The first caller's highest ancestor that runs as the agent uid, not the first caller itself: the first caller is usually a short lived `berth` command, so binding to it would end the session at once. Item 7's `berth agent start` can instead record the harness when it creates the session |
| T2 | Machine states | `Issued`, `Bound`, `Expired`, `Revoked`, `Ended` instead of this plan's earlier `Starting`, `Running`, `Ending`, `Ended`, `Lost`, which describe a harness `berthd` launches (item 7); state names are stored data, so item 7 adds states rather than renaming these |
| T3 | Time to live | 12 hours by default, at most 7 days, set per session with `--ttl` |
| T4 | Token shape | `berth_at_` and 32 random bytes in unpadded base64url, so scanners and people recognise a leak; the session id is a separate random value, not derived from the token |
| T5 | How the token reaches `berth` | A file named by `BERTH_AGENT_TOKEN_FILE`, mode 0600, and the socket in `BERTH_AGENT_SOCKET`; never an argument or an environment value, since tools print their environment |
| T6 | Rate limit | 3 open store dialogs and 10 per rolling hour per session, counted in memory |
| T7 | Handshake | Needs a token like every other agent method, so an agent without one learns only that it was refused |

#### Honest limits of item 4

- **Bind on first use:** whoever presents the token first binds the session. The token must reach the harness before any other process can read it; a token copied before its first use binds to the copier's tree.
- **Reparenting to launchd:** a process that leaves its tree (a double fork, `nohup` with its parent gone, a launchd job) is adopted by launchd and refused, so a background helper of the harness loses the session. This fails closed by design.
- **Parent pid races:** the lineage is read one level at a time. A parent that exits mid walk ends the lineage there; a parent whose pid a later process took mid walk is caught because it started after its child, and the walk stops there too, so a race only shortens a lineage and the call is refused. The root is matched by pid and start time, and the caller's own pid is pinned by its audit token.
- **One long lived process at the top shares one root:** the root is the highest agent uid ancestor, so when the agent user's sessions all hang below one long lived agent uid process (an `ssh` login, a `tmux` server, a spawner), every session binds to that same root and the tree check separates nothing between them. Item 7 closes this by binding the session to the harness `berthd` launches.
- **The token file is readable by every agent process:** with one shared agent user (D2), any agent uid process can read every session's token file, so sessions are not isolated from each other by the file, only by first use binding. Item 7 hands the token to the harness through an inherited file descriptor instead of a file.
- **Revocation reaches live work:** a proxy session authorized through the agent socket records its agent session. Once the agent session's final state is saved (revoked, expired by its timer, or its root exited), the agent actor's saved hook stops its proxy sessions, sending `STOP` (`PROXY_STOP_REASON_AGENT_SESSION_ENDED`) to their watch streams as a grant revoke does. The stop runs on `berthd`'s own context in its own goroutine, so a caller that cancels its revoke still stops every proxy, and the agent actor never waits on the proxy actors. At boot, any proxy session still open for an agent session that already ended is stopped. A proxy authorized just as its agent session ended is refused. Calls already past the token check finish.
- **One user tests prove the logic, not the boundary:** with the agent uid equal to the owner's, the root is a broad ancestor (a terminal's shell, a `tmux` server), so the tree check separates little. Real separation needs the agent user of items 1 and 2.
- **Unverified across users:** that `csops_audittoken` answers for another user's process, which the liveness check needs, is unconfirmed until the spike; if it does not, every cross user call is refused, which fails closed.
- **The store limit counts calls, not dialogs:** an agent that gives up on a call frees its slot while the dialog stays open on the owner's screen; the hourly limit still bounds it, and a restart of `berthd` resets both counts.
- **Watches are armed again only for sessions boot recovery sees as live:** a session recover finds expired or with its root gone ends at once, and an ended one only has its proxies stopped. A database edited while `berthd` is down is outside the model.
- **Refused tokens are logged once per call,** so an agent can grow the log; it belongs with the rate limited refusal warning in the follow ups.
- A bound session keeps one actor goroutine until it ends; ended sessions hold none.

### Item 5: spawn on behalf

- Connect over HTTP cannot carry file descriptors, so spawn uses a small framed protocol on a third socket beside the agent socket (`spawn.sock`, same folder, modes and uid check): one JSON line with the use ticket, argv, a working folder inside the workspace and the allowed environment names, plus the agent's stdin, stdout and stderr as `SCM_RIGHTS`.
- `berthd` asks the launch helper (D3) to start the signed `berth` in a consumer mode as the runner, holding the passed descriptors and one end of a socketpair, suspended; it starts `berth-native read` as the owner with the other end and a ticket bound to that runner process's audit token.
- The value travels from `berth-native` to the runner's `berth` to the consumer's environment, never through `berthd` (invariants 7 and 11). The runner's `berth` runs the E2b cdhash check on the suspended consumer; the agent cannot signal either process.
- The agent's `berth run --env` waits on the spawn socket for the exit status, so the terminal experience matches E2a.

### Item 6: `berthd` starts `berth-proxy` for agent sessions

- A new agent method `StartProxy` (names and a request id) makes `berthd` start the signed `berth-proxy` as the owner; the proxy authorizes on the owner socket as today.
- `berthd` returns the port, the per run credential and the CA bundle path, which lives in a run folder under the agent socket folder, readable by the `berth` group.
- The agent's `berth run --proxy` starts its child as the agent (the child holds no secret); the session ends when the agent's run stream closes, so a crashed agent cannot leave a proxy running.

### Item 7: `berth agent start`

- `berth agent start <harness> [--sandbox]` starts the harness as the agent user through the launch helper, in the workspace, with the agent's own `HOME` and `PATH`, the agent socket path and the session token (item 4), and the person's terminal attached.
- The token reaches the harness through an inherited file descriptor, never a file other agent processes could read, and `berthd` records the harness as the session's root when it creates the session, rather than binding on first use.
- `--sandbox` (D4, off by default) wraps the harness in a Seatbelt profile that denies writes outside the workspace and the agent's home and temporary folders. `sandbox-exec` needs no admin rights, so this part is tested as one user. Seatbelt is a deprecated API and its profiles are fragile, which is why it is an extra layer and never the boundary.

### Item 8: browser brokering

- `berthd` launches Chrome (M4's `BrowserDriver`, over the private CDP pipe) and the agent gets only the CDP proxy endpoint and its token; it never gets the pipe, the profile folder or Chrome's debugging port.
- Because `kern.procargs2` fails across users, a Chrome the agent user starts itself cannot be inspected; M2 lists it as a browser owned by the agent user, not brokered.
- Chrome runs as the runner identity (provisional), so CDP calls that write files (downloads) cannot land in the owner's files; the CDP proxy also refuses `Browser.setDownloadBehavior` to paths outside the workspace.

### Item 9: honest limits

These go into `ARCHITECTURE.md` when the items that create them land.

- Repository configuration stays agent controlled: `.git/config`, git hooks, a project `.npmrc`, `package.json` scripts and `Makefile` targets run as the runner with the injected environment, so env injection into a consumer that reads repository config can still leak. The header proxy is the mode for API keys.
- With one shared agent user (D2), agent sessions can read each other's files and signal each other; only the session token and process tree separate them at the API.
- The workspace is group writable: anything the owner runs there as the owner (a build, a test, a script the agent wrote) runs agent written code with the owner's rights.
- The agent can still use granted secrets through the proxy, exfiltrate through granted APIs, and reach the network, unless the Seatbelt layer or the VM backend restricts it.
- Until the spike answers it, the agent may share the owner's audit session and launchd domain (pasteboard, GUI services) when started with plain `sudo -u`.
- The owner's home may be readable by the agent through the `staff` group until setup tightens it.
- `berthd` cannot see another user's process arguments (`kern.procargs2`), so it can broker only browsers it launched.
- Before setup, the agent user can create `/Users/Shared/Berth` itself; that blocks setup (setup refuses a folder it did not create) but grants nothing, because `berthd` refuses a folder it does not trust and the agent socket does not start. After setup, nothing can squat the folders: the parent is root owned and the socket folder owner owned.
- Free signing (D7): no Endpoint Security and no notarization until M8.

## Follow ups from the review

- Refresh the stat after a root owned symlink jumps to `/` as the last component, so the final check reads the right folder (only root can set this up).
- Rate limit the warning logged for each refused connection, so an agent cannot flood the log.

- The folder check reads only the owner and mode bits and ignores ACLs, so an ACL that grants another user write on a folder in the agent socket's path passes the check. Setup creates the folders without ACLs; the check should also refuse an ACL entry that grants write or delete to anyone but the owner.
- Done in item 4: `RequestSecret` is limited per agent session to 3 open store dialogs and 10 an hour.
- A process that execs another image in place keeps its pid and start time, so a signature or arguments cached for it describe the old image until the session ends.
- A failed argument read is cached as nil, so a process whose arguments could not be read once is never read again.
- `AuthorizeUse` prompts have no per session limit yet, unlike store dialogs.
- `agent_sessions` rows are never pruned; ended sessions stay in the table.
- `AdmitStore` reads the session's state from the database while holding the agents mutex, which serializes store admissions behind a database read.

## Acceptance tests

| Test | Kind | Status |
|---|---|---|
| A non agent method on the agent socket is refused with `PermissionDenied`; an agent method works | e2e, one user | Built (item 3) |
| A connection whose uid is not the agent uid is closed on the agent socket | e2e and unit, one user | Built (item 3) |
| The owner socket refuses the agent uid | Unit, one user | Built (M1) |
| Without a valid session token every agent method is refused; a valid token works; expired and revoked tokens are refused | e2e, one user | Built (item 4) |
| The same token from a process outside the session's tree (one launchd adopted) is refused | e2e, one user | Built (item 4) |
| A token from another uid is refused | Unit, one user; by hand, real second user | Built as a unit test (item 4); by hand planned |
| One session can hold at most 3 store dialogs and start 10 an hour | e2e, one user | Built (item 4) |
| Revoking or expiring an agent session stops the proxy sessions authorized through it | Unit, one user | Built (item 4) |
| The token never reaches a log, a database file or an output other than the one that issued it | e2e, one user | Built (item 4) |
| From the agent identity: reading your home, Keychain items, `berth.db` and dotfiles fails | By hand, real second user | Planned (items 1, 2) |
| The agent cannot signal or trace the runner consumer or `berthd` | By hand, real second user | Planned (item 5) |
| A consumer started on behalf of an agent sees the runner's `HOME` and `PATH`, not the agent's | One user e2e for the protocol; by hand for the uid | Planned (item 5) |
| A hostile agent script that tries every E2a review attack (driving signed binaries, fake daemon, wrapper helpers, tool configuration) gets no secret value | By hand, real second user | Planned (item 10) |
| A Chrome the agent starts itself is not brokered; one launched through Berth gives the agent only the CDP proxy | e2e with M4 | Planned (item 8) |

## Order

After E2b and before or alongside M6, since M6 browser credentials face the same threat.
Item 3 lands first, since it needs nothing from the maintainer; item 4 is built; items 6 and the Seatbelt half of 7 follow as one user; the spike (item 1) unblocks setup (2), spawn (5), agent start (7) and browser launch as another user (8).
