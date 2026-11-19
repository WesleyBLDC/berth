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
10. `Berth.app` (`macos/`) linking Keychain code, writing to the socket before the peer check, or showing a process chosen string without `shown`.

## What the review rounds kept finding

The reviews of E2b to M4 kept finding these; look for them by name.

11. **Parser differentials.** Berth checks a message and another program then parses it (CDP JSON to Chrome, HTTP to an upstream, `procargs2` from the kernel, a URL to Chrome's parser). Block when the two could read it differently: duplicate keys, keys that differ by case, numbers in another form, stripped whitespace, a layout that is guessed. The fix refuses the input; it never re-encodes or guesses.
12. **Pid reuse.** A pid used without its start time or an audit token; a signal, a watch or a map keyed by pid alone; a child reaped while something still names its pid; a kill of a process group whose leader is already reaped.
13. **Unbounded memory.** A map, slice, queue, goroutine, actor, timer, stream or child process that a client, an agent or a process on the Mac can make grow without a limit; a machine with an end state and no `final`; an entry added on a path with no matching removal. Ask for the churn test.
14. **Terminal escapes.** A string a process, a server or an agent chose (paths, names, product strings, event details, error text from the daemon) printed without `shown`, in the CLI or the TUI, or cut before it is escaped.
15. **Untested branches and fake only defaults.** A refusal, a limit, a timeout or a fallback with no test that fails when it is removed; an injected dependency whose real default (the kernel call, the real process start, real SQLite) has no test of its own because every test passes a fake.
16. **Cleanup tied to the caller.** Stops, kills or revokes that run on the request's context, so a caller that cancels leaves them half done; cleanup that is not driven from the actor's `saved` hook on `berthd`'s context; a limit counted after the work it should bound.
17. **Tokens and the request log.** A token in a reply other than its create call, in a URL, an argument, a log line or the stored reply; a request id looked up without the principal; a trust check that runs after the request log is read.
18. **Inconsistent trust.** The same question (is this the signed binary, is this host local, is this path inside the profile, is this process the agent's) answered by two pieces of code, or checked on one path and not on its twin (owner and agent socket, `--env` and `--proxy`, launch and boot recovery).
19. **Docs overclaiming.** `ARCHITECTURE.md`, a plan's exit notes or a comment stating a limit, a count, a status or a command the code does not have. Check each sentence the diff adds against the code, and the pinned walk counts, the ports list and the agent surface against their tests.
20. **Enum and migration numbers.** A branch not rebased onto `main` right before its final check: its `ErrorReason` or migration number may be taken.

## Report

Each blocker: file and line, the concrete inputs that trigger it, and which numbered rule above it breaks. A scenario names the exact message, sequence or race; "could be unsafe" is a question. Anything you cannot make concrete is a question. Everything else is one line each under Later. End with `merge` or `merge after fixes`.
You may run `make test` and nothing that writes files.
