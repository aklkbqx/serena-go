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

BUILD_TAGS="grammar_subset,grammar_subset_go,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_python,grammar_subset_rust,grammar_subset_swift,grammar_subset_c,grammar_subset_cpp,grammar_subset_objc,grammar_subset_java,grammar_subset_kotlin,grammar_subset_sql,grammar_subset_bash,grammar_subset_ruby,grammar_subset_php,grammar_subset_vue,grammar_subset_svelte,grammar_subset_json,grammar_subset_yaml,grammar_subset_toml,grammar_subset_markdown,grammar_subset_css,grammar_subset_html,grammar_subset_dockerfile"

echo "=== 3. Running go vet & go test -race (with grammar_subset tags) ==="
go vet -tags "$BUILD_TAGS" ./...
go test -race -tags "$BUILD_TAGS" -count=1 ./...

echo "=== 4. Verifying CGO_ENABLED=0 cross-compilation matrix (grammar_subset) ==="
TMP_OUT="$(mktemp -d)"
trap 'rm -rf "$TMP_OUT"' EXIT
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  echo "  building ${GOOS}/${GOARCH}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -tags "$BUILD_TAGS" -ldflags="-s -w" -o "${TMP_OUT}/serena-go-${GOOS}-${GOARCH}" .
done

echo "ALL OSS HYGIENE & BUILD CHECKS PASSED."
