#!/bin/sh
# Proof 5: open a visible Chrome through the proxy, then capture its window by PID from a signed app.
set -u
spike=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
"$spike/bin/spike-proxy" -headless=false -token-file "$work/token" > "$work/env" 2>/dev/null &
proxy=$!
for _ in $(seq 1 40); do grep -q CHROME_PID "$work/env" 2>/dev/null && break; sleep 0.25; done
. "$work/env"
sleep 2.5
open -W -n --stdout "$work/out.txt" --stderr "$work/err.txt" "$spike/bin/Berth.app" --args "$CHROME_PID" "$work/window.png"
cat "$work/out.txt" "$work/err.txt"
kill -TERM $proxy; wait $proxy 2>/dev/null
grep -q "^captured" "$work/out.txt" && echo "PNG: $work/window.png" && exit 0
exit 1
