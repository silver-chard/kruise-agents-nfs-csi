#!/bin/sh
set -eu

: "${DRIVER_NAME:=csi.nfs.zhida}"
: "${CSI_ENDPOINT:=unix:///csi/csi.sock}"
: "${WRAPPER_SOCKET_PATH:=/var/lib/kruise-agents-nfs-csi/wrapper.sock}"
: "${WRAPPER_STAGING_ROOT:=/var/lib/kruise-agents-nfs-csi/staging}"
: "${WRAPPER_UNSTAGE_AFTER_MOUNT:=true}"
: "${WRAPPER_AGENT_ENABLED:=true}"

wrapper_pid=""

if [ "${WRAPPER_AGENT_ENABLED}" = "true" ]; then
  /usr/local/bin/kruise-nfs-wrapper \
    --driver-name="${DRIVER_NAME}" \
    --socket-path="${WRAPPER_SOCKET_PATH}" \
    --staging-root="${WRAPPER_STAGING_ROOT}" &
  wrapper_pid="$!"
fi

shutdown() {
  if [ -n "${wrapper_pid}" ]; then
    kill "${wrapper_pid}" 2>/dev/null || true
    wait "${wrapper_pid}" 2>/dev/null || true
  fi
}
trap shutdown TERM INT

if [ "$#" -eq 0 ]; then
  node_id="${KUBE_NODE_NAME:-${NODE_ID:-}}"
  if [ -z "${node_id}" ]; then
    echo "KUBE_NODE_NAME or NODE_ID is required" >&2
    exit 1
  fi
  set -- /nfsplugin --endpoint="${CSI_ENDPOINT}" --nodeid="${node_id}" --drivername="${DRIVER_NAME}"
elif [ "${1#-}" != "$1" ]; then
  set -- /nfsplugin "$@"
fi

exec "$@"
