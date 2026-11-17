# M0 spike results

Run on macOS 26.5, Go 1.27.4, Swift 6.3.3, Xcode 26.6, Chrome 157.

| # | Proof | Result |
|---|---|---|
| 1 | Swift streams from Connect in Go over a Unix socket | Pass |

## 1. Swift streaming over the Unix socket

- Go server: Connect v2 (`connectrpc.com/connect/v2` 2.0.2), mounted with `connecthttp.WithGRPC()` and `WithProtoJSON()`, served by `net/http` with unencrypted HTTP/2 (`http.Protocols.SetUnencryptedHTTP2`) on a Unix socket created with mode 0600 in a 0700 folder, with a `LOCAL_PEERCRED` user check on every accepted connection.
- Swift client: `grpc-swift-2` 2.x with `GRPCNIOTransportHTTP2` (`.unixDomainSocket(path:)`, plaintext).
- Server stream: 1,000 of 1,000 messages in order in 10 ms. Client stream: 1,000 of 1,000 input events in 6 ms.
- `buf curl --unix-socket --http2-prior-knowledge --protocol grpc` also works for debugging.

Findings for the real build:

- The current BSR Connect plugin generates Connect **v2** code; v2 needs `WithGRPC()` explicitly or gRPC callers (the Swift app) are rejected.
- The Swift plugin for grpc-swift 2 is `buf.build/grpc/swift-protobuf`; `buf.build/grpc/swift` still generates grpc-swift 1 code.
- buf's STANDARD lint wants `<Method>Request` and `<Method>Response` message names.
- Plain `curl` with JSON works for unary calls; streams need the Connect envelope, so use `buf curl` for streams.
- Not tested: a connection from another user (needs a second account); the check itself is in place.
