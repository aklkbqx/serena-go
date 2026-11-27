#!/usr/bin/env bash
# Local multi-arch release builder & optional GitHub Release publisher (via gh CLI).
# Usage: ./scripts/release.sh --version v0.1.0 [--publish]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION=""
PUBLISH=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --publish) PUBLISH=1; shift ;;
    *) echo "Usage: $0 --version vX.Y.Z [--publish]" >&2; exit 2 ;;
  esac
done

if [[ -z "$VERSION" ]]; then
  echo "Error: --version is required (e.g. v0.1.0)" >&2
  exit 2
fi

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo local)"
LDFLAGS="-s -w -X main.buildVersion=${VERSION} -X main.buildCommit=${COMMIT}"
DIST_DIR="${ROOT}/dist/${VERSION}"
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  STAGE="$(mktemp -d)"
  echo "[release] Building serena-go ${VERSION} (${GOOS}/${GOARCH})..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags="$LDFLAGS" -o "${STAGE}/serena-go" .
  cp LICENSE README.md "${STAGE}/"
  ARCHIVE="serena-go_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  tar -C "$STAGE" -czf "${DIST_DIR}/${ARCHIVE}" serena-go LICENSE README.md
  rm -rf "$STAGE"
done

(
  cd "$DIST_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum *.tar.gz > SHA256SUMS.txt
  else
    shasum -a 256 *.tar.gz > SHA256SUMS.txt
  fi
)

echo "[release] Artifacts ready in ${DIST_DIR}:"
ls -lh "$DIST_DIR"
cat "${DIST_DIR}/SHA256SUMS.txt"

if [[ "$PUBLISH" -eq 1 ]]; then
  echo "[release] Publishing GitHub Release ${VERSION} via gh..."
  gh release create "$VERSION" "${DIST_DIR}"/*.tar.gz "${DIST_DIR}/SHA256SUMS.txt" \
    --title "serena-go ${VERSION}" \
    --generate-notes
fi
