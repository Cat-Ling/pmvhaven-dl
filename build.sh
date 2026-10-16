#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if ! command -v go >/dev/null 2>&1; then
    echo "Error: 'go' toolchain is not installed or not in PATH." >&2
    exit 1
fi

OUTPUT_BIN="pmvhaven-dl"
LDFLAGS="-s -w"
if [[ -n "${VERSION:-}" ]]; then
    LDFLAGS="${LDFLAGS} -X main.version=${VERSION}"
fi

echo "Building ${OUTPUT_BIN}..."

CGO_ENABLED=0 go build -ldflags="${LDFLAGS}" -trimpath -o "$OUTPUT_BIN" main.go

if [[ ! -x "$OUTPUT_BIN" ]]; then
    echo "Error: Build finished but executable '${OUTPUT_BIN}' was not produced." >&2
    exit 1
fi

echo "Build complete: ${SCRIPT_DIR}/${OUTPUT_BIN}"
