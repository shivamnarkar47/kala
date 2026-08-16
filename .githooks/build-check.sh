#!/usr/bin/env bash
# Build-check hook (post-burn, Go-only): gofmt, vet, full test suite, and a
# version probe on the built binary. Shared by pre-commit and pre-push.
# Skip everything with KAAL_SKIP_HOOKS=1; skip with a warning if go is absent.
set -euo pipefail

if [ "${KAAL_SKIP_HOOKS:-0}" = "1" ]; then
  echo "hooks: skipped (KAAL_SKIP_HOOKS=1)"
  exit 0
fi

if ! command -v go >/dev/null 2>&1; then
  echo "hooks: WARNING go not on PATH — skipping build check (commit proceeds)"
  exit 0
fi

cd "$(dirname "$0")/.."

# 1. Formatting gate
if [ -n "$(gofmt -l .)" ]; then
  echo "hooks: gofmt FAILED — run gofmt -w on:" >&2
  gofmt -l . >&2
  exit 1
fi

# 2. Vet + full test suite (race detector)
go vet ./...
go test -race -count=1 ./...

# 3. The binary builds and reports the right version
go build -o /tmp/kaal-hook-check ./cmd/kaal
test "$(/tmp/kaal-hook-check --version)" = "kaal 0.3"
rm -f /tmp/kaal-hook-check

echo "hooks: build check passed"
