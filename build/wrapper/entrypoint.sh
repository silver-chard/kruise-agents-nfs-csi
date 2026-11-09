#!/bin/sh
set -eu

: "${DRIVER_NAME:=csi.nfs.zhida}"
: "${CSI_ENDPOINT:=unix:///csi/csi.sock}"
: "${WRAPPER_SOCKET_PATH:=/var/lib/kruise-agents-nfs-csi/wrapper.sock}"
: "${WRAPPER_SOCKET_MODE:=0660}"
: "${WRAPPER_STAGING_ROOT:=/var/lib/kruise-agents-nfs-csi/staging}"
: "${WRAPPER_MOUNT_STATE_DIR:=/var/lib/kruise-agents-nfs-csi/mounts}"
: "${WRAPPER_UNSTAGE_AFTER_MOUNT:=true}"
: "${WRAPPER_ENABLE_MOUNT:=true}"
: "${WRAPPER_AGENT_ENABLED:=true}"
: "${WRAPPER_JUICEFS_CSI_SOCKET:=}"
: "${WRAPPER_JUICEFS_PUBLISH_ROOT:=/var/lib/kruise-agents-nfs-csi/juicefs-publish}"
: "${WRAPPER_JUICEFS_STATE_DIR:=/var/lib/kruise-agents-nfs-csi/juicefs-state}"
: "${WRAPPER_JUICEFS_SECRET_REFS:=}"
: "${WRAPPER_BINARY:=/usr/local/bin/kruise-nfs-wrapper}"
: "${NFS_PLUGIN_BINARY:=/nfsplugin}"

wrapper_pid=""
plugin_pid=""
terminating=false

if [ "${WRAPPER_AGENT_ENABLED}" = "true" ]; then
  "${WRAPPER_BINARY}" \
    --driver-name="${DRIVER_NAME}" \
    --socket-path="${WRAPPER_SOCKET_PATH}" \
    --staging-root="${WRAPPER_STAGING_ROOT}" \
    --mount-state-dir="${WRAPPER_MOUNT_STATE_DIR}" \
    --unstage-after-mount="${WRAPPER_UNSTAGE_AFTER_MOUNT}" \
    --enable-mount="${WRAPPER_ENABLE_MOUNT}" \
    --juicefs-csi-socket="${WRAPPER_JUICEFS_CSI_SOCKET}" \
    --juicefs-publish-root="${WRAPPER_JUICEFS_PUBLISH_ROOT}" \
    --juicefs-state-dir="${WRAPPER_JUICEFS_STATE_DIR}" \
    --juicefs-secret-refs="${WRAPPER_JUICEFS_SECRET_REFS}" &
  wrapper_pid="$!"
fi

shutdown() {
  terminating=true
  if [ -n "${plugin_pid}" ]; then
    kill "${plugin_pid}" 2>/dev/null || true
  fi
  if [ -n "${wrapper_pid}" ]; then
    kill "${wrapper_pid}" 2>/dev/null || true
  fi
  if [ -n "${plugin_pid}" ]; then
    wait "${plugin_pid}" 2>/dev/null || true
  fi
  if [ -n "${wrapper_pid}" ]; then
    wait "${wrapper_pid}" 2>/dev/null || true
  fi
}
handle_signal() {
  shutdown
  exit 0
}
trap handle_signal TERM INT

if [ "$#" -eq 0 ]; then
  node_id="${KUBE_NODE_NAME:-${NODE_ID:-}}"
  if [ -z "${node_id}" ]; then
    echo "KUBE_NODE_NAME or NODE_ID is required" >&2
    exit 1
  fi
  set -- "${NFS_PLUGIN_BINARY}" --endpoint="${CSI_ENDPOINT}" --nodeid="${node_id}" --drivername="${DRIVER_NAME}"
elif [ "${1#-}" != "$1" ]; then
  set -- "${NFS_PLUGIN_BINARY}" "$@"
fi

if [ -z "${wrapper_pid}" ]; then
  exec "$@"
fi

"$@" &
plugin_pid="$!"

# BusyBox/POSIX sh has no wait -n. Poll child liveness and exit the container
# when either critical process dies, so Kubernetes can recreate the Pod.
while kill -0 "${wrapper_pid}" 2>/dev/null && kill -0 "${plugin_pid}" 2>/dev/null; do
  sleep 1
done
if [ "${terminating}" != "true" ]; then
  shutdown
fi
exit 1
