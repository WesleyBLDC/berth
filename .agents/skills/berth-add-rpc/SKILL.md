---
name: berth-add-rpc
description: "Add or change a Berth API method end to end: proto contract with validation, generated code, handler, idempotency, agent exposure, Go client and CLI, and tests. Use when a task adds, renames or changes a berth.v1 RPC, message or field, or exposes a method to agents."
metadata:
  author: wesley
---

# Add or change an RPC

The schema is the contract every client is generated from, so it changes first and alone.

## Steps

1. **Contract** in `proto/berth/v1/<topic>.proto`:
   - Names follow buf STANDARD: `rpc DoThing(DoThingRequest) returns (DoThingResponse)`.
   - Add `buf.validate` rules to every field a caller sets (lengths, ranges, required).
   - A mutating method takes `string request_id = 1` with `min_len: 8, max_len: 128`.
   - Mark a method for agents only when the plan says so: `option (berth.v1.agent) = true;`.
   - Only add fields; never renumber, retype or remove. `buf breaking` enforces this.
   - A new `ErrorReason` takes the next free number. Parallel branches take the same one, so rebase onto `main` right before the final check and run `make check` (with `buf breaking`) again; renumber yours if `main` took it.
   - A stream has no stored reply: it takes a `request_id` only to hold back a retry while the first stream runs (`h.claim`, as `StartProxy` does).
2. `make gen`, then `buf lint` passes. Commit the contract with its generated code before writing handlers, so other agents can build on it.
3. **Handler** in `internal/adapters/api/`:
   - First line: `if err := validate(req); err != nil { return nil, err }`.
   - Mutating: wrap the work in `once(ctx, h, "<Method>", req.GetRequestId(), &berthv1.<Resp>{}, func() ...)`.
   - The request log is keyed by principal (`api.Principal(ctx)`: the socket, the agent session, the peer uid and the caller's audit token), so a stored reply only ever replays to the caller that made the call. Never look a request id up any other way.
   - A check on who the peer is (`TrustProxy`, `TrustRedeemer`) runs before `once`, so a process replaying a request id learns nothing from the log.
   - Anything counted against a limit goes inside `once`, so a retry that replays costs nothing (see `admitStore` in `RequestSecret`).
   - Call the core through a function in `Deps`; the handler holds no state and makes no decisions.
   - Errors: a `connect` code plus `withReason(..., berthv1.ErrorReason_...)` when a client must react to it; add a new `ErrorReason` value rather than parsing messages. Never put a secret or a stored value in an error message.
4. **Agent surface**: if you marked the method for agents, update `TestAgentSurface` in `api_test.go` in the same change, and follow the agent surface rules below. Decide it for `berth-mcp` too, in `surface` in `internal/mcp/mcp_test.go`: a tool in `internal/mcp/tools.json` (then update `toolsSHA256`) or the reason it is left out.
5. **Client and CLI**: when `Berth.app` reads the message (today `WatchSessions` and `Session`), update the fixtures with `go test ./internal/cli -run TestSwiftFixtures -update-swift` and the Swift models in `macos/Sources/BerthAppCore/Models.swift`. Go tools use `internal/client`; add a `berth` subcommand in `cmd/berth` only when the plan asks, with output a person can read and a golden or e2e test.
6. **Tests** with `connectinprocess` in `api_test.go`: the happy path, validation rejection, the typed error, and for mutating calls a repeated `request_id` running once.
7. `make check` ends with `check: PASS`.

## Agent surface rules

A marked method is served on the agent socket to another user's process that holds a session token, so it is written for a hostile caller.

- The interceptors do the gate: `AgentOnly` refuses unmarked methods, then `AgentToken` needs the `Berth-Agent-Token` header of a live session, from the agent uid, inside the session's process tree. A handler never reads the token.
- Scope by `api.AgentSession(ctx)`: the method sees and changes only what belongs to that session. Another session's id answers the same not found error as an unknown id, so ids cannot be probed. Nothing lists across sessions except names in `ListSecrets`.
- The same method on the owner socket has `api.AgentSession(ctx) == ""`; say what that means (usually no limit and no scoping) and test both.
- Give it a per session limit, counted in memory under a mutex with `agentsession.Limit` (at once and per rolling hour), for everything it starts (a dialog, a process, a browser), answer `ERROR_REASON_RATE_LIMITED` beyond it, and check the limit before any work that writes a row or starts a process. Drop the session's counts when it ends.
- What it starts ends with the session: record the agent session on the thing, and stop it from the agent actor's `saved` hook (see `berth-state-machine`), also at boot for sessions that ended while `berthd` was down.
- A start the caller can abandon runs on `berthd`'s context, and whoever takes its outcome owns it: a cancelled caller hands the result to a goroutine that closes what started (see `managed.start`). Check the session again once the start settles, since its end hook may have run while the thing had no row.
- It never returns a path, a pid, raw arguments, a ticket a separate user could redeem, or a token, except its own create call.
- The end to end test calls it with no token, a forged token and a token from outside the tree, and checks the token and any canary are in no log, database file or other output.

## Do not

- Return secrets, Keychain values or tokens in any response. The one exception is a create call (`CreateAgentSession`, `LaunchBrowser`, `OpenAgentBrowser`), which returns the new token once and keeps it out of the request log with `onceKeeping`; `berthd` stores only its SHA-256, and a retry gets the thing without the token. Add any such field to `TestNoFieldCarriesAValue`'s list on purpose.
- Print a string a process, a server or an agent chose without `shown` (`internal/cli/sessions.go`).
- Add a method to the agent surface "for convenience".
- Hand edit anything under `gen/`.
