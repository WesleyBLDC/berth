# E2b: Header proxy and secret lifecycle

Goal: API keys reach HTTP APIs without ever entering the calling process, the agent, `berth` or `berthd`, plus `revoke`, `rm` and `rotate`.
Comes right after E2a; E3 (isolated agents) follows.
Every check rests on kernel facts (audit tokens, live cdhash), never on paths or the caller's environment, which is the lesson of E2a's four review rounds.

## Defaults chosen (Wesley can override)

1. **Per run CA passed through environment variables**, not a persistent CA in the login keychain: no global trust change, no admin prompt. Trade: tools that only use the macOS trust store (Go binaries, Swift `URLSession`) cannot use the proxy in E2b.
2. **`copy` is deferred** to E3 or the M5 app: any agent can poll the clipboard while a person approves, and without E3 the "refused for agents" check is only a process tree heuristic.
3. **Scope moves:** MCP tools to M4 (with `berth-mcp` and session tokens), adopting existing Keychain items to after E3 (and only as a copy into `berth.<name>`, never by reference), the Always Allow audit, Herdr labels and `--from-clipboard` to later. The cdhash pinned suspended exec that closes the E2a hash to exec race is item 7, after integration.

## Design

### A separate signed `berth-proxy`, one per run

- Go, standard library only, signed with the hardened runtime as `com.wesleybldc.berth.proxy`; `berthd` pins it like the CLI and the helper (`codetrust`, live cdhash from the audit token).
- Not inside `berthd`: it parses hostile HTTP and TLS, and the value must never be in `berthd` (invariant 11).
- The value never becomes a Go `string`: the granted host gets a hand written HTTP/1.1 forwarder (`http.ReadRequest` for the request head, headers and body written from `[]byte`), ALPN `http/1.1` only.
- It refuses to start if its environment holds anything beyond `HOME`, `PATH`, `BERTH_HOME`, `TMPDIR` (blocks `GODEBUG`, `SSL_CERT_FILE`, `HTTPS_PROXY`, `GOTRACEBACK`).

### Use flow

1. `berth run --proxy <name> [--proxy <name2>] -- <cmd>`: the signed `berth` starts the `berth-proxy` beside its resolved executable (`BERTH_PROXY` override only under `berth_e2e`) and sends `{"secrets":[...]}` on stdin.
2. The proxy verifies the signed `berthd` on the socket, then calls `AuthorizeProxy(names)`; `berthd` accepts only the signed proxy, matches an `Active` proxy grant per name, and returns one ticket per grant (bound to name and proxy pid), each grant's target, and a `session_id`.
3. Per ticket, the proxy starts `berth-native read` with a socketpair end on fd 3. `berth-native` accepts a parent satisfying the CLI or the proxy requirement, and the redeem response's new `consumer_kind` must match the parent.
4. The proxy holds values as mlocked `[]byte`, makes a per run CA and leaves, listens on `127.0.0.1:0`, writes `ca.pem` and `bundle.pem` into a fresh 0700 folder, opens `WatchProxySession`, and replies to `berth` with port, credential, file paths and placeholders.
5. `berth` starts the child with the caller's environment except the variables Berth owns: `HTTPS_PROXY`/`https_proxy` (`http://berth:<cred>@127.0.0.1:<port>`), `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`, `NODE_USE_ENV_PROXY=1`, placeholders; `NO_PROXY`/`no_proxy` removed. The child holds no secret, so its environment does not need the E2a allowlist. `berth` never reads a value in proxy mode.
6. When the child exits, `berth` closes the proxy's stdin; the proxy calls `EndProxySession` with counts, zeroes values, deletes its folder, exits.

### The CA

- Fresh ECDSA P-256 per run, `CA:TRUE, pathlen 0`, critical `nameConstraints` permitting only the granted hosts, valid for the run and at most 24 hours.
- Leaves only for granted hosts. Keys exist only in proxy memory.
- `bundle.pem` is `/etc/ssl/cert.pem` plus the CA; other hosts are tunnelled untouched and the child verifies them itself.

### Who may connect

- Loopback TCP with a 128 bit per run credential in `Proxy-Authorization: Basic`, compared in constant time; otherwise 407.
- Honest: the credential sits in the child's environment, readable by any same user process; it grants only use of the granted hosts until the run ends or the grant is revoked, never the value.

### Injection rules

- Only `CONNECT`; absolute form plain HTTP gets 405, so nothing is ever injected over cleartext.
- Exact host and port match (lowercase, default 443) is terminated with the leaf; anything else is a plain tunnel with no header.
- On the granted host: `Host` must equal it (else 421); optional method list and path glob (else 403, counted); every client copy of the header removed; `<Header>: <prefix><value><suffix>` written; hop by hop headers dropped; redirects never followed.
- Upstream: dial the CONNECT target with `ServerName` set to the granted host, TLS 1.2 or later, system verifier (a compiled in test root only under `berth_e2e`), no proxy from the environment; a failed verification gets 502 with zero request bytes sent.
- Header names that cannot be granted: `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`, `Trailer`, `Proxy-*`. IP literals and `localhost` are refused outside `berth_e2e`. A value with CR, LF or NUL is refused.
- Backstop: the exact value is redacted from response headers and identity encoded bodies.

