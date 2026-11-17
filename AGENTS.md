# Berth: agent instructions

Berth is a local macOS app that lists every browser agents, tests and scrapers have open, lets a person watch and take over any of them, and brokers Keychain secrets to browsers and terminals without agents ever reading them.

The design lives in the wiki and in `docs/adr/`; read them before changing behaviour:
https://wiki.wesleybldc.com/wiki-artifacts/berth (start with the reading order on that page).

## Rules that are never negotiable

- Every stateful thing is an explicit state machine: one `state` field, a closed set of events, one transition table. No boolean flags whose combinations mean something.
- Transitions are pure functions returning the next state and a list of effects; they do no I/O. The effect runner does the I/O.
- `internal/core` never imports an adapter. `depguard` enforces it.
- Observations (process seen, page activity) arrive as events; nothing writes state directly.
- Secrets never cross the API, never enter a Go string, never reach logs, errors or agent replies. Only `berth-native` and secret helpers read them.
- Agents can store and use secrets, never read them. Power actions need Touch ID.
- Approval prompts are built only from facts the daemon verified, never from text an agent sent.
- No third party code runs inside `berthd`.
- Nothing is pushed to a remote until the open source milestone (M8).

## How to make a change

1. Change the contract first: the Protobuf schema in `proto/` or the transition table of the machine involved.
2. Write or update the acceptance tests for it.
3. Change the core, then adapters, then clients.
4. Run `make check` (lint, `buf lint`, `buf breaking`, tests) before committing.
5. If the change alters a decision, add or supersede a record in `docs/adr/` and update the wiki page.

## Lint and hooks

- `make hooks` installs the prek git hooks; `make lint` runs every hook over every file; `make fmt` formats Go, Swift, Protobuf and Python.
- Go: `golangci-lint` v2 with `.golangci.yml` (gofumpt, goimports, exhaustive, depguard keeping `internal/core` free of adapters, forbidigo banning `time.Now` in the core, gosec and more).
- Swift: `swift format` from the Xcode toolchain with `.swift-format`.
- Protobuf: `buf format` and `buf lint`.
- Python and shell: `ruff` and `shellcheck`.
- Every file: trailing whitespace, final newline, private key detection, `gitleaks`, and no em or en dashes in Markdown.
- Claude Code hooks in `.claude/settings.json`: after every edit the file is formatted and linted, and problems come straight back to the agent; a guard refuses Bash commands that would print a Keychain secret or put one on the command line.

## Writing style

- Plain commit messages, no `type:` prefixes, no co-author trailers.
- No dashes as punctuation in docs or messages; hyphens inside words are fine.
- One sentence per line in long Markdown.
- Comments explain why, never what.
