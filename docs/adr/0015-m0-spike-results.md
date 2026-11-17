# 0015: M0 spike results

Status: accepted, 2026-11-16

## Context

M0 had to prove five risky parts before real code: Swift streaming over the Unix socket, Playwright MCP through a token proxy, screencast in SwiftUI, Touch ID with a Keychain read from a signed helper, and window capture by process ID.

## Decision

All five passed, so no fallback is needed and the architecture stands as designed. The spike code is kept in git under the tag `m0-spike` and removed from the tree; full numbers are in `spike/RESULTS.md` at that tag.

## Why and what changes

- **API (ADR 0002 confirmed):** Connect v2 in Go with `connecthttp.WithGRPC()`; Swift via `grpc-swift-2` and the NIO transport over the socket. Use the BSR plugin `buf.build/grpc/swift-protobuf`, and `<Method>Request` / `<Method>Response` names.
- **Browsers:** Chrome over `--remote-debugging-pipe` in its own process group; the proxy gates by header token; Playwright MCP works with `--cdp-endpoint` and `--cdp-header`. Tokens must not travel as command line flags. Berth's own CDP calls use a separate id range on the same pipe.
- **Lifecycle:** `berthd` handles SIGTERM and stops every Chrome process group it owns.
- **Live view:** 60 fps at 1440x900 with drop stale buffering; ack screencast frames first; a still page sends no frames.
- **Native:** free Apple ID signing covers Touch ID and the login keychain; items Berth creates need no extra Keychain dialog; the data protection keychain waits for the paid program.
- **Screen Recording:** belongs to `Berth.app` (`com.wesleybldc.berth`) only; the process connects to the window server before capturing.
