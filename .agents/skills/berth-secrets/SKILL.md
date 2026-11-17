---
name: berth-secrets
description: "Store and use secrets (API keys, tokens, passwords) through Berth without ever seeing them: berth secret request, berth secret ls, berth run --proxy for HTTP APIs, berth run --env, revoke, rm and rotate, what each exit code means and how to fix it. Use whenever a task needs a credential, an environment variable holding one, or a Keychain item, and before any security find-generic-password or add-generic-password command."
metadata:
  author: wesley
---

# Using secrets through Berth

This skill is for agents that use Berth, not for agents building it.
Berth keeps secret values away from you: the person types a value into a native dialog, and only the one command a grant names ever receives it.
You store and use secrets; you never read them.

## Rules

- Never ask for a secret in chat, and never accept one pasted there; point the person to the dialog instead.
- Never try to read a value: no `security find-generic-password -w` or `-g`, no printing the variable `berth run` sets, no writing it to a file. A hook blocks the Keychain reads.
- Never put a value on a command line (`security add-generic-password ... -w <value>`, `export TOKEN=...`).
- Output from `berth run` shows the value as `****`; that is expected, not a bug to work around.
- When you run as Berth's agent user, the person sets `BERTH_AGENT_SOCKET` and `BERTH_AGENT_TOKEN_FILE`, and `berth` sends your session token for you. Never print, copy or move the token file, and never pass the token on a command line; a token used from outside your process tree is refused. If `berth` says it refused the agent token, ask the person for a new session.

## Store a secret

```sh
berth secret request <name> --why "<what you need it for>"
```

The person sees a dialog with your reason (labelled as your words), pastes the value and confirms with Touch ID.
You get only `saved <name>`.
Write the reason for the person: what task needs it and what it will be used with.

Name it `<vendor>-<credential name as shown in the vendor's console>-<part>`, lowercase, one secret per value, for example `twilio-render-backend-staging-key-sid` and `twilio-render-backend-staging-key-secret`.
Names use `a-z`, `0-9`, `.`, `_` and `-`.

## See what exists

```sh
berth secret ls          # names, state, consumers, last used, use count; never values
berth secret ls --json
```

Check here before requesting, so you do not ask for a secret that is already stored.

## Use an API key with an HTTPS API: berth run --proxy

Prefer this for API keys: the command never holds the value at all.

```sh
berth run --proxy openai-api-key -- curl -sS https://api.openai.com/v1/models
```

`berth run --proxy` starts a per run proxy that writes the key into one header of requests to the one host the person granted, and passes everything else through untouched.
The command gets your environment plus `HTTPS_PROXY`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS` and `NODE_USE_ENV_PROXY=1`, with `NO_PROXY`, `HTTP_PROXY` and `ALL_PROXY` removed.
If the grant names a placeholder variable (for example `OPENAI_API_KEY`), the command sees a stand in value there, so tools that refuse to start without a key still start; the proxy replaces whatever header the tool sends with the real one.
Repeat `--proxy` for more secrets; `--proxy` and `--env` cannot be combined.

- Works with tools that honour `HTTPS_PROXY` and those CA variables; tested with `curl` and Node 24 `fetch`.
- Does not work with tools that use only the macOS trust store: Go binaries and Swift `URLSession`. Use `--env` for those, with a grant to that exact tool.
- Plain `http://` requests through the proxy get 405; only HTTPS is proxied.
- Granting is the person's step, with Touch ID: ask them to run `berth secret proxy <name> --host <host> --header <Header> [--prefix "Bearer "] [--placeholder-env NAME] [--method M ...] [--path <glob>]`. You cannot grant.

## Use a secret in a command's environment: berth run --env

```sh
berth run --env GH_TOKEN=gh-personal-token -- gh api user
```

`berth run` puts the value in that one command's environment, and only when the person has granted that exact executable.
Repeat `--env` for more secrets.
Granting is the person's step, with Touch ID: ask them to run `berth secret grant <name> --exe <path> [--args <glob>]`; you cannot grant.
Interpreters and tools that run configured commands (shells, `python`, `node`, `git`, `ssh`, `make`, `#!` scripts) are refused unless the person adds `--allow-interpreter`, because anything an interpreter runs could print the secret; prefer granting the real tool.
Only the signed `berth` gets secrets: a script of yours that calls the API or starts `berth-native` itself is refused, and `berth run` talks only to the signed `berthd`.
The command gets a clean environment: `HOME`, `PATH`, `TERM`, `LANG`, `LC_*`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, the secrets, and only the variables its grant lists (`berth secret grant ... --pass-env NAME`). Variables you set on `berth run` do not reach it otherwise; if the tool needs one, ask the person to grant it with `--pass-env`. `DYLD_*`, `LD_*` and `*_OPTIONS`/`*_OPTS`/`*OPT` never pass.
A grant pins the file's hash, so after the tool is upgraded the person grants it again.

## Exit codes and fixes

| Code | Meaning | Fix |
|---|---|---|
| 0 | Success | |
| 1 | berthd unreachable, or it refused the request; the message says why | `berth status`; for "already exists" use the stored secret; for "no stored secret" request it; for "signed berth binary" or "not the signed berthd" the person runs `make build`, starts `bin/berthd`, and you use `bin/berth`; for "cannot read secret" the person runs `make native` and `make build`, and you use `bin/berth` directly rather than a copy or a wrapper; right after a rebuild, wait 10 seconds and try once more; for "is an interpreter" ask the person whether to grant the real tool instead |
| 2 | Usage error | Check the command shape above |
| 3 | The person denied the dialog or Touch ID | Do not retry on your own; ask the person whether they want to |
| 4 | The store, grant, rotate or remove failed | Read the message, then ask the person |
| 5 | `run`: no grant lets this executable use the secret with these arguments; nothing started | Ask the person to run the `berth secret grant` line the error prints |
| 6 | `run`: the executable changed since it was granted; nothing started | Ask the person to grant it again with the printed line |

| 7 | The secret is busy: a store, rotate or remove dialog, or a grant's Touch ID, is waiting for the person | Wait until the person finishes, then try once more |
| 8 | `run --proxy`: the proxy did not start; nothing started. Usually no active proxy grant for a name (the message says so), or the secret was removed | `berth secret ls`; ask the person to grant the proxy with the `berth secret proxy` line above |

Once the command starts, `berth run` exits with the command's own code.

## Revoke, remove and rotate

```sh
berth secret revoke <grant id>   # the first column of each consumer line in berth secret ls
berth secret rm <name>           # the person confirms with Touch ID
berth secret rotate <name>       # the person types the new value and confirms with Touch ID
```

- `revoke` needs no Touch ID, since it only lowers privilege; revoke a grant you no longer need. A proxy run using that grant stops at once.
- `rm` deletes the Keychain item, revokes every grant of the secret and stops its proxy runs; the name can be requested again afterwards. Ask the person before running it.
- `rotate` replaces the value; grants stay active, and runs already going keep the old value until they end. Ask the person before running it.
