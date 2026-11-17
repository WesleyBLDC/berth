#!/bin/sh
# Usage: scripts/build-app.sh [--adhoc] [<out>/Berth.app]
# Assembles Berth.app: the SwiftUI app in Contents/MacOS, and berthd, berth, berth-native and
# berth-proxy beside each other in Contents/Helpers, so berthd's pins find them as in bin/ (and
# berth never meets Berth on a case insensitive volume), and the LaunchAgent plist that
# SMAppService registers. Each binary is signed with its own identifier before the bundle is
# signed as com.wesleybldc.berth, never with --deep. --adhoc signs without the team identity,
# for the layout check in make check; only a team signed bundle is trusted at run time.
set -eu
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
root=$(cd "$(dirname "$0")/.." && pwd)
adhoc=0
if [ "${1:-}" = "--adhoc" ]; then adhoc=1; shift; fi
app=${1:-$root/bin/Berth.app}

sign() {
  if [ "$adhoc" = 1 ]; then
    codesign --force --sign - --identifier "$2" --options runtime --timestamp=none "$1"
  else
    "$root/scripts/sign.sh" "$1" "$2"
  fi
}

swift build --package-path "$root/macos" -c release --product Berth
swift build --package-path "$root/native" -c release --product berth-native
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Helpers" "$app/Contents/Resources" "$app/Contents/Library/LaunchAgents"
(cd "$root" && go build -o "$app/Contents/Helpers/" ./cmd/berthd ./cmd/berth ./cmd/berth-proxy)
cp "$root/macos/.build/release/Berth" "$app/Contents/MacOS/Berth"
cp "$root/native/.build/release/berth-native" "$app/Contents/Helpers/berth-native"
cp "$root/macos/Bundle/Info.plist" "$app/Contents/Info.plist"
cp "$root/macos/Bundle/com.wesleybldc.berth.daemon.plist" "$app/Contents/Library/LaunchAgents/"
cp "$root/brand/app-icon/Berth.icns" "$app/Contents/Resources/AppIcon.icns"

sign "$app/Contents/Helpers/berthd" com.wesleybldc.berth.daemon
sign "$app/Contents/Helpers/berth" com.wesleybldc.berth.cli
sign "$app/Contents/Helpers/berth-proxy" com.wesleybldc.berth.proxy
sign "$app/Contents/Helpers/berth-native" com.wesleybldc.berth.native
sign "$app" com.wesleybldc.berth
echo "build-app: $app"
