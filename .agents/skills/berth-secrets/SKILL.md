---
name: berth-secrets
description: "Store and use secrets (API keys, tokens, passwords) through Berth without ever seeing them: berth secret request, berth secret ls, berth run --env, what each exit code means and how to fix it. Use whenever a task needs a credential, an environment variable holding one, or a Keychain item, and before any security find-generic-password or add-generic-password command."
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

## Use a secret

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
| 1 | berthd unreachable, or it refused the request; the message says why | `berth status`; for "already exists" use the stored secret; for "no stored secret" request it; for "signed berth binary" or "not the signed berthd" the person runs `make build`, starts `bin/berthd`, and you use `bin/berth`; for "is an interpreter" ask the person whether to grant the real tool instead |
| 2 | Usage error | Check the command shape above |
| 3 | The person denied the dialog or Touch ID | Do not retry on your own; ask the person whether they want to |
| 4 | The store or grant failed | Read the message, then ask the person |
| 5 | `run`: no grant lets this executable use the secret with these arguments; nothing started | Ask the person to run the `berth secret grant` line the error prints |
| 6 | `run`: the executable changed since it was granted; nothing started | Ask the person to grant it again with the printed line |

Once the command starts, `berth run` exits with the command's own code.
