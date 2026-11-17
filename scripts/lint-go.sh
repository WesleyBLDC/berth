#!/bin/sh
# Lints every Go module in the repository; generated code is excluded by .golangci.yml.
set -eu
root=$(git rev-parse --show-toplevel)
status=0
for mod in $(find "$root" -name go.mod -not -path '*/.build/*' -not -path '*/spike/*' -exec dirname {} \;); do
  (cd "$mod" && golangci-lint fmt --diff --config "$root/.golangci.yml" ./... && golangci-lint run --config "$root/.golangci.yml" ./...) || status=1
done
exit $status
