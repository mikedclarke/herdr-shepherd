#!/bin/sh
# Build the shepherd binary at install time. Requires a Go toolchain for now;
# a prebuilt-release fallback will land once releases are published.
set -e
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
  echo "herdr-shepherd: Go toolchain not found; install Go and retry" >&2
  exit 1
fi
mkdir -p bin
go build -o bin/herdr-shepherd .
