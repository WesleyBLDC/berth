# M0 spike results

Run on macOS 26.5, Go 1.27.4, Swift 6.3.3, Xcode 26.6, Chrome 157.

| # | Proof | Result |
|---|---|---|
| 1 | Swift streams from Connect in Go over a Unix socket | Pass |
| 2 | Playwright MCP through the token gated CDP proxy | Pass |
| 3 | Chrome screencast frames shown in SwiftUI | Pass |

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

## 2. Playwright MCP through the token gated CDP proxy

- Chrome 157 launched with `--remote-debugging-pipe` and a temporary profile, in its own process group; no TCP debugging port exists.
- The Go proxy serves `/json/version` and a WebSocket on loopback, both gated by an `X-Berth-Token` header (32 random bytes, SHA-256 hashed, constant time compare).
- Playwright MCP 0.0.85 with `--cdp-endpoint http://127.0.0.1:<port>` and `--cdp-header "X-Berth-Token: <token>"` navigated to a test page, took a snapshot and clicked a button; the page changed. The proxy logged 74 CDP methods by name.
- No token and a wrong token: HTTP 401. A refused method (`Storage.getCookies`) got a CDP error from the proxy; an allowed one (`Browser.getVersion`) passed.
- Stopping the proxy killed the whole Chrome process group: 7 processes before, 0 after.

Findings for the real build:

- Passing the token as a command line flag exposes it to `ps`. The real `berth-mcp` shim or launcher must hand it over through an environment variable or a file only the agent can read.
- Playwright MCP's `browser_click` now takes `target` instead of `ref`; anything Berth ships that drives MCP tools should read the tool schema, not hard code argument names.
- Berth's own calls use a separate id range (from 1,000,000,000) on the same pipe, which proof 3 relies on.

## 3. Chrome screencast frames in SwiftUI

- The Go server launches headless Chrome over the pipe, opens an animated canvas page at 1440x900, and runs `Page.startScreencast` (JPEG, quality 70) on its own CDP session and id range.
- Frames go to clients over `WatchScreen`, a server stream on the same Unix socket API. Each watcher has a one frame buffer; a new frame replaces an unsent one, so a slow client drops frames instead of lagging.
- A SwiftUI window (`NSHostingView`, `Image(nsImage:)`) showed **601 frames in 9.9 s = 60.6 fps** at 1440x900, with send to display latency of 0.3 ms (p50) and 0.5 ms (p95).

Findings for the real build:

- Chrome sends the next frame only after `Page.screencastFrameAck`; ack before decoding.
- Headless Chrome only sends frames when the page repaints, so a still page sends almost none. Live view must keep the last frame and not treat silence as a failure.
- The daemon must handle SIGTERM (launchd sends it) and stop every Chrome process group it owns. Without a handler, a killed server left Chrome running; with `signal.NotifyContext` the group is gone after SIGTERM.
- Not measured yet: CPU cost of 60 fps decode in the app; the real app should let the client choose a frame rate.
