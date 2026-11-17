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
2. `make gen`, then `buf lint` passes. Commit the contract with its generated code before writing handlers, so other agents can build on it.
3. **Handler** in `internal/adapters/api/`:
   - First line: `if err := validate(req); err != nil { return nil, err }`.
   - Mutating: wrap the work in `once(ctx, h, "<Method>", req.GetRequestId(), &berthv1.<Resp>{}, func() ...)`.
   - Call the core through a function in `Deps`; the handler holds no state and makes no decisions.
   - Errors: a `connect` code plus `withReason(..., berthv1.ErrorReason_...)` when a client must react to it; add a new `ErrorReason` value rather than parsing messages. Never put a secret or a stored value in an error message.
4. **Agent surface**: if you marked the method for agents, update `TestAgentSurface` in `api_test.go` in the same change.
5. **Client and CLI**: when `Berth.app` reads the message (today `WatchSessions` and `Session`), update the fixtures with `go test ./internal/cli -run TestSwiftFixtures -update-swift` and the Swift models in `macos/Sources/BerthAppCore/Models.swift`. Go tools use `internal/client`; add a `berth` subcommand in `cmd/berth` only when the plan asks, with output a person can read and a golden or e2e test.
6. **Tests** with `connectinprocess` in `api_test.go`: the happy path, validation rejection, the typed error, and for mutating calls a repeated `request_id` running once.
7. `make check` ends with `check: PASS`.

## Do not

- Return secrets, Keychain values or session tokens in any response. The one exception is `CreateAgentSession` on the owner socket, which returns a new agent session's token once and keeps it out of the request log with `onceKeeping`.
- Add a method to the agent surface "for convenience".
- Hand edit anything under `gen/`.
