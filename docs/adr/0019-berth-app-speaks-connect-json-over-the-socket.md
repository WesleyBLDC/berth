# 0019: Berth.app speaks Connect JSON over the socket

Status: Proposed, 2026-11-17; a default taken while the maintainer was away. It supersedes the line of [ADR 0015](0015-m0-spike-results.md) that names grpc-swift-2 for the Swift client.

## Context

`Berth.app` must stream `WatchSessions` from `berthd` and, in later items, call the screen and control methods.
The spike proved grpc-swift-2 with its NIO transport over the Unix socket.
Since then `berth-native` has shipped a small Connect JSON client over HTTP/1.1 on a raw socket, which first asks the kernel for the peer's audit token and checks that the signed `berthd` answers before it writes a byte.

| Option | Peer check before the first byte | What the signed app links | Generated code |
|---|---|---|---|
| grpc-swift-2, NIO transport | No: the transport owns the socket | About twenty packages, including `swift-nio-ssl`, `swift-crypto` and `swift-certificates` | A Swift step in `make gen` |
| Connect JSON over HTTP/1.1 on a raw socket | Yes, as `berth-native` does | Foundation and the Security code signing calls only | None; hand written models for the messages the app uses |
| JSON through the `berth` CLI | No | Nothing extra | None, but text parsing and no stream |

## Decision

The app speaks the Connect protocol with JSON over HTTP/1.1 on its own Unix socket connection, one connection per call, after the same audit token check `berth-native` makes.
The socket code moves into a `BerthSocket` target of `native/` that both import.
The hand written models are kept true by fixtures a Go test writes from the real schema with `protojson`.

## Why

Checking who answers before sending anything matters once the app sends the person's keystrokes during a takeover; it is also what `berth-native` already does and the maintainer already checked.
A small client is easier to audit than twenty packages in a signed bundle, and it adds no code generation step.
The cost is that Connect has no client streaming over HTTP/1.1, so takeover input travels as unary batches, and the models must learn each new field by hand.
