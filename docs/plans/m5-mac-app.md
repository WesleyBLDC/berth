# M5: Mac app

Status: In progress. Items 1 to 8 are built; items 9 to 15 (screencast, takeover, live view, window pictures, headful agent browsers) are planned and are best built once the [test lab](test-lab.md) can run headful browsers and the app unattended (lab items 7 and 9).

Goal: `Berth.app`, a SwiftUI menu bar item and one window, shows every browser session live from `WatchSessions`, registers `berthd` as a login item, tells the person when Berth needs them, shows a live view of a managed browser and lets the person take it over while the agent waits, and shows a picture of the window of a browser Berth does not manage.
Agent browsers may then run headful, since the person can now see and take over what the agent drives.
The maintainer was away when this plan was written and asked not to be blocked; every choice that is his to make is listed under [Decisions for the maintainer](#decisions-for-the-maintainer), with the default taken, and everything that needs a GUI, a permission or a signed build is under [Manual checks](#manual-checks), which the test lab turns into VM tests.

## What earlier milestones left to build on

- **M0 proofs 1, 3 and 5** ([ADR 0015](../adr/0015-m0-spike-results.md), code under the tag `m0-spike`): Swift streamed from the Go Connect server over the Unix socket; a SwiftUI window showed Chrome screencast frames at 60 fps with a one frame buffer per watcher, acking each frame before decoding it; ScreenCaptureKit captured a Chrome window by process ID from a signed bundle, and Screen Recording belongs to `Berth.app` (`com.wesleybldc.berth`) only.
- **`berth-native`** already speaks Connect JSON over HTTP/1.1 on a raw Unix socket and checks by the peer's audit token that the signed `berthd` answers before it writes a byte (`native/Sources/BerthNativeCore/Socket.swift`, `Redeem.swift`). That code passed the maintainer's manual check in E2a.
- **M3** gives `WatchSessions`: a snapshot or one change per message, each with the `machine_event` id it is consistent with, resumable after that id across a `berthd` restart; `berth top` folds it in with `apply` and prints every process chosen string through `shown` (`strconv.QuoteToGraphic` when a string holds a control or format character).
- **M4** gives managed browsers over a private CDP pipe (`internal/adapters/cdppipe`), Berth's own CDP sessions on its own id range that the agent's proxy connection never sees, and the M5 hooks: `Page.startScreencast` on a Berth session, takeover through `Input.dispatch*` on Berth's session while the proxy holds the agent's messages, a `Control` machine separate from the browser machine. Agent browsers are headless only until M5.
- **Signing** (`scripts/sign.sh`): the Personal Team `XPJ7XA42J4`, hardened runtime, one stable identifier per binary; `berthd` trusts `berth`, `berth-native` and `berth-proxy` found beside its own executable (`paths.Beside`) by team, identifier and cdhash.
- macOS 26 and later only ([ADR 0010](../adr/0010-macos-26-and-later-only.md)), so `MenuBarExtra`, `Window`, `@Observable`, `SMAppService` and ScreenCaptureKit's screenshot API need no availability checks.

## Design

```mermaid
%%{init: {"theme":"dark","themeVariables":{"primaryTextColor":"#F4F7FA","lineColor":"#C9D2DC","edgeLabelBackground":"#16263F"},"flowchart":{"nodeSpacing":60,"rankSpacing":70,"htmlLabels":false}}}%%
flowchart LR
  UI["Berth.app<br/>menu bar and window"]
  CORE["BerthAppCore<br/>client, models, store"]
  D["berthd<br/>LaunchAgent"]
  CH["managed Chromium<br/>CDP pipe"]
  SCK["ScreenCaptureKit<br/>other browsers"]
  UI --> CORE
  CORE -->|Connect JSON,<br/>HTTP/1.1, Unix socket| D
  D -->|screencast and input<br/>on Berth's session| CH
  UI -->|window pictures| SCK
```

### App shape

- **A separate Swift package, `macos/`**, beside `native/`, built with SwiftPM and no Xcode project. Targets: `BerthAppCore` (a library: the `berthd` client, the JSON models, the session store, the watch loop, `shown`), `BerthApp` (the executable `Berth`: SwiftUI scenes only), `berth-app-probe` (a command line tool the end to end tests run, never bundled) and `BerthAppCoreTests` (Swift Testing).
- **Shared socket code**: `native/` gains a library target `BerthSocket` holding `UnixSocket`, `checkPeer`, `berthDaemonRequirement` and `socketPath`, moved out of `BerthNativeCore`; both `berth-native` and the app import it. Nothing else of `native/` reaches the app.
- **No secrets in the app.** `Berth.app` never asks for, holds or shows a secret value, and links no Keychain code: the bundle check fails if the `Berth` executable imports any `SecItem`, `SecKeychain` or `SecAccess` symbol. The only Security framework calls are the code signing checks (`SecCodeCopyGuestWithAttributes`, `SecCodeCheckValidity`).
- **SwiftUI**: a `MenuBarExtra` whose label is the brand's template image and the number of live sessions, with a menu that names the counts (active, idle, leftovers), the connection state, "Open Berth" and "Quit"; and one `Window` with the session list. The app is an accessory (`LSUIElement`), so it has no Dock icon; opening the window activates it.
- **Bundle layout**, assembled and signed by `scripts/build-app.sh`:

| Path in `Berth.app/Contents` | Holds | Signed as |
|---|---|---|
| `MacOS/Berth` | The app | `com.wesleybldc.berth` (the bundle) |
| `Helpers/berthd`, `Helpers/berth`, `Helpers/berth-native`, `Helpers/berth-proxy` | The daemon, the CLI, the helper and the proxy, beside each other so `paths.Beside` and every pin work unchanged; not in `MacOS`, where `berth` and `Berth` would be one file on a case insensitive volume | Their own identifiers, signed before the bundle |
| `Library/LaunchAgents/com.wesleybldc.berth.daemon.plist` | The login item `SMAppService.agent` registers: `BundleProgram` `Contents/Helpers/berthd`, `RunAtLoad`, `KeepAlive` on unsuccessful exit | Sealed by the bundle signature |
| `Resources/AppIcon.icns` | `brand/app-icon/Berth.icns` | Sealed |
| `Info.plist` | Bundle id `com.wesleybldc.berth`, `LSMinimumSystemVersion` 26.0, `LSUIElement` | Sealed |

The bundle is signed with the hardened runtime and no entitlements, and is not sandboxed: the App Sandbox would block the socket under `~/Library/Application Support/Berth`.
The person's `berth` on `PATH` becomes a symlink to `Contents/Helpers/berth` once they use the app build, so the CLI that `berthd` pins is the one they run.

### How the app talks to berthd

**Choice: a small Swift Connect client speaking JSON over HTTP/1.1 on the Unix socket**, the protocol `berth-native` already speaks, recorded in [ADR 0019](../adr/0019-berth-app-speaks-connect-json-over-the-socket.md).

- **Why not grpc-swift-2**, which the spike used: its NIO transport owns the socket, so the app cannot ask the kernel who answers (`LOCAL_PEERTOKEN`) before it sends a byte; it brings about twenty packages (`swift-nio`, `swift-nio-ssl`, `swift-crypto`, `swift-certificates` and more) into a signed app that must stay small and auditable; and it needs a Swift code generation step in `make gen`. The cost of the choice is hand written models for the few messages the app uses, kept true by fixtures a Go test writes from the real schema (below).
- **Why not JSON through the CLI**: `berth` prints text for people, a process per update cannot carry a stream, and resume would mean parsing cursors out of text.
- **Peer check, app side**: every call opens its own connection, checks by the audit token that the process answering is the signed `berthd` (`berthDaemonRequirement`, Apple's chain and Berth's team), and only then writes the request. The app binary has no test rule; `berth-app-probe` takes the requirement as an argument so the end to end tests can point it at the ad hoc signed `berthd`.
- **Unary calls**: `POST /<service>/<method>`, `Content-Type: application/json`, `Connect-Protocol-Version: 1`, `Connection: close`; a non 200 answer carries a Connect error (`code`, `message`, and the typed `ErrorReason` in `details` when present).
- **Server streams**: `Content-Type: application/connect+json`, the request in one envelope (one flag byte, a four byte big endian length, the JSON); the answer is chunked, one envelope per message, ending with an end of stream envelope (flag `0x02`) whose JSON holds an `error` or nothing. Envelopes over 16 MiB and heads over 64 KiB end the stream as malformed.
- **Proto JSON**: field names in lowerCamelCase, enums by name (an unknown name decodes as unknown, never a crash), 64 bit integers as strings, timestamps as RFC 3339 with 0 to 9 fractional digits, default values omitted.
- **Client streaming is not possible over HTTP/1.1** in Connect, so input for takeover is a unary `SendInput` carrying a batch of events (item 9).
- **berthd side, a pin for the app** (item 7): `berthd` pins `../MacOS/Berth` from its own folder (`Contents/Helpers`) with identifier `com.wesleybldc.berth`: since an executable sealed by its bundle cannot be checked alone, berthd copies the whole bundle and pins each architecture slice of the copy's executable whose cdhash, in one codesign call, satisfies the requirement on the bundle (see the second review fixes). The listing and watching methods stay open to any owner socket client, as today; the pin gates only the live view and takeover methods, which show page content and send input. It was to land with the first method that checks it; the second pass built it ahead of that method, with no method marked and the file pinned only on first use (decision 13).

### The session list and the menu bar (items 3 and 4)

- `SessionStore` folds `WatchSessions` in exactly like `berth top`'s `apply`: a snapshot replaces everything; a change replaces one session, keeping the facts it already had when the change carries only `id`, `state` and `state_since`; at most 200 stopped sessions are kept, the oldest dropped first. It records the cursor of the last message it applied.
- `SessionsModel` (`@Observable`, main actor) runs the watch loop: connect, `Handshake` with protocol version 1, `WatchSessions(after_event_id: cursor)`, apply each message; when the stream ends or fails it shows "reconnecting" with the reason and resumes after the cursor, waiting 1 second, doubling to at most 10.
- **Every process chosen string** (owner name, profile folder, executable, event name, error text) goes through `shown`, the Swift twin of the CLI's: unchanged when every character is graphic, else quoted with Go's escapes. A Go test writes the expected outputs of the CLI's `shown` for an injection corpus into a fixture the Swift tests read, so the two cannot drift.
- The window lists live sessions (and stopped ones behind a toggle) with state, browser, driver, headless, owner, profile folder, age and id, newest first, and a status line for the connection. Stop, inspect and filter come after the live view.
- The menu bar label shows the number of live sessions; the menu adds the counts by state and marks leftovers.

### Login item (item 6)

- `SMAppService.agent(plistName: "com.wesleybldc.berth.daemon.plist")` registers `berthd` from inside the bundle; the window's settings section shows its status (`enabled`, `requiresApproval`, `notRegistered`, `notFound`) and offers Register, Unregister and "Open Login Items" (`SMAppService.openSystemSettingsLoginItems()`).
- The app never registers on its own: the person turns it on once, and macOS asks them to allow it in System Settings, General, Login Items.
- A `berthd` already serving (started by hand from `bin/`) holds the socket; the launchd one then exits 1 with "another berthd serves this socket", and `KeepAlive` on unsuccessful exit retries with launchd's throttle. The settings section says which `berthd` answers (its path from `Status`).
- Tests: the plist's keys are checked by the bundle check; registering needs the person, so it is a manual check.

### Notifications (item 8)

- "Needs you" events: a store or rotate dialog waiting (secret `Pending`, `Rotating`, `Removing`), a grant waiting for Touch ID (grant `Requested`), and a new leftover browser (session `Leftover`).
- A new server stream, `WatchNeeds`, resumable on the same `machine_event` cursor and built like `WatchSessions`, sends only facts `berthd` verified: the entity id, the kind, the secret name (already validated by its name rule) or the session's owner name and browser kind. It is not on the agent surface.
- The app posts one `UNUserNotificationCenter` notification per need, with the agent controlled text quoted and labelled (checklist item 5); clicking it opens the window. It asks for notification permission the first time the person turns notifications on, never at launch.
- Tests: the stream in Go (`internal/berthd`, end to end with a fake helper's open dialog and a real leftover), the notification text builder in Swift; posting is a manual check.

### Live view and takeover for managed browsers (items 9 to 12)

```mermaid
%%{init: {"theme":"dark","themeVariables":{"primaryTextColor":"#F4F7FA","lineColor":"#C9D2DC","edgeLabelBackground":"#16263F"}}}%%
stateDiagram-v2
  [*] --> Agent
  Agent --> Person: Take / HoldAgent
  Person --> Agent: Release / ReplayHeld
  Person --> Agent: ViewerGone / ReplayHeld
  Person --> Agent: HeldOverflow / DropAgent
  Agent --> Ended: BrowserClosed
  Person --> Ended: BrowserClosed / DropHeld
```

- **Screencast**: `WatchScreen(browser_id, max_fps)` is a server stream. `berthd` attaches to the browser's page target on its own pipe session (`Target.attachToTarget` with `flatten`), sends `Page.startScreencast` (JPEG, quality 70, the window's size), acks each frame before anything else, and keeps one frame per watcher: a new frame replaces an unsent one, so a slow app drops frames instead of lagging. A still page sends no frames, so the app keeps the last one. The agent's proxy connection never sees Berth's session (M4's session filter).
- **Control machine** (`internal/core/control`, entity `control/<browser id>`, 6 legal pairs): `Take` holds the agent: the CDP proxy stops forwarding the agent's messages and queues them, bounded at 1,000 messages or 16 MiB; the agent sees only a slow browser, never a takeover event ([ADR 0008](../adr/0008-atomic-fill-with-the-agent-paused.md)'s pause, which M6's fill reuses). `Release`, or the person's screen stream ending (`ViewerGone`), replays the queue in order. A queue that overflows closes the agent's proxy connection (`HeldOverflow`), which fails closed: the agent reconnects with its token and finds the person in control until release.
- **Input**: `TakeControl(browser_id, request_id)` answers a control id; `SendInput(control_id, events)` is unary and carries a batch of mouse, wheel and key events, which `berthd` sends as `Input.dispatchMouseEvent` and `Input.dispatchKeyEvent` on Berth's session; `ReleaseControl(control_id, request_id)` ends it. Only the signed `Berth.app` may call these three and `WatchScreen` (the app pin).
- **Hard requirement, the app is no deputy:** the app calls `WatchScreen`, `TakeControl`, `SendInput` and `ReleaseControl` only from a direct action of the person in its own UI; no command line argument, URL scheme, AppleEvent, notification action or other input from another process may start one. Item 10 adds a test or review step that checks it, and `ARCHITECTURE.md` records it as an invariant of the app.
- **No machine for screen streams.** The planned "Screen stream" machine becomes a watcher like `WatchSessions`'s: a frame buffer of one, no stored state, so nothing to recover at boot.
- Tests: the control table walked and pinned; the queue's bound; the Go end to end with a real headless shell: frames arrive (JPEG magic bytes, the page's size), an agent's `Runtime.evaluate` sent during a takeover gets no answer until release and then the right one, the person's typed text reads back from an `<input>`, and the agent's connection sees no event of Berth's session.

### Window pictures for browsers Berth does not manage (item 13)

- For any other session (the person's Safari or Chrome, a Cowork browser) the app finds the largest on screen window whose owning application's pid is the session's browser pid (`SCShareableContent`), and captures it with `SCScreenshotManager`, sized from the content rectangle and scale, refreshed every 2 seconds while the detail is open.
- Needs Screen Recording for `Berth.app`; without it the detail says how to turn it on and the app never asks on its own. Capture runs in the app, never in `berthd`, since the grant belongs to the bundle.
- Tests: the window choice is a pure function over `(pid, frame, on screen, layer)` tested in Swift; capture is a manual check.

### Headful agent browsers (item 14)

- `OpenAgentBrowser` gains `headless`, default true; headful is allowed once takeover is built, so the person can watch an agent's browser in the app and take it over through Berth.
- Honest limit: input the person gives directly to a headful window reaches Chrome without pausing the agent; the app's takeover is the supported way to act on an agent's page.

## Contract (proto additions, items 7 to 12)

```proto
service ScreenService {
  rpc WatchScreen(WatchScreenRequest) returns (stream WatchScreenResponse);
  rpc TakeControl(TakeControlRequest) returns (TakeControlResponse);
  rpc SendInput(SendInputRequest) returns (SendInputResponse);
  rpc ReleaseControl(ReleaseControlRequest) returns (ReleaseControlResponse);
}
message WatchScreenRequest { string browser_id; uint32 max_fps; }
message WatchScreenResponse { bytes jpeg; uint32 width; uint32 height; uint64 seq; google.protobuf.Timestamp at; }
message TakeControlRequest { string request_id; string browser_id; }
message TakeControlResponse { string control_id; }
message InputEvent { oneof event { MouseEvent mouse; KeyEvent key; WheelEvent wheel; } }
message SendInputRequest { string control_id; repeated InputEvent events; }
message SendInputResponse {}
message ReleaseControlRequest { string request_id; string control_id; }
message ReleaseControlResponse {}
// NeedsService, item 8
rpc WatchNeeds(WatchNeedsRequest) returns (stream WatchNeedsResponse);
// ErrorReason, appended after the last value on main
ERROR_REASON_NOT_BERTH_APP;
ERROR_REASON_CONTROL_HELD;
```

- Nothing is on the agent surface; `TestAgentSurface` is unchanged.
- `SendInput` takes no `request_id`: an input batch replayed by a retry would type twice, so the client never retries it, and the control id scopes it.
- Validation: `browser_id` 16 lowercase hex, `max_fps` 1 to 60, at most 64 events per batch, key text at most 16 runes.

## Work items

Each part ends with `make check` passing and its own commit.
Items 2 to 5 were built in the first pass and items 6 to 8 in the second (see the exit notes); the rest follow.

| # | Item | Depends on | Owns | Tests |
|---|---|---|---|---|
| 1 | Plan | | this file, ADR 0019 | |
| 2 | Shared socket library and app core | 1 | `native/Package.swift`, `native/Sources/BerthSocket/`, `macos/Sources/BerthAppCore/`, `macos/Sources/berth-app-probe/`, `internal/cli/swiftfixture_test.go`, `test/e2e/app_test.go` | Below |
| 3 | App shell and bundle | 2 | `macos/Sources/BerthApp/`, `macos/Bundle/`, `scripts/build-app.sh`, `scripts/check-app-bundle.sh` | The bundle check, ad hoc signed: layout, plists, every signature and identifier, no Keychain symbol in `Berth` |
| 4 | Make targets | 3 | `Makefile` | `make check` builds and tests the app |
| 5 | Docs | 4 | `ARCHITECTURE.md` | |
| 6 | Login item | 3 | `macos/Sources/BerthApp/LoginItem.swift` | The plist keys; registering is manual |
| 7 | App pin in `berthd` | 2 | `internal/codetrust`, `internal/berthd/berthd.go`, `internal/peertrust` | A release pin needs the app's requirement; a peer that is not the app gets `NOT_BERTH_APP` |
| 8 | Notifications | 2 | `WatchNeeds` contract and shell, `macos/.../Needs*.swift` | Go stream tests, Swift text builder |
| 9 | Screen and control contract | 1 | `proto/berth/v1/screen.proto`, `gen/`, `internal/core/control/` | `buf breaking`; control walk pinned at 6 |
| 10 | Screencast in `berthd` | 9 | `internal/berthd/screen.go`, `internal/adapters/cdppipe` | Go end to end: frames arrive from a real headless shell |
| 11 | Takeover in `berthd` | 9, 10 | `internal/adapters/cdpproxy` (hold and replay), `internal/berthd/screen.go` | Go end to end: the agent waits, then gets its answer; typed text reads back |
| 12 | Live view in the app | 10, 11 | `macos/.../LiveView*.swift` | Frame decoding and input mapping in Swift; the view is manual |
| 13 | Window pictures | 3 | `macos/.../WindowPicture*.swift` | Window choice in Swift; capture is manual |
| 14 | Headful agent browsers | 11 | `OpenAgentBrowser.headless` | Go: a headful launch request is honoured only for an agent session |
| 15 | Close | all | `ARCHITECTURE.md`, this file | Agent safety checklist |

**Item 2 tests:**

- `BerthSocket`: the moved tests still pass in `native/`.
- Client (Swift, against a scripted in memory connection): a unary call's exact request bytes; a 200 answer decoded; a Connect error with a typed reason; a stream of envelopes split across reads and chunks, ending cleanly; an end of stream with an error; a malformed chunk, an oversized envelope and an early close each end the stream with an error; and the real dialer, against a listening socket this unsigned test process owns, refuses it and writes nothing.
- Models: the fixture a Go test writes with `protojson` from a `WatchSessionsResponse` that sets every `Session` field decodes into the Swift models with every field read back; unknown enum names decode as unknown; 64 bit ids as strings.
- `shown`: every fixture case equals the Go output.
- Store: snapshot replaces, change upserts, a state only change keeps the facts, stopped trimmed at 200, counts by state, cursor after each message.
- Watch loop: after an ended stream it resumes with the last cursor, after an error it shows reconnecting with the escaped reason and backs off 1, 2, 4, 8, 10, 10 seconds on a fake sleeper, and a message resets the backoff.
- **End to end** (`test/e2e/app_test.go`): the real `berthd` (ad hoc signed, `berth_e2e`) and a real headless Chromium with M2's skip rules; `berth-app-probe`, the app's core with the e2e requirement, prints a `START` snapshot, then the browser's `Seen` and its `Stopped` after the test kills it; run again after the last cursor it prints the same changes and no snapshot; run with the release requirement it refuses the server before writing a byte and exits 1.

## Exit gate

`make check` passes with the app built and its tests run; the end to end probe test runs (or skips with the reason when no Chromium is found); every manual check below passes on the maintainer's Mac; the agent safety checklist is walked for M5; `ARCHITECTURE.md` marks M5 done.

## Manual checks

These need a signed build, a permission grant or a GUI. The [test lab](test-lab.md) runs checks 2 to 8 unattended in a VM with a lab certificate, pre granted permissions and Accessibility (lab item 9); check 1, with the real team key, stays on the release checklist.

1. `make app` builds and signs `bin/Berth.app`; `codesign --verify --deep --strict bin/Berth.app` passes and `codesign -dv` shows team `XPJ7XA42J4` and `com.wesleybldc.berth`.
2. With `berthd` running (`bin/berthd` or the bundle's), open `bin/Berth.app`: the menu bar shows the template icon and the number of live sessions; "Open Berth" shows the window with the same sessions as `berth ps`.
3. Start a Playwright browser: it appears in the window and the count grows; close it: it shows stopped and the count drops. Quit `berthd`: the status says reconnecting; start it again: the list catches up with no duplicate rows.
4. Login item (item 6): in the window's Settings tab, press Register under "Start berthd at login", allow Berth in System Settings, General, Login Items (Open Login Items goes there); log out and in; `launchctl print gui/$(id -u)/com.wesleybldc.berth.daemon` shows it running from inside the bundle, `berth status` answers, and the tab's "Answering" line says the bundle's `berthd`. With a `bin/berthd` already running, the line says a `berthd` started by hand instead.
5. Notifications (item 8): press Turn On under Notifications in Settings and allow notifications when macOS asks (it must not ask at launch); a store dialog requested by an agent and a leftover browser each post one notification, with the secret's or owner's name quoted and labelled; answering the dialog or stopping the browser removes it; clicking one opens the window; quitting and reopening the app neither asks again nor posts the same open need twice in one run.
6. Live view and takeover (items 10 to 12): launch a managed browser, drive it with Playwright MCP; the app shows it live; take over, type into the page; the agent's next step waits; release, and the agent continues.
7. Window pictures (item 13): grant Screen Recording to Berth in System Settings, Privacy and Security; a Safari session's detail shows its window.
8. A headful agent browser (item 14) opens on screen and can be taken over from the app.

## Honest limits

- **The app trusts only the release `berthd`.** A `berthd` built by `go build` or the end to end tests is refused by the app; only `make build` or the bundle's `berthd` is accepted.
- **Hand written models.** The app decodes only the fields it uses; a new field is ignored until the app learns it. The fixture test catches renames and removals, not additions the app should show.
- **One connection per call.** Every unary call and every stream pays a connect and a code signing check (well under a millisecond each locally); fine for a list and for input batches, measured again in item 12.
- **Accessory app.** No Dock icon, so the window is reached only from the menu bar (or a notification).
- **A needs snapshot is one message with every open need**, with no cap of its own beyond the 16 MiB envelope the app accepts; a Mac with thousands of leftover browsers would hit it, which no one has.
- **No version floor.** The app does not require a minimum `berthd`; against one without `WatchNeeds` (or a field it reads) the needs watch shows reconnecting with `unimplemented` and backs off, and the session list keeps working.
- **Each app pin copies the whole bundle**, helpers included (tens of megabytes), on the first marked call and on a later mismatch at most once per 10 seconds from the end of the last pin; the copy stops past 512 MiB, 10,000 files or 32 levels of folders.
- **A downgrade is not refused.** With no version floor, an older signed `berthd` or an older signed `Berth.app` from Berth's team passes every pin; nothing compares versions.
- **`check-app-bundle.sh` and the pin copy both refuse symlinks** in the bundle, so a bundle layout that needs one (a framework's `Versions/Current`) must change both first.
- **Unicode tables can drift** between the Swift `shown` and `quoted` (ICU) and Go's (its own tables), as noted in the review of items 1 to 5; the fixture uses only long assigned code points.
- **Follow up before item 9 marks any method:** consider a second lock beside the pin, a live check of the peer's audit token against the app's code requirement (as `BerthSocket` checks `berthd`), so trust does not rest on a pinned cdhash alone.
- **Anything on the owner socket can still watch** sessions and needs, as with `berth top`; only screen and input will need the app's signature.
- **A needs snapshot reads every secret and grant row**, not only the open ones, since nothing indexes rows by state; both grow only with what the person stores and grants, and nothing prunes them yet.
- **A leftover's owner is usually unknown** to a need: the owner exited, which is what made the browser a leftover, so the notification names the browser's kind instead.
- **Notifications are remembered per app run only**: a relaunched app posts the needs still open once more.
- **Direct input to a headful agent window does not pause the agent** (item 14).

## Decisions for the maintainer

Defaults taken while he was away; each is cheap to reverse before M5 closes.

1. **`macos/` as its own SwiftPM package**, no Xcode project; `scripts/build-app.sh` assembles the bundle. An Xcode project would add a second build system for one target.
2. **Connect JSON over HTTP/1.1 on the raw socket**, not grpc-swift-2 ([ADR 0019](../adr/0019-berth-app-speaks-connect-json-over-the-socket.md), superseding that line of ADR 0015), with the socket code shared with `berth-native` through a new `BerthSocket` target.
3. **The app checks `berthd` before writing a byte**, release requirement only; the probe takes the requirement as an argument for the end to end tests.
4. **All four binaries inside the bundle, beside each other in `Contents/Helpers`**, so `paths.Beside` and every pin stay as they are and `berth` never collides with `Berth` on a case insensitive volume; the person's `berth` becomes a symlink into the bundle.
5. **An accessory app** (`LSUIElement`), no Dock icon.
6. **No App Sandbox, no entitlements**, hardened runtime.
7. **The `berthd` pin for the app lands with the first method that checks it** (item 7), the screen and control methods; listing and watching stay open to the owner socket. Superseded by decision 13: the pin shipped before any method uses it.
8. **Input as unary batches** (`SendInput`), since Connect has no client streaming over HTTP/1.1; it takes no `request_id`, so a retry never types twice.
9. **No screen stream machine**; a `Control` machine with `Agent`, `Person` and `Ended`, 6 legal pairs, and the held queue bounded at 1,000 messages or 16 MiB, overflow closing the agent's connection.
10. **The app never registers the login item or asks for notifications or Screen Recording on its own**; each starts from a button.
11. **Reconnect backoff** 1 second doubling to 10; **200 stopped sessions** kept, like `berth top`.
12. **Shared fixtures**: a Go test in `internal/cli` writes the session and `shown` fixtures the Swift tests read, so the hand written models and the escaping cannot drift silently from the schema and the CLI.
13. **The app pin shipped before any method uses it** (items 6 to 8 pass): decision 7 said it lands with the first method that checks it, but the pass was asked to build item 7 and to keep listing and watching (`WatchNeeds` included) open, so the mechanism (the `(berth.v1.app)` method option, `api.AppOnly` on both sockets, `ERROR_REASON_NOT_BERTH_APP` = 25, the deferred `codetrust.File` for `../MacOS/Berth`) is built and tested and no method is marked; `TestAppSurface` pins the empty list until the screen methods mark themselves. The deferred pin reads no file until a marked method is called, so nothing runs for it today. Reverting is deleting the option, the interceptor and the reason before any client depends on them.
14. **`WatchNeeds` stays open to the owner socket**, like `WatchSessions`: it shows only secret names (which `ListSecrets` already shows) and leftover browsers (which `ListSessions` shows). Gating it behind the app pin would also keep `berth` from ever reading it.
15. **A leftover need carries the browser's kind**, a field the plan did not name (`Need.browser`): a browser becomes a leftover when its owner exits, so `owner_name` is almost always empty and the notification would say nothing about which browser.
16. **`StatusResponse.executable`** is new, so the login item section can say which `berthd` answers, as the plan's design asked; it is the resolved path of the running `berthd`, served only on the owner socket (`Status` is not an agent method).
17. **The machine kit gained a `Saved` hook**, told as soon as a transition is saved and before its effects run: a store dialog runs as the secret actor's effect, so the existing after effects hook would have told `WatchNeeds` of the dialog only after the person answered it.
18. **Notifications are posted once per open need per app run**, in id order, including the needs of the first snapshot (a dialog already open, or leftovers the Mac already had, when the app starts); a need whose snapshot or change repeats is not posted again, and a resolved need's notification is removed. Turning notifications off removes every posted one and is remembered (`notificationsTurnedOff` in the app's defaults).

## Exit notes (2026-11-17, items 1 to 5)

Built on branch `m5`, rebased onto `main` at 4884507 (M4 merged): the plan with ADR 0019, the app core, the app shell and bundle, the make targets, then `ARCHITECTURE.md`, each with its own commit, with `make check` passing.

- **No proto change** in this pass; `buf breaking` is clean against `main`, and no `ErrorReason` was added.
- **`BerthSocket`** took `Socket.swift` (less the value channel check, now `ValueChannel.swift` in `BerthNativeCore`) and `Paths.swift` from `native/`; `UnixSocket` gained `readSome` and `shutdownBoth` for streams.
- **The client** passes against the real `berthd`: `berth-app-probe` streams a `START` snapshot, the browser's `Seen` and `Exited` changes, resumes after the cursor with the same changes, and refuses the ad hoc signed `berthd` under the release requirement (`test/e2e/app_test.go`).
- **The bundle** moved the four Go and Swift helpers from `Contents/MacOS` to `Contents/Helpers` after the first build: `berth` and `Berth` are one file on the default case insensitive volume.
- **Not run:** the signed `make app` (it uses the team identity from the login keychain, which this pass did not touch) and the GUI; both are manual checks 1 to 3.
- **Outside the plan:** `TestASlowAgentIsDroppedPastTheByteLimit` in `internal/adapters/cdpproxy` (from M4) failed about once in 30 runs, because the test emitted before the proxy set its sink and kept emitting after the proxy dropped it; the test now waits for the sink and stops at the drop.

### Later, from the review (2026-11-17)

- **Quadratic `removeFirst`.** `HTTPReader` and `EnvelopeReader` drop consumed bytes with `Array.removeFirst`, which copies the rest each time; a large message arriving in small reads costs time quadratic in its size. A read index that compacts now and then fixes it.
- **The head cap is approximate.** `HTTPReader.head` counts line lengths without their line endings, and a single line may hold up to 64 KiB of buffered bytes before the cap trips, so a head can pass somewhat over 64 KiB.
- **No total deadline on unary reads.** A unary call bounds each read at 10 seconds, but a server that sends one byte every few seconds can hold a call open for as long as it likes; a deadline over the whole call fixes it.
- Added after the review: a fixture test that the Swift `berthDaemonRequirement` equals Go's release requirement for berthd byte for byte, and bundle checks that no binary carries entitlements and that `berth-app-probe` is not in the bundle.

### Agent safety checklist (M5, items 1 to 5)

1. No agent method was added; the app uses `Handshake` and `WatchSessions` on the owner socket only.
2. The app holds no value: it links no Keychain call (the bundle check reads `Berth`'s imported symbols), and no fixture holds a secret.
3. No new path carries a value, so no new canary; the app's only output is the session facts `WatchSessions` already serves.
4. No power action was added.
5. Every process chosen string the app shows goes through `shown`, checked against the CLI's own output for an injection corpus; error text is escaped too.
6. No mutating method was added; the app makes no mutating call yet.
7. Every client failure is a typed `ClientError` shown as "reconnecting" with its reason, and the loop backs off to 10 seconds; nothing retries silently.
8. OWASP agentic risks: unchanged, except that the app refuses a `berthd` that is not the signed one (a fake daemon an agent started cannot feed the person's view or, later, receive their keystrokes).

## Exit notes (2026-11-17, items 6 to 8)

Built on branch `m5b` from local `main` at 8bebbe1: the login item, the app pin, then `WatchNeeds` with the app's notifications, each with its own commit and `make check` passing, then this file and `ARCHITECTURE.md`.

- **Proto changes, additive only, `buf breaking` clean against `main`:** `StatusResponse.executable` (7); the method option `app` (50002) beside `agent`; `ERROR_REASON_NOT_BERTH_APP` (25, after `ENTITY_NOT_FOUND`); `proto/berth/v1/needs.proto` with `NeedsService.WatchNeeds`, `Need` (id, kind, secret name, owner name, browser kind), `NeedKind`, `NeedEvent`, `NeedsSnapshot`, `NeedChange`. `ERROR_REASON_CONTROL_HELD` waits for item 11.
- **Login item:** `LoginItemModel` in `BerthAppCore` (status, which `berthd` answers, the last error, all escaped) over a `LoginItemService` protocol; `LaunchAgentLoginItem` in the app wraps `SMAppService.agent(plistName:)`. Swift tests check that refreshing never registers, that the buttons register and unregister, and that failures and a foreign `berthd`'s facts are escaped; the Go e2e checks that `Status` names the running `berthd`; the plist keys were already in the bundle check.
- **App pin:** `codetrust.Deferred` pins a file on the first `Runs`; `execid.PinSigned` checks a copy of the whole bundle for an executable in `X.app/Contents/MacOS` (checked alone it fails with "invalid Info.plist") and pins the cdhash of every slice, and `codetrust.File` accepts a peer running any of them (the CLI, helper and proxy pins also take every slice now); `berthd` makes one for `Config.App`, default `../MacOS/Berth` from its own folder (`appExecutable`, tested). `api.AppOnly` reads the mark from the method descriptor like `AgentOnly`; its test builds a marked descriptor at run time, since no `berth.v1` method carries the mark, and checks that another peer gets `PermissionDenied` with `NOT_BERTH_APP` before the handler runs, that the app passes, and that an unmarked method never asks who calls. `TestReleaseTrustsOnlyAppleChain` covers the app's release requirement. There is no end to end test yet: one needs a marked method and `berth-app-probe` signed as `com.wesleybldc.berth` in a `MacOS` folder beside the test `berthd`, which item 10 can add.
- **`WatchNeeds`:** `internal/berthd/watchers.go` now holds the stream machinery `WatchSessions` and `WatchNeeds` share (`watchSet`, `serveStream`, `catchUp`); `needs.go` reads the three kinds' rows after the cursor and merges them by id, so a result within the backlog is complete and one beyond it sends a snapshot. The snapshot reads secret and grant rows by kind and only the live sessions' rows. Tests: needs opening and resolving for a store, a grant and leftovers with a quiet use in between; an open dialog in the snapshot and the need opening while the dialog waits (this test fails with the old after effects hook); resume, unknown cursor and behind; the stream bound and the clean end at stop; which transitions open a need. End to end: a store dialog held open by the fake helper opens a need, and a stream resumed after its cursor sees it resolve from the next `berthd`; a real headless browser whose owner exits opens a leftover need with the right browser kind, and `berth stop` resolves it.
- **App:** `NeedsStore` folds the stream and says what to post and remove; `NeedText` builds the title and body; `NeedsModel` runs the watch and posts only while turned on. `SessionsModel` and `NeedsModel` share one reconnect loop (`resumeWatching`, 1 second doubling to 10). `quoted`, the always quoting half of `shown`, is checked against Go's `strconv.QuoteToGraphic` for the whole injection corpus (`shown.json` gained a `quoted` column). The app's `SystemNotifier` and `NotificationSetting` (UserNotifications, the delegate that opens the window) are not unit tested: they are the manual check.
- **Not run:** the GUI, `SMAppService` registration and notification posting (manual checks 4 and 5), and the signed `make app`.
- **Deferred:** a need for an agent browser waiting on the person (none exists before takeover), a need for a power action other than a grant (none exists yet), and the menu bar's own badge for needs beyond the "waiting on you" line.

### Review fixes (2026-11-17)

- **Rows saved between the per kind reads** could be skipped forever: `needs.rows` read secret, grant and session rows one query each, so a secret row saved after the secret read sat below a grant row the next read returned, and the cursor passed it. `rows` now reads the newest id first and drops rows above it, so they come in the next round; `TestNeedsMissNoRowSavedBetweenKinds` saves rows between the reads and fails without the fix.
- **The app pin failed for a bundle signed executable**: it copied only `Contents/MacOS/Berth`, which codesign refuses alone ("invalid Info.plist"). It now checks a copy of the whole bundle and pins every slice of its executable; `TestPinSignedChecksAnAppBundle` signs a real bundle ad hoc with the hardened runtime, an `Info.plist` and a resource, pins it, and checks that another identifier and a changed resource are refused.
- **A need whose facts change while open** is posted again under its id; **`quoted` escapes a leading combining mark or variation selector**, in Swift and in the Go reference the fixture is written from; **the stream sends `NeedsCursor`** after rows that changed no need, and `NeedsStore` stores it, so a reconnect does not reread them.

### Second review fixes (2026-11-17)

- **The pin binds the cdhash to the check.** The pin folder is the user's own, so a same user process could rename its own ad hoc hardened binary over the copy between the requirement check and the cdhash read and get that cdhash pinned. Each slice's cdhash is now kept only after `codesign --verify --strict --architecture <arch> -R '(<requirement>) and cdhash H"<hash>"'` passes on the copy, for every pin; the whole copy is first checked with `--all-architectures`. `TestPinSignedRefusesACopySwappedAfterItsCheck` swaps the copy between the check and the read, for a file and a bundle, and fails without the fix.
- **The bundle's executable must be the one its `Info.plist` names** (`CFBundleExecutable`, read with `plutil`); `TestPinSignedRefusesAnExecutableTheBundleDoesNotName`.
- **Repins are single flight** and the 10 second spacing counts from a pin's end; a pin runs on its own context with a one minute limit, so a caller that gives up does not fail it for the others; `TestRepinsAreSingleFlightAndSpacedFromTheirEnd`.
- **The copy is hardened and bounded:** every entry is opened relative to its open folder with `O_NOFOLLOW` and `O_NONBLOCK`, checked with `fstat` to be a folder or a regular file and copied up to its `fstat` size, under 512 MiB, 10,000 files and 32 levels, stopping with its context; boot removes `berth-pin-*` folders older than an hour; `TestPinCopyRefusesOddEntriesAndStopsAtItsLimits` and `TestLeftoverPinFoldersGoAtBoot`.
- **A universal binary with one mismatched slice is refused**, `TestPinSignedRefusesAMismatchedSlice` (skipped without a working `lipo`).

### Agent safety checklist (M5, items 6 to 8)

1. No agent method was added; `TestAgentSurface` is unchanged, and `TestAppSurface` checks that no method is both an agent and an app method.
2. No value crosses `WatchNeeds`, `Status` or the app: a need holds an entity id, a kind, a secret's name, an owner's name and a browser kind; the fixtures hold no secret.
3. No new path carries a value, so no new canary.
4. No power action was added; registering the login item is macOS's own approval, and the app's buttons are its only start.
5. The secret's and the owner's names in a notification are always quoted with Go's escapes and labelled with who chose them; nothing an agent chose is stored as an event note.
6. No mutating method was added; `WatchNeeds` is read only (`TestMutatingMethodsTakeARequestID` lists it).
7. A refused app method is a typed `NOT_BERTH_APP`; a needs stream beyond the limit is `RATE_LIMITED`; a client failure shows as reconnecting with its escaped reason and backs off.
8. OWASP agentic risks: unchanged; the app pin is ready for item 10, so a process that is not the signed app cannot reach screen or input methods once they exist.
9. Needs streams are bounded at 16, each holding one goroutine and one page of rows; the app's posted set is bounded by the open needs, which a snapshot replaces.
10. Owner names and secret names reach the window and notifications only through `shown` or `quoted`.
