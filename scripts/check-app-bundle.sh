#!/bin/sh
# Builds Berth.app signed ad hoc in a temporary folder and checks its layout, its plists, every
# signature and identifier, that it holds no symlink, and that the app links no Keychain code.
# With --check <Berth.app> it checks that bundle instead of building one. Opens nothing.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
if [ "${1:-}" = --check ]; then
  app=$2
else
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  app="$tmp/Berth.app"
  "$root/scripts/build-app.sh" --adhoc "$app" >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log"; exit 1; }
fi

fail() { echo "check-app-bundle: $*" >&2; exit 1; }
plist() { /usr/libexec/PlistBuddy -c "Print :$2" "$1" 2>/dev/null || true; }

info="$app/Contents/Info.plist"
agent="$app/Contents/Library/LaunchAgents/com.wesleybldc.berth.daemon.plist"
plutil -lint -s "$info" "$agent" || fail "a plist does not parse"
[ "$(plist "$info" CFBundleIdentifier)" = com.wesleybldc.berth ] || fail "wrong bundle id"
[ "$(plist "$info" CFBundleExecutable)" = Berth ] || fail "wrong executable"
[ "$(plist "$info" LSUIElement)" = true ] || fail "not an accessory app"
[ "$(plist "$info" LSMinimumSystemVersion)" = 26.0 ] || fail "wrong minimum system"
[ "$(plist "$agent" Label)" = com.wesleybldc.berth.daemon ] || fail "wrong LaunchAgent label"
[ "$(plist "$agent" BundleProgram)" = Contents/Helpers/berthd ] || fail "the LaunchAgent does not run the bundled berthd"
[ "$(plist "$agent" AssociatedBundleIdentifiers:0)" = com.wesleybldc.berth ] || fail "the LaunchAgent is not tied to the app"
[ -f "$app/Contents/Resources/AppIcon.icns" ] || fail "no icon"
# berthd's pin copies only folders and regular files, so a symlink would make the app untrusted.
[ -z "$(find "$app" -type l)" ] || fail "the bundle holds a symlink: $(find "$app" -type l | head -n 1)"

codesign --verify --strict --deep "$app" || fail "the bundle signature does not verify"
for pair in Berth:com.wesleybldc.berth berthd:com.wesleybldc.berth.daemon berth:com.wesleybldc.berth.cli \
  berth-proxy:com.wesleybldc.berth.proxy berth-native:com.wesleybldc.berth.native; do
  name=${pair%%:*}
  dir=Helpers
  [ "$name" = Berth ] && dir=MacOS
  file=$app/Contents/$dir/$name
  details=$(codesign -dv "$file" 2>&1)
  echo "$details" | grep -qx "Identifier=${pair#*:}" || fail "$file is not signed as ${pair#*:}"
  echo "$details" | grep -q "flags=.*runtime" || fail "$file lacks the hardened runtime"
done

# No entitlement may widen what the hardened runtime allows, and test tools stay out of the bundle.
for file in "$app" "$app"/Contents/Helpers/*; do
  [ -z "$(codesign -d --entitlements - "$file" 2>/dev/null)" ] || fail "$file carries entitlements"
done
[ -z "$(find "$app" -name 'berth-app-probe*')" ] || fail "berth-app-probe is in the bundle"

# Berth.app holds no secret: no Keychain item, keychain or access call may be linked into it.
if nm -u "$app/Contents/MacOS/Berth" | grep -E '_Sec(Item|Keychain|Access)'; then
  fail "Berth links Keychain code"
fi
echo "check-app-bundle: ok"
