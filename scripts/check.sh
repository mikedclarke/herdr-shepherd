#!/bin/sh
# The pre-release gate: formatting, vet, and the race-enabled test run. This
# repository has no CI by design, so this script is the contract — run it before
# opening a pull request and before cutting a release.
set -e
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
  echo "herdr-shepherd: Go toolchain not found; the checks need one" >&2
  exit 1
fi

echo "==> gofmt" >&2
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "herdr-shepherd: these files need gofmt:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "==> go vet" >&2
go vet ./...

echo "==> go test -race" >&2
go test -race ./...
