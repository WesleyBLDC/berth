# Agent safety

How Berth keeps an agent that is confused, prompt injected or jailbroken from doing anything beyond its task, and the checklist every milestone walks before it closes.
It extends the security model in [ARCHITECTURE.md](../ARCHITECTURE.md#security-model).

The core rule: Berth never trusts the agent's judgement.
Every agent can be hijacked by text on a web page or in a repository, so every limit is enforced by deterministic code outside the model: the daemon, the native helper, the CDP proxy and Touch ID.
Instructions and prompts are never a security control.

Most controls below are planned for the browser milestones (M4 to M6); each row says which exist today.

## Why browser agents are the hard case

| Framework | What it says | What it means for Berth |
|---|---|---|
| [Lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) | Private data, untrusted content and a way to send data out, together, let an attacker steal data | A browser agent with your logins has all three by nature |
| [Agents Rule of Two](https://ai.meta.com/blog/practical-ai-agent-security/) | An agent should have at most two of untrusted input, sensitive access and the power to act; with all three, a human approves | Berth cannot remove one, so it supplies the human and the hard limits |
| [OWASP Top 10 for Agentic Applications](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications/) | ASI01 to ASI10, from goal hijack to rogue agents | Each one is mapped below |
| [OWASP LLM Top 10 2025](https://genai.owasp.org/llm-top-10/) | LLM01 prompt injection, LLM02 sensitive information disclosure, LLM06 excessive agency | Least privilege and approvals for high impact actions |
| [MCP security best practices](https://modelcontextprotocol.io/specification/2026-07-28/basic/security_best_practices) and the [OWASP MCP cheat sheet](https://cheatsheetseries.owasp.org/cheatsheets/MCP_Security_Cheat_Sheet.html) | No token passthrough, least privilege per server, consent on install, pinned tool definitions | Shapes `berth-mcp` and the token proxy |

## Controls mapped to the OWASP agentic list

| Risk | How it hits Berth | Control | Status |
|---|---|---|---|
| ASI01 Goal hijack | A page tells the agent to log in somewhere else or send data out | Site binding on every fill; a per profile site allowlist enforced in the browser; session token scope | Planned (M4, M6) |
| ASI02 Tool misuse | The agent calls tools in harmful combinations | A short agent method allowlist pinned by `TestAgentSurface`; CDP methods outside the session refused; rate limits per session | Allowlist built; the rest planned |
| ASI03 Identity and privilege abuse | The agent acts as you with your logins | Grants per consumer (executable today, profile and site later); power actions need Touch ID; strict mode | Terminal grants built; browser grants planned |
| ASI04 Supply chain | A fake secret helper, signature file or MCP shim | The helper is trusted by code signing team; other store helpers pinned by signing identity and added with Touch ID; signatures are data, never code; pinned dependencies, `go mod verify`, `govulncheck` | Partly built |
| ASI05 Unexpected code execution | Agent input reaches a shell | Berth never runs a command built from agent input; helpers get a reference on stdin, never shell arguments | Built |
| ASI06 Memory and context poisoning | The agent rewrites its own runbook to grant itself more | Agents can read runbooks, never write them; runbook changes land in the audit log | Planned (M6) |
| ASI07 Insecure agent to agent messages | One agent impersonates another | One token per session; no agent can see or call another session | Planned (M4) |
| ASI08 Cascading failures | A loop hammers a login page and locks your account | Rate limits and a circuit breaker that moves the session to Needs you | Planned (M6) |
| ASI09 Human trust exploitation | The agent writes a convincing approval request | Dialog and Touch ID text built only from facts the daemon verified; agent supplied text shown quoted and labelled as the agent's claim | Built for secrets |
| ASI10 Rogue agents | The agent keeps going after it should stop | Kill switch per session and for all agents; anomaly rules pause the session into Needs you | Planned |

## The secret is never readable by the agent

Agents store and use secrets but never read them ([ADR 0012](adr/0012-agents-can-store-and-use-secrets-never-read-them.md)).
In the terminal, values travel only from `berth-native` over a pipe into the one child process `berth run` starts; the flow is in [ARCHITECTURE.md](../ARCHITECTURE.md#secrets-store-and-use-never-read).

In a browser (planned, M6), typing a password into a page puts it in the DOM, where any CDP client can read it back with `Runtime.evaluate` or a screenshot, so the fill runs as one atomic step the agent cannot watch ([ADR 0008](adr/0008-atomic-fill-with-the-agent-paused.md)):

1. **Pause the agent.** The CDP proxy holds the agent's messages for this session.
2. **Check, fill and submit.** Berth checks the page's real origin and the form's target against the grant, types the value over its own CDP session on Chrome's private pipe, and submits.
3. **Wipe if needed.** If the page did not navigate away (a failed login), Berth clears every field it filled.
4. **Resume the agent.** Every held message is scanned for the value and dropped if it matches; the agent receives only `filled`, `refused` or `failed`.

The harness never passes a password and never receives one: the planned `fill_login` tool has no secret parameters, not even the Keychain item name, and `berth-mcp` adds the session token itself, so the token never enters the model's context.

**Where a value never goes:** tool arguments or results, Connect or MCP messages to clients, command line arguments, files, logs, error reports, event notes, screenshots, or the agent's CDP connection.
The one deliberate exception is the environment of the child that `berth run` starts, pinned by a grant.

**If a session token leaks** (planned, M4): it works only over the local socket, only as your user, only from the agent's own process tree, only for that session's agent methods, and it ends with the session; it can ask Berth to fill on a granted site, never read a value.

**What this cannot cover:** an agent driving a browser Berth does not own, and a command that leaks the value it was given; see the honest limits in [ARCHITECTURE.md](../ARCHITECTURE.md#honest-limits).

## Hard limits per profile (planned, M6)

Set in local config, enforced by the daemon, never changeable by an agent:

| Limit | Example |
|---|---|
| Site allowlist | `agent-portal.example.com`, `*.carrier.example` |
| Fill budget | 10 fills per hour per session |
| Navigation budget | 300 page loads per hour |
| Downloads | Off, or a fixed folder |
| New windows and tabs | A maximum, then Needs you |
| Session lifetime | 4 hours, then stop |

## Milestone checklist

Walk this for everything a milestone added, and record the result in its plan in `docs/plans/`.

1. Every new agent reachable method is marked in the schema, listed in `TestAgentSurface`, and cannot read a value or reach another session.
2. No value appears in a `string`, a log line, an error, an event note, a proto field, a fixture that is not a canary, or the request log.
3. A canary test plants a fake secret and fails if it reaches any reply, log, database row or agent facing output the milestone added.
4. Every power action (new grant, new consumer, connecting to your own browser, weakening a security setting) needs Touch ID, and its prompt text comes only from verified facts.
5. Text an agent controls (reasons, names, page titles) is quoted and labelled wherever a person sees it, and never stored as an event note.
6. Every mutating method takes a `request_id`, and a retry cannot act twice.
7. Every new failure path ends in a named state or a typed error, never a silent retry.
8. Each OWASP agentic risk above is either covered, unchanged, or written down as a known limit with the milestone that closes it.

## Standards the project follows

| Standard | Used for |
|---|---|
| OWASP Agentic Top 10 and LLM Top 10 | The threat checklist above, reviewed at every milestone gate |
| OWASP ASVS (selected chapters) | Session tokens, logging, secret handling, input validation |
| NIST SSDF | Threat model per milestone, review of contracts, vulnerability scanning |
| SLSA level 2 at release | Signed builds with provenance |
| Apple platform security | Hardened runtime, notarization, login items, Keychain access control |
