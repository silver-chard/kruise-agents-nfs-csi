#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

REGISTRY="${REGISTRY:-iregistry.baidu-int.com/cnap-cluster}"
VERSION="${VERSION:-0.0.1-beta.12}"
BASE_IMAGE="${BASE_IMAGE:-iregistry.baidu-int.com/baidu-base/ubuntu:resolute}"
UPSTREAM_NFS_CSI_IMAGE="${UPSTREAM_NFS_CSI_IMAGE:-iregistry.baidu-int.com/cnap-cluster/nfsplugin:v4.13.2}"
GO_CACHE="${GO_CACHE:-/private/tmp/kruise-agents-nfs-csi-gocache}"
GO_MOD_CACHE="${GO_MOD_CACHE:-/private/tmp/kruise-agents-nfs-csi-gomodcache}"
BIN_DIR="${BIN_DIR:-dist/linux-amd64}"
PUSH="${PUSH:-0}"

WRAPPER_IMAGE="${WRAPPER_IMAGE:-${REGISTRY}/kruise-agents-nfs-csi-wrapper:${VERSION}}"
MOUNTER_IMAGE="${MOUNTER_IMAGE:-${REGISTRY}/kruise-agents-nfs-csi-mounter:${VERSION}}"

mkdir -p "${BIN_DIR}"

gofmt -w ./cmd ./internal
GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" go test ./...
GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" go vet ./...

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" \
  go build -trimpath -ldflags="-s -w" -o "${BIN_DIR}/kruise-nfs-wrapper" ./cmd/wrapper
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE="${GO_CACHE}" GOMODCACHE="${GO_MOD_CACHE}" \
  go build -trimpath -ldflags="-s -w" -o "${BIN_DIR}/kruise-nfs-mounter" ./cmd/mounter

docker buildx build --platform linux/amd64 --load \
  -f Dockerfile.wrapper \
  --build-arg "UPSTREAM_NFS_CSI_IMAGE=${UPSTREAM_NFS_CSI_IMAGE}" \
  --build-arg "BASE_IMAGE=${BASE_IMAGE}" \
  -t "${WRAPPER_IMAGE}" .

docker buildx build --platform linux/amd64 --load \
  -f Dockerfile.mounter \
  --build-arg "BASE_IMAGE=${BASE_IMAGE}" \
  -t "${MOUNTER_IMAGE}" .

if [[ "${PUSH}" == "1" ]]; then
  docker push "${WRAPPER_IMAGE}"
  docker push "${MOUNTER_IMAGE}"
fi

echo "wrapper image: ${WRAPPER_IMAGE}"
echo "mounter image: ${MOUNTER_IMAGE}"
