#!/bin/sh
# PreToolUse on Bash: refuse commands that would put a secret value into the transcript or shell history.
set -u
cmd=$(jq -r '.tool_input.command // empty')
case "$cmd" in
  *"find-generic-password"*" -w"*|*"find-internet-password"*" -w"*)
    echo "Blocked: this prints a Keychain secret into the transcript. Check presence only (drop -w), or use the berth secret commands once they exist." >&2
    exit 2 ;;
  *"add-generic-password"*" -w "?*)
    echo "Blocked: a secret value on the command line lands in history and the transcript. End the command with -w and no value so Keychain prompts for it." >&2
    exit 2 ;;
esac
exit 0
