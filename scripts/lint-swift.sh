#!/bin/sh
# swift-format ships with the Xcode toolchain; generated code is skipped.
set -eu
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
files=""
for f in "$@"; do
  case "$f" in */SpikeProto/* | *.pb.swift | *.grpc.swift) ;; *) files="$files $f" ;; esac
done
[ -z "$files" ] && exit 0
# shellcheck disable=SC2086
xcrun swift format lint --strict $files
