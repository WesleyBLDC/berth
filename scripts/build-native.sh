#!/bin/sh
# Builds berth-native in release and signs it with the Personal Team's Apple Development
# identity. berthd trusts the helper by team ID and Keychain items trust it by its designated
# requirement, so every build must carry the same team and identifier.
set -eu
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
team=XPJ7XA42J4
identifier=com.wesleybldc.berth.native
root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/bin/berth-native"

# The certificate's display name carries the person's ID, not the team; the team is the
# subject's OU, so pick the identity by that.
identity=""
for hash in $(security find-identity -v -p codesigning | awk '/Apple Development/ { print $2 }'); do
  if security find-certificate -a -Z -p | awk -v h="$hash" '
      /^SHA-1 hash:/ { keep = ($3 == h) }
      keep && /BEGIN CERTIFICATE/ { p = 1 }
      p { print }
      /END CERTIFICATE/ { if (p) exit; }' |
    openssl x509 -noout -subject 2>/dev/null | grep -q "OU *= *$team"; then
    identity=$hash
    break
  fi
done
if [ -z "$identity" ]; then
  echo "build-native: no valid Apple Development identity for team $team" >&2
  exit 1
fi

swift build --package-path "$root/native" -c release --product berth-native
mkdir -p "$root/bin"
cp "$root/native/.build/release/berth-native" "$out"
codesign --force --sign "$identity" --identifier "$identifier" --options runtime --timestamp=none "$out"

codesign --verify --strict "$out"
details=$(codesign -dv "$out" 2>&1)
echo "$details" | grep -qx "TeamIdentifier=$team" || { echo "build-native: wrong team in $out" >&2; exit 1; }
echo "$details" | grep -qx "Identifier=$identifier" || { echo "build-native: wrong identifier in $out" >&2; exit 1; }
echo "$details" | grep -q "flags=.*runtime" || { echo "build-native: hardened runtime missing" >&2; exit 1; }
echo "build-native: $out signed by team $team as $identifier"
