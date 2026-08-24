#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

CACHE_ROOT="${CACHE_ROOT:-${TMPDIR:-/tmp}/kruise-agents-nfs-csi}"
GO_CACHE="${GO_CACHE:-${CACHE_ROOT}/gocache}"
GO_MOD_CACHE="${GO_MOD_CACHE:-${CACHE_ROOT}/gomodcache}"
BIN_DIR="${BIN_DIR:-dist/linux-amd64}"

mkdir -p "${BIN_DIR}"

gofmt -w ./cmd ./internal ./mounter
GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" go test ./...
GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" go vet ./...

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" \
  go build -trimpath -ldflags="-s -w" -o "${BIN_DIR}/kruise-nfs-wrapper" ./cmd/wrapper
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" \
  go build -trimpath -ldflags="-s -w" -o "${BIN_DIR}/kruise-nfs-mounter" ./cmd/mounter

echo "wrapper binary: ${BIN_DIR}/kruise-nfs-wrapper"
echo "mounter binary: ${BIN_DIR}/kruise-nfs-mounter"
