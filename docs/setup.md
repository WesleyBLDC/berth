# Setup

Checked on 2026-11-16 on macOS 26.5.

| Tool | Needed for | Status on this Mac |
|---|---|---|
| Go 1.27 | Daemon, CLI, TUI | Installed (1.27.4) |
| Swift 6.3 | `berth-native`, the app | Installed (6.3.3) |
| Xcode | App and signed helper builds | Xcode 26.6 installed; Command Line Tools are selected, so builds set `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` |
| `buf` | Schema lint, breaking checks, code generation | Installed (1.75.0) |
| `golangci-lint` | Lint with `exhaustive` and `depguard` | Installed (2.15.0) |
| `prek` | Pre-commit hooks | Installed (0.5.11) |
| Node and `npx` | Playwright MCP in M0 and end to end tests | Used through `npx @playwright/mcp` |
| A Chromium | The browser end to end tests of M2, M3 and M4, which skip without one and install nothing | Found in this order: `BERTH_E2E_CHROME`, Playwright's cache (`~/Library/Caches/ms-playwright`), Puppeteer's cache (`~/.cache/puppeteer`), `/Applications/Google Chrome.app` |
| `playwright-core`, `puppeteer-core` | The M4 driver compatibility test, which skips without them | Found in npx's cache, or through `BERTH_E2E_PLAYWRIGHT_CORE` and `BERTH_E2E_PUPPETEER_CORE` |
| Apple ID in Xcode | Free signing for M0 to M7 | Personal Team; Apple Development certificate valid, team `XPJ7XA42J4`. It needs Apple's WWDR G3 intermediate (`AppleWWDRCAG3.cer` from apple.com/certificateauthority) in the login keychain, added 2026-11-16 |

## macOS permissions, all of them

Permissions are tied to the bundle ID plus the signing certificate, so every build keeps one stable ID and is signed with the same Apple Development certificate. The app is `com.wesleybldc.berth` from the spike onward, so a grant made once carries to the real `Berth.app`.

| Permission | Who holds it | When | One time setup |
|---|---|---|---|
| Screen and System Audio Recording | `Berth.app` (`com.wesleybldc.berth`) | M0 proof 5, then M5 window pictures for Safari and Cowork | System Settings, Privacy and Security, Screen and System Audio Recording: turn on Berth |
| Touch ID | `berth-native` | M0 proof 4, M6, E2 | None: Touch ID needs no grant, only a finger per prompt |
| Keychain access per item | `berth-native` | First use of each existing item (M6, E2) | Choose Always Allow once per item; items Berth creates trust it already |
| Login Items and Extensions | `Berth.app` registering `berthd` | M5 | System Settings, General, Login Items: allow Berth once |
| Notifications | `Berth.app` | M5 (Needs you alerts) | Allow on first launch |
| Chrome remote debugging toggle | Your daily Chrome | M7 only | chrome://inspect/#remote-debugging, then Allow per connection |
| Paste from other apps | `berth` CLI or app | E2 `--from-clipboard` | Allow if macOS asks |
| 1Password or Bitwarden CLI | Their apps | E1 only | Turn on their CLI integration |
| Paid Apple Developer Program | Signing | M8 release only | Enroll |

`make app` builds and signs `bin/Berth.app` with the same identity; `make check` builds the bundle signed ad hoc only to check it, and never opens it or asks for a permission.
The app asks for nothing on its own: Login Items, Notifications and Screen Recording each start from a button in the app (see [the M5 plan](plans/m5-mac-app.md#manual-checks)).

Not needed: Accessibility (input goes through CDP, not synthetic events), Automation (no Apple Events), Full Disk Access (scanning your own processes needs none), Local Network (loopback only), camera and microphone.

## Planned for the test lab

The [test lab](plans/test-lab.md) is planned, not built; none of these is installed or needed for `make check` today.

| Tool | Needed for |
|---|---|
| `tart` and Packer with `packer-plugin-tart` | The macOS base image and the VMs `make lab-test` clones |
| apple/container | Linux containers for the outside world (upstreams, DNS, hostile pages) |
| `shfmt`, `markdownlint-cli2`, `actionlint` | The lint additions in lab item 2 |
