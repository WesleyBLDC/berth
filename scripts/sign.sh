#!/bin/sh
# Usage: scripts/sign.sh <file> <identifier>
# Signs a Berth binary with the Personal Team's Apple Development identity and the hardened
# runtime, so no debugger or injected library can attach to it. berthd trusts the berth CLI
# and berth-native only by this team, identifier and the cdhash of the signed file.
set -eu
[ $# -eq 2 ] || {
  echo "usage: scripts/sign.sh <file> <identifier>" >&2
  exit 2
}
file=$1
identifier=$2
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
team=XPJ7XA42J4

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
  echo "sign: no valid Apple Development identity for team $team; $file stays unsigned and berthd will not trust it" >&2
  exit 1
fi

codesign --force --sign "$identity" --identifier "$identifier" --options runtime --timestamp=none "$file"
codesign --verify --strict "$file"
details=$(codesign -dv "$file" 2>&1)
echo "$details" | grep -qx "TeamIdentifier=$team" || {
  echo "sign: wrong team in $file" >&2
  exit 1
}
echo "$details" | grep -qx "Identifier=$identifier" || {
  echo "sign: wrong identifier in $file" >&2
  exit 1
}
echo "$details" | grep -q "flags=.*runtime" || {
  echo "sign: hardened runtime missing in $file" >&2
  exit 1
}
echo "sign: $file signed by team $team as $identifier"
