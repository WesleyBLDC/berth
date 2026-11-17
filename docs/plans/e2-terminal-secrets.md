# E2: Terminal secrets

Goal: agents store and use Keychain secrets from the terminal without ever reading them, and Berth shows who holds what.
Design: https://wiki.wesleybldc.com/wiki-artifacts/berth/terminal-secrets and ADR 0012.

## Scope of this slice (E2a)

In:
- `berth secret request <name> --why "<reason>"`: a native dialog (secure field plus Touch ID) stores the value; the caller gets only `saved <name>`.
- `berth secret add <name>`: the person types the value at a no echo prompt.
- `berth secret ls`: names, consumers, last used, use count; never values.
- `berth secret grant <name> --exe <path> [--args <glob>]`: Touch ID; pins a command line tool by path plus SHA-256 of the file (or its code signature when signed).
- `berth run --env KEY=<name> -- <command>`: injects the value into that one child only when a grant matches the exact executable; masks the value in the child's output.
- Audit rows for store, grant, use, refusal.
- A Claude Code `PreToolUse` hook that blocks `security ... -w` reads and inline `-w <value>` writes and points to `berth secret`.

Out (E2b, later): the header proxy, `copy`, `rotate`, `rm`, the Keychain "Always Allow" audit, Herdr tab labels, MCP tools, `--from-clipboard`, adopting existing Keychain items.

## Invariants for this milestone

- Values exist only inside `berth-native` and in the child process `berth run` starts. They never cross the Connect API, never become a Go `string`, never reach logs, errors, events or replies.
- `berth run` gets the value from `berth-native` over an inherited pipe and puts it straight into the child's environment; `berthd` only decides and records.
- Every grant names a verified consumer; a changed binary hash loses its grants.
- Granting and adding consumers need Touch ID; the dialog text is built only from facts `berthd` verified (process tree, executable path, hash).
- Keychain items Berth creates use the service prefix `berth.` so they never collide with existing items.

## Work items

| # | Wave | Item | Owns | Acceptance tests it carries |
|---|---|---|---|---|
| 1 | Contract | Schema, ports, enums, migration | `proto/berth/v1/secrets.proto` (`SecretsService`: `RequestSecret`, `ListSecrets`, `GrantSecret`, `AuthorizeUse`), `gen/`, ports `SecretVault`, `Approver`, `ExecIdentity`, grant and secret `State`/`EventKind` enums, migration `0002_secrets.sql` | `buf breaking` clean; no field carries a value; `TestAgentSurface` lists `RequestSecret` and `ListSecrets` only |
| 2 | Build | `berth-native` (Swift) | `native/` Swift package: pipe protocol (length prefixed JSON frames, value bytes in a separate frame), Keychain add and read under `berth.<name>`, Touch ID prompt, secure input dialog, signed with the Personal Team | Canary round trip; Touch ID cancel returns `denied`; the value never appears on stdout or stderr except the value frame |
| 3 | Build | Grant machine | `internal/core/grant/` transitions (`Requested`, `Active`, `Revoked`, `Stale` on hash change) and secret machine (`Pending`, `Stored`, `Failed`) | Walk test with pinned counts; a hash change makes `Stale`; `Revoked` refuses use |
| 4 | Build | Native and identity adapters | `internal/adapters/native/` (Go side of the pipe, `[]byte` only, zeroed after use), `internal/adapters/execid/` (path resolve, SHA-256, `codesign` team ID) | A fake native helper speaks the protocol; a changed file hash is detected |
| 5 | Build | CLI | `cmd/berth` `secret request`, `add`, `ls`, `grant`, and `run` with output masking | Golden output for `ls`; masking replaces the value in stdout and stderr |
| 6 | Integrate | Wiring and e2e | `internal/berthd` handlers and actors, `test/e2e/secrets_test.go`, `.claude/hooks/guard-bash.sh` update, a `berth-secrets` skill | Canary e2e: request, store, grant, `berth run` sees it, a different executable is refused, the canary appears in no log, response or database row |

Items 2 to 5 run in parallel after item 1 lands; item 6 runs after them.
Touch ID and the dialog need a person, so e2e uses a test approver selected only by `BERTH_TEST_APPROVER=1` in a test `BERTH_HOME`, and one manual check proves the real dialog.

## Exit gate

`make check` passes with the canary e2e, the manual Touch ID check is recorded here, the agent safety checklist is walked for E2a, and the wiki plan is updated.
