#!/bin/sh
# Format check and lint for the buf module.
set -eu
cd "$(git rev-parse --show-toplevel)"
buf format --diff --exit-code
buf lint
