#!/usr/bin/env bash
# tunnel-up.sh — Ensure an SSH tunnel is open for Kubernetes API access.
#
# Forwards local 127.0.0.1:16443 → remote 127.0.0.1:6443 (k3s API server).
# Port 6443 on the server is NOT publicly exposed; this tunnel is the only
# safe way to reach it from a developer machine.
#
# Usage:
#   scripts/tunnel-up.sh          # start tunnel if not already running
#   scripts/tunnel-up.sh --status # just print tunnel status and exit
#
# The SSH password is NOT stored here. Set SSHPASS in your environment
# or install your public key on the server for passwordless operation.
# See docs/DEMO_RUNBOOK.md for one-time setup steps.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=demo.env
source "${SCRIPT_DIR}/demo.env"

PID_FILE="/tmp/iiot-tunnel.pid"
LOG_FILE="/tmp/iiot-tunnel.log"

tunnel_running() {
  if [[ -f "${PID_FILE}" ]]; then
    local pid
    pid=$(cat "${PID_FILE}")
    if kill -0 "${pid}" 2>/dev/null; then
      return 0
    fi
    rm -f "${PID_FILE}"
  fi
  # Also check if port is actually listening (catches tunnels started externally)
  nc -z 127.0.0.1 "${TUNNEL_LOCAL_PORT}" 2>/dev/null
}

if [[ "${1:-}" == "--status" ]]; then
  if tunnel_running; then
    pid=$(cat "${PID_FILE}" 2>/dev/null || echo "external")
    echo "TUNNEL UP  — 127.0.0.1:${TUNNEL_LOCAL_PORT} → ${SERVER_IP}:6443  (pid ${pid})"
  else
    echo "TUNNEL DOWN"
  fi
  exit 0
fi

if tunnel_running; then
  pid=$(cat "${PID_FILE}" 2>/dev/null || echo "external")
  echo "[tunnel-up] already running (pid ${pid}) — nothing to do"
  exit 0
fi

echo "[tunnel-up] starting SSH tunnel: 127.0.0.1:${TUNNEL_LOCAL_PORT} → ${SERVER_IP}:6443"

# Use sshpass if SSHPASS is set in env; otherwise rely on SSH key auth.
if [[ -n "${SSHPASS:-}" ]]; then
  SSH_CMD="sshpass -e ssh"
else
  SSH_CMD="ssh"
fi

if [[ -n "${SSHPASS:-}" ]]; then
  AUTH_ARGS=(-o PreferredAuthentications=password -o PubkeyAuthentication=no)
else
  AUTH_ARGS=(-o PreferredAuthentications=publickey)
fi

${SSH_CMD} \
  -N \
  -L "${TUNNEL_LOCAL_PORT}:127.0.0.1:6443" \
  -o StrictHostKeyChecking=no \
  -o ConnectTimeout=15 \
  -o ServerAliveInterval=15 \
  -o ServerAliveCountMax=6 \
  -o ExitOnForwardFailure=yes \
  "${AUTH_ARGS[@]}" \
  -p "${SSH_PORT}" \
  "${SSH_USER}@${SERVER_IP}" \
  >"${LOG_FILE}" 2>&1 &

echo $! > "${PID_FILE}"

# Wait for port to open (up to 10s)
for i in $(seq 1 20); do
  if nc -z 127.0.0.1 "${TUNNEL_LOCAL_PORT}" 2>/dev/null; then
    echo "[tunnel-up] port ${TUNNEL_LOCAL_PORT} open after ${i} attempts — tunnel ready"
    exit 0
  fi
  sleep 0.5
done

echo "[tunnel-up] ERROR: port ${TUNNEL_LOCAL_PORT} did not open in 10s" >&2
echo "[tunnel-up] check logs: ${LOG_FILE}" >&2
cat "${LOG_FILE}" >&2
rm -f "${PID_FILE}"
exit 1
