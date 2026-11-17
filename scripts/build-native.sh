#!/bin/sh
# Builds berth-native in release and signs it with the Personal Team's Apple Development
# identity. berthd trusts the helper by team, identifier and cdhash, and Keychain items trust
# it by its designated requirement, so every build must carry the same team and identifier.
set -eu
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/bin/berth-native"

swift build --package-path "$root/native" -c release --product berth-native
mkdir -p "$root/bin"
cp "$root/native/.build/release/berth-native" "$out"
"$root/scripts/sign.sh" "$out" com.wesleybldc.berth.native
