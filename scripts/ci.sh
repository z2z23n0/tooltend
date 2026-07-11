#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO_BIN="${GO_BIN:-go}"
GOFMT_BIN="${GOFMT_BIN:-gofmt}"

unformatted="$({ find . -type f -name '*.go' -not -path './.git/*' -print0 | xargs -0 "$GOFMT_BIN" -l; } || true)"
if [[ -n "$unformatted" ]]; then
  echo "The following Go files are not formatted:" >&2
  echo "$unformatted" >&2
  exit 1
fi

"$GO_BIN" mod tidy -diff
"$GO_BIN" vet ./...
"$GO_BIN" test ./...
"$GO_BIN" build ./...
