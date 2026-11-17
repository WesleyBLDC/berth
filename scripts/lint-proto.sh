#!/bin/sh
# Checks formatting and lint for every buf module.
set -eu
root=$(git rev-parse --show-toplevel)
status=0
for mod in $(find "$root" -name buf.yaml -not -path '*/.build/*' -not -path '*/spike/*' -exec dirname {} \;); do
  (cd "$mod" && buf format --diff --exit-code && buf lint) || status=1
done
exit $status
