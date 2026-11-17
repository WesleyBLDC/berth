#!/bin/sh
# PreToolUse on Bash: refuse commands that would put a secret value into the transcript or shell history.
set -u
cmd=$(jq -r '.tool_input.command // empty')
case "$cmd" in
  *"find-generic-password"*" -w"*|*"find-generic-password"*" -g"*|*"find-internet-password"*" -w"*|*"find-internet-password"*" -g"*)
    echo "Blocked: this prints a Keychain secret into the transcript. Never read a value: berth secret request <name> --why \"<reason>\" stores one, berth run --env KEY=<name> -- <command> uses one." >&2
    exit 2 ;;
  *"add-generic-password"*" -w "?*|*"add-generic-password"*" -w"[!\ ]*)
    echo "Blocked: a secret value on the command line lands in history and the transcript. Use berth secret request <name> --why \"<reason>\" so the person types it into the native dialog." >&2
    exit 2 ;;
esac
exit 0
