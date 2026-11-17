# Setup

Checked on 2026-11-16 on macOS 26.5.

| Tool | Needed for | Status on this Mac |
|---|---|---|
| Go 1.27 | Daemon, CLI, TUI | Installed (1.27.4) |
| Swift 6.3 | `berth-native`, the app | Installed (6.3.3) |
| Xcode | App and signed helper builds | Installed, but Command Line Tools are selected; use `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` |
| `buf` | Schema lint, breaking checks, code generation | Missing: `brew install bufbuild/buf/buf` |
| `golangci-lint` | Lint with `exhaustive` and `depguard` | Missing: `brew install golangci-lint` |
| `prek` | Pre-commit hooks | Missing: `brew install prek` |
| Node and `npx` | Playwright MCP in M0 and end to end tests | Used through `npx @playwright/mcp` |
| Apple ID in Xcode | Free signing for M0 to M7 | Sign in under Xcode, Settings, Accounts |
