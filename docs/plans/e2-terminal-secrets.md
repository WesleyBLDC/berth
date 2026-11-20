# E2: Terminal secrets

Status: E2a is Done; the maintainer's manual check with real signing and Touch ID passed on 2026-11-17 (below). E2b has [its own plan](e2b-header-proxy.md).

Goal: agents store and use Keychain secrets from the terminal without ever reading them, and Berth shows who holds what.
Design: [ARCHITECTURE.md](../../ARCHITECTURE.md#secrets-store-and-use-never-read) and ADR 0012.

## Scope of this slice (E2a)

In:

- `berth secret request <name> --why "<reason>"`: a native dialog (secure field plus Touch ID) stores the value; the caller gets only `saved <name>`.
- `berth secret add <name>`: the same native dialog, with the person as the requester, so no value ever passes through a Go process to store it.
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

## Flows (fixed by the contract item)

**Store** (`request` and `add`):

1. CLI calls `RequestSecret(request_id, name, reason)`.
2. `berthd` builds the requester facts from the socket peer: pid, executable, process tree up to the harness, working directory.
3. `berthd` asks `berth-native` (port `Approver.StoreSecret`) to show the dialog with those facts; the person pastes the value and confirms with Touch ID; `berth-native` writes Keychain item `berth.<name>`.
4. `berthd` gets only `stored`, `denied` or `failed`, records the transition, and replies with that outcome.

**Grant**: `GrantSecret(request_id, name, exe_path, args_glob)`; `berthd` identifies the executable (port `ExecIdentity`: resolved path, SHA-256, team and signing ID when signed), asks `Approver.ConfirmPower` for Touch ID with those facts, and stores the grant.

**Use** (`berth run --env KEY=<name> -- <cmd>`):

1. `berth run` resolves `<cmd>` and calls `AuthorizeUse(request_id, name, exe_path, argv)`.
2. `berthd` identifies the executable itself (never trusting a client hash), matches an `Active` grant, and returns a single use ticket: 32 random bytes, bound to name, exe hash and the caller's pid, valid 30 seconds.
3. `berth run` starts `berth-native read`, passing the ticket on stdin.
4. `berth-native` calls `RedeemUseTicket(ticket)` over the socket; `berthd` accepts it only from a peer whose executable is the signed `berth-native` (team `XPJ7XA42J4`), marks it used, and returns the name.
5. `berth-native` checks that fd 3 is a socket whose peer is its signed `berth` parent, then reads `berth.<name>` and writes the value to fd 3, one end of a socketpair `berth run` created; `berth run` holds it only as `[]byte`, puts it in the child's environment, zeroes its copy, and masks it in the child's output.
6. A changed executable hash moves its grants to `Stale`, so step 2 refuses until the person grants again.

Neither `RedeemUseTicket` nor `GrantSecret` is on the agent surface. Agents get `RequestSecret`, `ListSecrets` and `AuthorizeUse`.

## Work items

| # | Wave | Item | Owns | Acceptance tests it carries |
|---|---|---|---|---|
| 1 | Contract | Schema, ports, enums, migration | `proto/berth/v1/secrets.proto` (`SecretsService`: `RequestSecret`, `ListSecrets`, `GrantSecret`, `AuthorizeUse`, `RedeemUseTicket`), `gen/`, ports `Approver`, `ExecIdentity`, `PeerFacts`, `TicketSource`, grant and secret `State`/`EventKind` enums, migration `0002_secrets.sql` | `buf breaking` clean; no field carries a value; `TestAgentSurface` lists `Handshake`, `RequestSecret`, `ListSecrets` and `AuthorizeUse` |
| 2 | Build | `berth-native` (Swift) | `native/` Swift package: the Native helper protocol below (one JSON line in and out; raw bytes for `read`), Keychain add and read under `berth.<name>`, Touch ID prompt, secure input dialog, signed with the Personal Team | Canary round trip; Touch ID cancel returns `denied`; the value never appears on stdout or stderr except the value frame |
| 3 | Build | Grant machine | `internal/core/grant/` transitions (`Requested`, `Active`, `Revoked`, `Stale` on hash change) and secret machine (`Pending`, `Stored`, `Failed`) | Walk test with pinned counts; a hash change makes `Stale`; `Revoked` refuses use |
| 4 | Build | Native and identity adapters | `internal/adapters/native/` (Go side of the pipe, `[]byte` only, zeroed after use), `internal/adapters/execid/` (path resolve, SHA-256, `codesign` team ID) | A fake native helper speaks the protocol; a changed file hash is detected |
| 5 | Build | CLI | `cmd/berth` `secret request`, `add`, `ls`, `grant`, and `run` with output masking | Golden output for `ls`; masking replaces the value in stdout and stderr |
| 6 | Integrate | Wiring and e2e | `internal/berthd` handlers and actors, `test/e2e/secrets_test.go`, `.claude/hooks/guard-bash.sh` update, a `berth-secrets` skill | Canary e2e: request, store, grant, `berth run` sees it, a different executable is refused, the canary appears in no log, response or database row |

Items 2 to 5 run in parallel after item 1 lands; item 6 runs after them.
Touch ID and the dialog need a person, so the end to end tests use a fake helper written in Go (`internal/testutil/fakehelper`, chosen through `BERTH_NATIVE` only in `berth_e2e` builds), and one manual check proved the real dialog.

## Exit gate

`make check` passes with the canary e2e, the manual Touch ID check is recorded here, and the agent safety checklist is walked for E2a. All three hold.

### Manual check (2026-11-17)

The maintainer ran it on real hardware with real signing and Touch ID; every step passed.

1. `make build` built and signed `berth`, `berthd` and `berth-native`.
2. `berthd` trusted the signed binaries beside it.
3. A secret was stored through the `berth-native` dialog and Touch ID.
4. `berth secret ls` showed it `stored`, with no value.
5. It was granted to `/usr/bin/printenv`, after Touch ID.
6. `berth run --env` printed `****` in place of the value.
7. A copied `berth` and `berthd` with a wrapper `berth-native` got "helper refused the ticket", and the wrapper's leak file stayed empty.
8. `security add-trusted-cert` into the user trust domain asked for authorization, which was cancelled; this answers the E2b question on user trust settings (see [the E2b plan](e2b-header-proxy.md#e2b-exit-gate)).

Found during the check: `berth secret grant` showed `team unsigned` for `/usr/bin/printenv`, an Apple platform binary that is signed but names no team.
Grants now record the signature kind `berthd` verified (migration 0007), and the grant output, `berth secret ls` and the Touch ID prompt show `Apple (platform)`; only code with no valid signature reads `unsigned`, and ad hoc code reads `ad hoc`.

## Native helper protocol (items 2, 4 and 6 must match it exactly)

`berth-native <command>` reads one JSON object on one line from stdin and writes one JSON object on one line to stdout, except `read`.
It exits 0 for every outcome, including denied; a non-zero exit means it crashed or was misused.
stderr carries short diagnostics and never a value.
`berthd` always starts it with a scrubbed environment (only `HOME`, `PATH`, `BERTH_HOME`).

| Command | stdin | stdout |
|---|---|---|
| `store` | `{"name":"...","reason":"...","requester":{"pid":1,"executable":"/path","tree":[{"pid":1,"executable":"/path"}],"working_dir":"/path"}}` | `{"outcome":"stored"}`, `"denied"` or `"failed"`, with optional `"error":"<short text>"` |
| `confirm` | `{"action":"grant","secret":"...","consumer":{"path":"/path","sha256":"<64 hex>","team_id":"","signing_id":"","signature":"unsigned"},"args_glob":"","requester":{...}}` | `{"outcome":"approved"}`, `"denied"` or `"failed"` |
| `read` | the ticket as 64 lowercase hex characters and a newline | nothing on stdout; the raw value bytes go to fd 3 (a socketpair end from `berth run`, checked by the kernel to belong to the signed `berth` parent) on any failure nothing is written and exit 3 |

- Keychain item: generic password, service `berth.<name>`, account `berth`, login keychain.
- `store` refuses with `failed` if `berth.<name>` already exists.
- `read` redeems the ticket itself: Connect unary JSON over HTTP/1.1 on the Unix socket `$BERTH_HOME/berthd.sock` (default `~/Library/Application Support/Berth/berthd.sock`): `POST /berth.v1.SecretsService/RedeemUseTicket`, `Content-Type: application/json`, body `{"requestId":"<random hex>","ticket":"<base64>"}`, response `{"name":"..."}`. No generated Swift code is needed.
- Dialog and Touch ID text is built only from the JSON facts; the reason is shown as quoted text the requester wrote, never as a heading.
- Test switches exist only in `#if DEBUG` builds. Go tests and e2e use a fake helper written in Go that speaks this protocol, never the Swift binary.

## Decisions from the contract item (merged at 9027153)

- `add` calls `RequestSecret` with a fixed reason; `reason` is required (1 to 500).
- `RequestSecret` on a Stored name: `ERROR_REASON_SECRET_EXISTS` (replacing is rotate, E2b).
- Grant or use on a name that is not Stored: `ERROR_REASON_SECRET_NOT_FOUND`; an unreadable executable: `ERROR_REASON_EXECUTABLE_NOT_FOUND`.
- Unknown, expired or used ticket: `ERROR_REASON_TICKET_INVALID`; a peer that is not the signed helper: `ERROR_REASON_PEER_NOT_TRUSTED`, checked before the request log is read.
- `USE_OUTCOME_NO_GRANT` covers no grant, denied, revoked, and argv not matching the glob; `STALE_GRANT` only for a hash change. An empty `args_glob` allows any arguments; `argv` excludes the executable.
- Grant ids are SQLite autoincrement; sha256 is lowercase hex outside the core and `[32]byte` inside.
- Tickets live only in `berthd` memory and are never persisted.
- An event `Note` never holds the requester's reason, because an agent controls that text.
- `UseRefused` goes on the grant entity when a grant exists, else on the secret entity.
- The grant machine has no Failed state; item 3 decides where `GrantFailed` goes.