### What a hostile same user agent can still do

Use the key against the granted host while a run lives, within the grant's method and path limits; turn use into a readable credential if the API mints keys or echoes headers (mitigated by those limits); exfiltrate through the granted API itself; start `berth-proxy` directly (same as `berth run --proxy`); deny service.
It cannot read the value, send it to another host, send it over cleartext, keep using it after revocation, or find it in `berth`, `berthd`, the child, files or logs.

## Lifecycle

| Command | Design |
|---|---|
| `berth secret revoke <grant id>` | `RevokeGrant` sends `Revoke` (no Touch ID, it only lowers privilege) and stops running proxy sessions on that grant; not on the agent surface |
| `berth secret rm <name>` | `RemoveSecret`: an action ticket bound to `berthd`'s pid goes to `berth-native remove`, which redeems it, asks Touch ID, runs `SecItemDelete` (not found counts as removed); then every grant is revoked, live tickets dropped, sessions stopped; `BUSY` while Pending, Rotating or a grant is Requested |
| `berth secret rotate <name>` | `RotateSecret`: the store dialog in replace mode plus Touch ID, then `SecItemUpdate`; grants stay Active; running sessions keep the old value until they end |

## State machines

- **Grant:** no new states or events (still 11 legal pairs); the consumer kind (`exe` or `proxy`) is data on the grant row; proxy grants never see `HashChanged`.
- **Secret:** new states `Rotating`, `Removing`, `Removed`; events `RotateRequested`/`PromptRotate`, `RotateSucceeded|Denied|Failed` (back to Stored), `RemoveRequested`/`PromptRemove`, `RemoveSucceeded`/`RevokeAll` (to Removed), `RemoveDenied|Failed` (back to Stored), and `StoreRequested` from Removed. Uses accept Stored and Rotating. Boot recovery fails Rotating and Removing as "interrupted" and revokes grants of Removed secrets. Pin the walk count the test finds.
- **Proxy session** (new, `internal/core/proxysession`, entity `proxy/<id>`): `Authorized` (initial), `Serving`, `Stopping`, `Ended`, `Lost`; `ProxyReady`, `StopRequested`/`SendStop`, `ProxyEnded`, `ProxyLost` (watch stream dropped, kqueue `NOTE_EXIT`, no ready within 30 seconds, or boot recovery); 9 legal pairs. Fails closed: a stop or a broken stream makes the proxy zero its values and exit. Records counts only, never paths.

## Contract

- `secrets.proto` additions: `ConsumerKind`; `SecretState` `ROTATING`, `REMOVING`, `REMOVED`; `RemoveOutcome`; `ProxyTarget` (`host`, `port`, `header_name`, `value_prefix`, `value_suffix`, `path_glob`, `methods`, `placeholder_env`); `Consumer.kind` and `proxy`; `RedeemUseTicketResponse.consumer_kind` and `action`.
- RPCs: `GrantProxy` (Touch ID), `RevokeGrant`, `RemoveSecret` (Touch ID), `RotateSecret` (dialog plus Touch ID), `AuthorizeProxy` (signed proxy only), `WatchProxySession` and `EndProxySession` (only the audit token that authorized the session). `AuthorizeProxy`, `WatchProxySession`, `EndProxySession` join the agent surface (only the signed proxy is accepted).
- Error reasons: `BUSY`, `BAD_PROXY_TARGET`, `GRANT_NOT_FOUND`.
- Native protocol: new `rotate` and `remove` commands that redeem an action ticket before any prompt; `confirm` gains `"action":"proxy"` with the target; `read` accepts a CLI or proxy parent and checks `consumer_kind`.
- Migrations: `0004_proxy_grants.sql` rebuilds `grants` with `kind` and the proxy columns, keeping ids; `0005_proxy_sessions.sql` adds `proxy_sessions` and `proxy_session_grants`.

## Work items

