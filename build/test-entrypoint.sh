#!/bin/sh
set -eu

entrypoint=${1:-build/wrapper/entrypoint.sh}
tmp=${TMPDIR:-/tmp}/kruise-entrypoint-test-$$
mkdir -p "$tmp/bin" "$tmp/state"
cleanup() {
  if [ -f "$tmp/pid" ]; then
    kill "$(sed -n '1p' "$tmp/pid")" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

cat >"$tmp/bin/kruise-nfs-wrapper" <<'EOF'
#!/bin/sh
touch "$ENTRYPOINT_TEST_STATE/wrapper.started"
trap 'exit 0' TERM INT
while :; do sleep 1; done
EOF
cat >"$tmp/bin/nfsplugin" <<'EOF'
#!/bin/sh
touch "$ENTRYPOINT_TEST_STATE/plugin.started"
exit "${ENTRYPOINT_TEST_PLUGIN_EXIT:-0}"
EOF
chmod 0755 "$tmp/bin/kruise-nfs-wrapper" "$tmp/bin/nfsplugin"

ENTRYPOINT_TEST_STATE="$tmp/state" \
  WRAPPER_BINARY="$tmp/bin/kruise-nfs-wrapper" \
  NFS_PLUGIN_BINARY="$tmp/bin/nfsplugin" \
  WRAPPER_AGENT_ENABLED=true WRAPPER_SOCKET_MODE=0660 KUBE_NODE_NAME=test-node \
  sh "$entrypoint" &
pid=$!
printf '%s\n' "$pid" >"$tmp/pid"

status=0
wait "$pid" || status=$?
[ "$status" -ne 0 ]
[ -f "$tmp/state/wrapper.started" ]
[ -f "$tmp/state/plugin.started" ]

printf '%s\n' "entrypoint supervision smoke passed"
