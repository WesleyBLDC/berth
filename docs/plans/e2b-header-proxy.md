# E2b: Header proxy and secret lifecycle

Goal: API keys reach HTTP APIs without ever entering the calling process, plus the rest of the secret lifecycle E2a left out.
Comes right after E2a.

## Builds

- `berth secret proxy <name> --host <api host> --header <Header>`: a grant whose consumer is a host and header, not an executable.
- A local HTTPS forward proxy in `berthd` (or a separate `berth-proxy` process) that the agent's tool uses through `HTTPS_PROXY`; it adds the header for the granted host only, after `berth-native` reads the value; the tool only ever holds a placeholder.
- Local CA: a Berth root certificate in the login keychain, trusted only for the proxied hosts, created with Touch ID; the private key never leaves `berth-native`.
- `berth secret copy` (Touch ID, clipboard cleared after 30 seconds, refused for agents), `rotate`, `rm`.
- The "Always Allow" Keychain audit, Herdr tab labels in prompts, MCP tools (`request_secret`, `list_secrets`, `request_grant`), `--from-clipboard`, and adopting existing Keychain items by reference.

## Acceptance tests

- A canary API key reaches a local test HTTPS server in the header, and appears in no environment, file, log or output of the calling process.
- A request to any other host never gets the header.
- `copy` is refused for an agent caller and works for a person with Touch ID.

## Open questions for planning

- Proxy inside `berthd` versus a separate process (blast radius versus simplicity).
- How tools are pointed at the proxy without trusting the agent's environment (grant pinned wrapper, or per tool config).
