#!/usr/bin/env bash
set -euo pipefail

REPOSITORY="${TOOLTEND_REPOSITORY:-z2z23n0/tooltend}"
INSTALL_DIR="${TOOLTEND_INSTALL_DIR:-$HOME/.local/bin}"
MANIFEST_URL="${TOOLTEND_MANIFEST_URL:-https://github.com/$REPOSITORY/releases/latest/download/tooltend-manifest.json}"

case "$(uname -s)" in
  Darwin) os_name="darwin" ;;
  Linux) os_name="linux" ;;
  *) echo "ToolTend does not publish binaries for $(uname -s)." >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) echo "ToolTend does not publish binaries for $(uname -m)." >&2; exit 1 ;;
esac

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tooltend-install.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT

manifest="$tmp_dir/tooltend-manifest.json"
curl -fsSL --proto '=https' --tlsv1.2 "$MANIFEST_URL" -o "$manifest"

asset_record="$(tr '}' '\n' < "$manifest" | sed -n "s#.*\"os\":\"$os_name\",\"arch\":\"$arch\",\"url\":\"\([^\"]*\)\",\"sha256\":\"\([0-9a-fA-F]*\)\",\"size\":\([0-9]*\).*#\1 \2 \3#p" | head -n 1)"
if [[ -z "$asset_record" ]]; then
  echo "The signed release manifest has no asset for $os_name/$arch." >&2
  exit 1
fi

read -r asset_url expected_sha expected_size <<< "$asset_record"
case "$asset_url" in
  https://github.com/*) ;;
  *) echo "The release manifest selected a non-GitHub HTTPS asset." >&2; exit 1 ;;
esac

binary="$tmp_dir/tooltend"
curl -fsSL --proto '=https' --tlsv1.2 "$asset_url" -o "$binary"
actual_size="$(wc -c < "$binary" | tr -d '[:space:]')"
if [[ "$actual_size" != "$expected_size" ]]; then
  echo "Downloaded ToolTend size does not match the release manifest." >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual_sha="$(sha256sum "$binary" | awk '{print $1}')"
else
  actual_sha="$(shasum -a 256 "$binary" | awk '{print $1}')"
fi
actual_sha="$(printf '%s' "$actual_sha" | tr '[:upper:]' '[:lower:]')"
expected_sha="$(printf '%s' "$expected_sha" | tr '[:upper:]' '[:lower:]')"
if [[ "$actual_sha" != "$expected_sha" ]]; then
  echo "Downloaded ToolTend SHA-256 does not match the release manifest." >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
chmod 0755 "$binary"
target="$INSTALL_DIR/tooltend"
if [[ -f "$target" && ! -L "$target" ]]; then
  cp -p "$target" "$target.previous"
fi
mv -f "$binary" "$target"

echo "Installed tooltend to $target"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Add $INSTALL_DIR to PATH before running tooltend." ;;
esac
