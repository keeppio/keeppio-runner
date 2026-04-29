#!/usr/bin/env bash
# Builds static/app.css from frontend/app.css using the Tailwind v4 standalone CLI.
#
# The CLI is downloaded into bin/ on first run (kept out of the image to
# avoid bloating the runtime container — production CSS is built in the
# Docker frontend stage). For local dev: run before `go run .` whenever
# you change templates/ or frontend/app.css.
#
# Usage:
#   scripts/build-frontend.sh           # one-shot build (minified)
#   scripts/build-frontend.sh --watch   # watch mode for development
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TAILWIND_VERSION="${TAILWIND_VERSION:-v4.2.4}"
BIN_DIR="$REPO_ROOT/bin"
TW_BIN="$BIN_DIR/tailwindcss"

# Detect platform — only used locally; Docker has its own download step.
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  asset="tailwindcss-macos-arm64" ;;
  Darwin-x86_64) asset="tailwindcss-macos-x64" ;;
  Linux-aarch64) asset="tailwindcss-linux-arm64" ;;
  Linux-x86_64)  asset="tailwindcss-linux-x64" ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

if [[ ! -x "$TW_BIN" ]]; then
  echo "→ downloading tailwindcss $TAILWIND_VERSION ($asset)" >&2
  mkdir -p "$BIN_DIR"
  curl -fsSL -o "$TW_BIN" \
    "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/${asset}"
  chmod +x "$TW_BIN"
fi

mode="${1:-build}"
case "$mode" in
  --watch|-w|watch)
    exec "$TW_BIN" -i frontend/app.css -o static/app.css --watch
    ;;
  *)
    "$TW_BIN" -i frontend/app.css -o static/app.css --minify
    echo "→ static/app.css built ($(wc -c < static/app.css | tr -d ' ') bytes)"
    ;;
esac
