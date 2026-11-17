# M0: Spike

Goal: prove the five parts that could force a redesign, before any real code exists.
Everything lives in `spike/` and is deleted when M0 closes; only the findings survive, as decision records.

## Proofs

| # | Proof | Pass when | Fallback if it fails |
|---|---|---|---|
| 1 | Swift client streams from a Connect server in Go over a Unix socket (`grpc-swift` 2, NIO transport) | A server stream of 1,000 messages and a client stream of 1,000 input events both complete over `~/Library/Application Support/Berth/spike.sock` | Loopback TCP with a token file readable only by the user |
| 2 | Playwright MCP drives a Berth launched Chrome through a token proxy | Chrome launched with `--remote-debugging-pipe`; a Go proxy exposes a loopback WebSocket that requires a header; Playwright MCP connects with `--cdp-endpoint` and `--cdp-header` and completes navigate, click and snapshot; a wrong token is refused | Loopback debugging port with a random path, guarded by the proxy |
| 3 | Chrome screencast frames shown smoothly in SwiftUI | `Page.startScreencast` JPEG frames at 1440 wide reach a SwiftUI view at 15 frames per second or more, with stale frames dropped | Lower resolution or ScreenCaptureKit for managed windows |
| 4 | Touch ID and a Keychain read from a signed command line helper, signed with a free Apple ID | The helper shows a Touch ID prompt with custom text, then reads a test generic password and writes it to its stdout pipe | Prompt from the Mac app instead of the helper |
| 5 | ScreenCaptureKit finds and captures a window by process ID | A window of a given Chrome or Safari process is captured to an image | Window list by title |

## Rules

- Throwaway code, no tests required, but every proof has a script or `make` target that reruns it.
- No real secrets: proof 4 uses a test item named `berth-spike-test`, deleted afterwards.
- Record each result in `docs/adr/` (accepted or the fallback) before closing M0.

## Setup needed

See `docs/setup.md`: Go 1.27, Swift 6.3, Xcode selected through `DEVELOPER_DIR`, `buf`, `golangci-lint`, `prek`, Node for Playwright MCP.

## Exit gate

All five proofs pass or have a recorded fallback, and the milestone status (now in `ARCHITECTURE.md`) is updated with the results.
