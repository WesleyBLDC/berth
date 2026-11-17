---
name: berth-review
description: "Review a Berth diff against its brief and the repository invariants, read only, reporting blockers with concrete scenarios. Use when reviewing any Berth branch or commit, before a merge, or when asked whether a Berth change is safe."
metadata:
  author: wesley
---

# Reviewing a Berth change

Read the brief or plan item, then the diff (`git diff <base>...HEAD`), then the files around it. Judge what the change does, not what its author meant.

## Block on

1. A state that is implied by flags or nullable fields instead of a `State` value; a transition with I/O, logging or `time.Now`; a `default:` hiding a missing case; a renamed or deleted stored state or event name; a changed legal pair count without a reason.
2. An effect run before its transition is saved, or a failure swallowed instead of becoming an event.
3. `internal/core` importing an adapter, `database/sql`, `os/exec` or `net/http` (lint should have caught it; check for workarounds such as interfaces defined in adapters).
4. A secret (Keychain value, token, password, cookie) in a `string`, a log line, an error message, an event note, a proto response, or test fixtures that are not canaries.
5. A new agent reachable method without `TestAgentSurface` updated, or an agent method that can read a secret or reach another session.
6. A mutating RPC without `request_id` idempotency, or validation missing on a caller set field.
7. Proto changes that renumber, retype or remove fields.
8. A behaviour branch with no test that fails without it; a test that mocks what an e2e test could run for real.
9. Anything beyond the brief's file plan without a reason.

## Report

Each blocker: file and line, the concrete inputs that trigger it, and which numbered rule above it breaks. Anything you cannot make concrete is a question. Everything else is one line each under Later. End with `merge` or `merge after fixes`.
You may run `make test` and nothing that writes files.
