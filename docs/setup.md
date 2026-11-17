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
| Apple ID in Xcode | Free signing for M0 to M7 | Signed in; no Apple Development certificate yet (Xcode, Settings, Accounts, Manage Certificates, add Apple Development) |
