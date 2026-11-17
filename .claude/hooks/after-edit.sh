#!/bin/sh
# PostToolUse: format the edited file, then lint it. Exit 2 sends the problems back to Claude.
set -u
file=$(jq -r '.tool_input.file_path // empty')
[ -n "$file" ] && [ -f "$file" ] || exit 0
root=$(git -C "$(dirname "$file")" rev-parse --show-toplevel 2>/dev/null) || exit 0
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"

nearest() { d=$(dirname "$1"); while [ "$d" != "/" ] && [ ! -f "$d/$2" ]; do d=$(dirname "$d"); done; [ -f "$d/$2" ] && echo "$d"; }
report() { out=$("$@" 2>&1) || { echo "$out" | head -40 >&2; exit 2; }; }

case "$file" in
  */gen/*|*.pb.go|*.pb.swift|*.grpc.swift|*.connect.go) exit 0 ;;
  *.go)
    mod=$(nearest "$file" go.mod) || exit 0
    cd "$mod" || exit 0
    golangci-lint fmt --config "$root/.golangci.yml" "$file" >/dev/null 2>&1
    pkg="./$(dirname "${file#"$mod"/}")"
    report go vet "$pkg"
    report golangci-lint run --config "$root/.golangci.yml" "$pkg"
    ;;
  *.swift)
    xcrun swift format -i "$file" >/dev/null 2>&1
    report xcrun swift format lint --strict "$file"
    ;;
  *.proto)
    mod=$(nearest "$file" buf.yaml) || exit 0
    cd "$mod" || exit 0
    buf format -w "$file" >/dev/null 2>&1
    report buf lint
    ;;
  *.py)
    uvx ruff format -q "$file" >/dev/null 2>&1
    report uvx ruff check --config "$root/ruff.toml" "$file"
    ;;
  *.sh)
    report shellcheck "$file"
    ;;
  *.md)
    if grep -n "$(printf '\342\200\224\|\342\200\223')" "$file" >&2; then
      echo "Dashes used as punctuation in $file; use a comma, colon, parentheses or a new sentence." >&2
      exit 2
    fi
    ;;
esac
exit 0
