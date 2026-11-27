#!/usr/bin/env bash
# Local pre-publish verification & OSS hygiene gate (no cloud CI required).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "=== 1. Checking forbidden directories (.github) ==="
if [[ -d ".github" ]]; then
  echo "FAIL: .github directory is forbidden in this repository" >&2
  exit 1
fi

echo "=== 2. Scanning for leaked personal paths, private LAN IPs, or secrets ==="
LEAK_PATTERN='(/Users/[a-zA-Z0-9._-]+/|/Volumes/|192\.168\.[0-9]+\.|10\.[0-9]+\.[0-9]+\.|AKIA[0-9A-Z]{16}|ghp_[a-zA-Z0-9]{20,}|sk-[a-zA-Z0-9]{20,}|BEGIN (RSA|OPENSSH|EC|PRIVATE) KEY)'
if grep -rnE --exclude-dir=.git --exclude-dir=dist --exclude="check-oss-hygiene.sh" "$LEAK_PATTERN" .; then
  echo "FAIL: Personal path, private IP, or secret pattern detected above!" >&2
  exit 1
fi
echo "OK: zero leaked paths, private IPs, or secrets"

echo "=== 3. Running go vet & go test -race ==="
go vet ./...
go test -race -count=1 ./...

echo "=== 4. Verifying CGO_ENABLED=0 cross-compilation matrix ==="
TMP_OUT="$(mktemp -d)"
trap 'rm -rf "$TMP_OUT"' EXIT
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  echo "  building ${GOOS}/${GOARCH}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags="-s -w" -o "${TMP_OUT}/serena-go-${GOOS}-${GOARCH}" .
done

echo "ALL OSS HYGIENE & BUILD CHECKS PASSED."
