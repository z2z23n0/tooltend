#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_DIR="${TOOLTEND_INSTALL_DIR:-$HOME/.local/bin}"
GO_BIN="${GO_BIN:-go}"

mkdir -p "$INSTALL_DIR"
tmp="$(mktemp "$INSTALL_DIR/.tooltend.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

version="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || printf 'dev')"
commit="$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')"
date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
ldflags="-s -w -X github.com/z2z23n0/tooltend/internal/buildinfo.Version=$version -X github.com/z2z23n0/tooltend/internal/buildinfo.Commit=$commit -X github.com/z2z23n0/tooltend/internal/buildinfo.Date=$date"

(
  cd "$ROOT"
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$tmp" ./cmd/tooltend
)
chmod 0755 "$tmp"
mv -f "$tmp" "$INSTALL_DIR/tooltend"
trap - EXIT

echo "Installed tooltend to $INSTALL_DIR/tooltend"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Add $INSTALL_DIR to PATH before running tooltend." ;;
esac