| # | Wave | Item | Owns | Acceptance tests |
|---|---|---|---|---|
| 1 | Contract | Schema, ports, names, migrations | `proto/`, `gen/`, `internal/core/ports/`, secret and proxy session name constants, `internal/adapters/api/` interfaces and error mapping, `internal/codetrust` (`ProxySigningID`), migrations 0004 and 0005 | `buf breaking` clean; no field carries a value; agent surface pinned at 7 methods; migration keeps grant ids |
| 2 | Build | Machines and store | `internal/core/secret/`, `internal/core/proxysession/`, `internal/adapters/sqlite/`, `internal/testutil/` | Walk tests pinned; Removed accepts a new store; Removing refuses use; proxy grant round trip |
| 3 | Build | `berth-proxy` | `cmd/berth-proxy/`, `internal/proxy/` | `httptest` TLS: header only on the granted host and port; client copies removed; untrusted upstream 502 with zero bytes; plain HTTP 405; 407 without the credential; CA name constraints and pathlen 0; CR or LF refused; path and method limits; stop or stream drop zeroes and exits; refuses `GODEBUG` or `SSL_CERT_FILE` |
| 4 | Build | `berth-native` and its Go side | `native/`, `internal/adapters/native/` and `fakehelper/` | Receiver table (CLI with exe, proxy with proxy, crossed refused); action ticket redeemed before any prompt; remove treats not found as removed |
| 5 | Build, when a slot frees | CLI | `internal/cli/`, `cmd/berth/` | Golden `ls` with a proxy consumer; `run --proxy` never reads a value; the caller's proxy and CA variables are overwritten or removed; exit code passes through |
| 6 | Integrate | Wiring, e2e, docs | `internal/berthd/`, `cmd/berthd/trust_*.go`, `test/e2e/proxy_test.go`, `Makefile` (build and sign `bin/berth-proxy`), `ARCHITECTURE.md` | Canary and lifecycle e2e below; `make check` passes |
| 7 | After 6 | Close the hash to exec race | `internal/cli/`, `internal/berthd/` | `berth run` starts the child suspended, compares its live cdhash with the grant's pinned cdhash, then resumes or kills it; a swapped binary is refused |

**Canary e2e:** three local TLS servers (A granted with a test root, B not granted, C granted with a self signed certificate). A Go consumer that loads `SSL_CERT_FILE` itself sends a placeholder to A and B, plain HTTP to A, tries C, and dumps its environment. A receives exactly the injected header; B none; C a failed handshake; plain HTTP none; a wrong credential gets 407; the canary appears in no environment, output, log, database row or proxy temp file; the session ends `Ended`. Repeat with `/usr/bin/curl` and `node` when present to record a compatibility matrix.

**Lifecycle e2e:** revoke during a run stops injection; killing the proxy makes the session `Lost`; `rotate` puts the new value in the next run; `rm` revokes grants and refuses new runs; `berth-proxy` with `GODEBUG` set exits before contacting `berthd`.

## Exit gate

`make check` passes with both e2e suites, the compatibility matrix is recorded here, the agent safety checklist is walked for E2b, `ARCHITECTURE.md` and the wiki are updated, and Wesley's manual check (real signing, Touch ID, a real API with a test key) passes.

## Contract decisions (merged at 19cc6dd)

- Proto validation turns port 0 into 443 and refuses ungrantable header names (plain `InvalidArgument`); `BAD_PROXY_TARGET` is only for `berthd`'s own checks (IP literals and `localhost` outside `berth_e2e`). `berth-proxy` repeats the header check.
- `ports.ProxyTarget.Port` is always 1 to 65535. Proxy grants store empty `exe_path` and `sha256`; `GrantRecord.Kind` empty means `exe`.
- `WatchProxySession` streams `WatchProxySessionResponse{event}`; the first event is SERVING after the session moves to Serving; STOP carries a `ProxyStopReason`.
- `AuthorizeProxy` answers AUTHORIZED only when every name has an Active proxy grant, else NO_GRANT with no session and no tickets. `session_id` is 32 lowercase hex; the entity is `proxy/<session_id>`.
- `TrustProxy` runs before the request log for all three proxy methods; an unknown session or one authorized by another audit token answers `PEER_NOT_TRUSTED`, so ids cannot be probed.
- Rotate reuses `StoreOutcome`; rotate and remove share `ports.LifecyclePrompt{Name, Requester}` and take an action ticket; `TicketAction` is use, remove or rotate.
- `RemoveSecretResponse` returns `revoked_grant_ids`.
- `proxy_sessions` stores the proxy pid and counts only.
- Stubs to replace: `internal/adapters/native/lifecycle_stub.go` (item 4), `internal/berthd/proxy_stub.go` (item 6). Item 6 sets `Consumer.kind` for exe grants too.
- `berth-proxy` reads each value through the existing `native.Read` adapter (socketpair on fd 3); `berth-native` must accept a proxy parent for tickets whose `consumer_kind` is PROXY.

## Notes for the integration item (item 6)

- From item 2: secret machine has 17 legal pairs, proxy session 9; update both diagrams in `ARCHITECTURE.md`.
- From item 2: answer BUSY or SECRET_EXISTS before sending RotateRequested, RemoveRequested or StoreRequested in a state where the table refuses them; fire RevokeAll only from RemoveSucceeded; boot recovery sends RotateFailed or RemoveFailed with note "interrupted" and revokes grants of Removed secrets in the shell.
- From item 2: a second StopRequested on a Stopping session is illegal, so revoke and remove racing on one session must tolerate it; `EndProxySession` records once per session, and ProxyLost records zero counts.
- From item 2: exe grants read back with Kind `exe`; set `Consumer.kind` from the record.
