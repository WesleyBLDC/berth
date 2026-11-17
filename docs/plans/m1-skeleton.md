# M1: Skeleton

Goal: the empty frame every later feature drops into: repository rules, schema, state machine kit, store, daemon and recovery.

## Builds

1. **Repository rules:** `Makefile` (`make check`, `make test`, `make e2e`, `make gen`), `golangci-lint` with `exhaustive` and `depguard`, `buf lint` and `buf breaking`, `govulncheck`, `go mod verify`, `prek` hooks.
2. **Contracts:** `proto/berth/v1/` with `DaemonService` (`Status`, `Handshake`) and `DebugService` (`GetState`), `protovalidate` rules, the `(berth.agent)` method option. Generated code committed under `gen/`.
3. **Core kit** (`internal/core/machine`): the `Transition` type, effect runner, one actor goroutine per entity, `Clock` port with a fake, and a test helper that walks every state and event pair of a machine.
4. **Store** (`internal/adapters/sqlite`): `modernc.org/sqlite`, migrations, `SaveTransition` writing state and event in one transaction.
5. **Daemon machine:** `Starting`, `Recovering`, `Serving`, `Draining`, `Stopped`.
6. **Socket:** `~/Library/Application Support/Berth/berthd.sock`, mode 0600 in a 0700 folder, peer user check with `getpeereid` on every connection, protocol version handshake, request IDs for idempotency.
7. **CLI:** `berth status`, `berth debug state daemon`.
8. **Logging:** `slog` with a redacting handler; core dumps off.

## Layout

```text
cmd/berthd  cmd/berth
internal/core/{machine,daemon}
internal/adapters/{sqlite,socket}
proto/berth/v1  gen/
docs/{adr,plans}
```

## Acceptance tests

- The daemon killed during a write restarts into `Recovering`, then `Serving`, with no half written transition.
- A connection from another user is refused (test with a fake peer credential source).
- A client with a wrong protocol version gets a typed error.
- The walk over every state and event pair of the daemon machine passes; an unhandled pair fails the build through `exhaustive`.
- A repeated request ID returns the first result without running twice.

## Exit gate

`make check` is clean, `berth status` talks to `berthd`, and `berth debug state daemon` prints its transitions.
