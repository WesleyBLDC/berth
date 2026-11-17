---
name: berth-port-adapter
description: "Add a port (an interface the Berth core needs) and its adapters and fakes, keeping the core free of macOS, Chrome, SQLite and network code. Use when a task needs the core to reach something outside it: processes, browsers over CDP or BiDi, the Keychain or another secret store, Touch ID, notifications, or a new storage table."
metadata:
  author: wesley
---

# Ports and adapters

The core decides; adapters do. `depguard` fails the build if `internal/core` imports an adapter, `database/sql`, `os/exec` or `net/http`.

## Steps

1. **Port**: add the smallest interface the core needs to `internal/core/ports/ports.go` (or a new file there). Name it for what the core needs (`ProcessSource`, `BrowserDriver`, `SecretStore`, `Approver`), with methods that take a `context.Context` and return domain types from `ports`, not adapter types.
2. **Fake** in `internal/testutil/`: deterministic, in memory, scriptable (queue results, record calls). Core tests use only fakes.
3. **Adapter** in `internal/adapters/<name>/` implementing the port, with `var _ ports.X = (*Y)(nil)`.
   - Use `context` for every call; use `net.Dialer`, `net.ListenConfig` and `exec.CommandContext` (the `noctx` linter enforces it).
   - Native macOS work (Keychain, Touch ID, ScreenCaptureKit) belongs in Swift (`berth-native` or the app); the Go adapter talks to it over a pipe and never links Apple frameworks.
   - Secrets: read into a `[]byte`, zero it after use, never convert to `string`, never log it.
4. **Adapter tests** against the real thing where it is cheap (SQLite file, temp socket, headless Chrome in e2e), otherwise against a recorded fixture.
5. **Wire** the adapter in the shell (`internal/berthd`), never in the core.
6. Storage tables: add a numbered migration in `internal/adapters/sqlite/migrations/` (take the next free number when you rebase) and keep `STRICT` tables.
7. `make check` ends with `check: PASS`.
