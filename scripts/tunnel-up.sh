#!/usr/bin/env bash
# tunnel-up.sh — Ensure an SSH tunnel is open for Kubernetes API access.
#
# Forwards local 127.0.0.1:16443 → remote 127.0.0.1:6443 (k3s API server).
# Port 6443 is NOT publicly exposed; this tunnel is the only safe path to it.
#
# Usage:
#   scripts/tunnel-up.sh          # start tunnel if not already running
#   scripts/tunnel-up.sh --status # print tunnel status and exit
#
# Auth priority (in order):
#   1. SSH key auth — preferred, passwordless, nothing to type
#   2. Interactive password prompt (read -s) — password is never stored or echoed
#
# NEVER pass a password via environment variable or command-line argument.
# If you want passwordless operation, copy your public key to the server:
#   ssh-copy-id -p 22 root@<SERVER_IP>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=demo.env
source "${SCRIPT_DIR}/demo.env"

PID_FILE="/tmp/iiot-tunnel.pid"
LOG_FILE="/tmp/iiot-tunnel.log"

# ── helpers ──────────────────────────────────────────────────────────────────

tunnel_running() {
  if [[ -f "${PID_FILE}" ]]; then
    local pid
    pid=$(cat "${PID_FILE}")
    if kill -0 "${pid}" 2>/dev/null; then
      return 0
    fi
    rm -f "${PID_FILE}"
  fi
  # Also catch tunnels started externally (nc returns 0 on open port)
  nc -z 127.0.0.1 "${TUNNEL_LOCAL_PORT}" 2>/dev/null
}

_start_tunnel() {
  local ssh_cmd=("$@")
  "${ssh_cmd[@]}" \
    -N \
    -L "${TUNNEL_LOCAL_PORT}:127.0.0.1:6443" \
    -o StrictHostKeyChecking=no \
    -o ConnectTimeout=15 \
    -o ServerAliveInterval=15 \
    -o ServerAliveCountMax=6 \
    -o ExitOnForwardFailure=yes \
    -p "${SSH_PORT}" \
    "${SSH_USER}@${SERVER_IP}" \
    >"${LOG_FILE}" 2>&1 &
  echo $! > "${PID_FILE}"
}

_wait_port() {
  for i in $(seq 1 20); do
    if nc -z 127.0.0.1 "${TUNNEL_LOCAL_PORT}" 2>/dev/null; then
      echo "[tunnel-up] tunnel ready (port ${TUNNEL_LOCAL_PORT} open after ${i} attempts)"
      return 0
    fi
    sleep 0.5
  done
  echo "[tunnel-up] ERROR: port ${TUNNEL_LOCAL_PORT} did not open in 10s" >&2
  echo "[tunnel-up] log: ${LOG_FILE}" >&2
  # Print log but strip any password hints — log should contain no secrets
  grep -v -i "password\|pass\|auth" "${LOG_FILE}" >&2 || true
  rm -f "${PID_FILE}"
  return 1
}

# ── status check ─────────────────────────────────────────────────────────────

if [[ "${1:-}" == "--status" ]]; then
  if tunnel_running; then
    pid=$(cat "${PID_FILE}" 2>/dev/null || echo "external")
    echo "TUNNEL UP  — 127.0.0.1:${TUNNEL_LOCAL_PORT} → ${SERVER_IP}:6443  (pid ${pid})"
  else
    echo "TUNNEL DOWN"
  fi
  exit 0
fi

# ── already running? ─────────────────────────────────────────────────────────

if tunnel_running; then
  pid=$(cat "${PID_FILE}" 2>/dev/null || echo "external")
  echo "[tunnel-up] already running (pid ${pid}) — nothing to do"
  exit 0
fi

echo "[tunnel-up] starting SSH tunnel: 127.0.0.1:${TUNNEL_LOCAL_PORT} → ${SERVER_IP}:6443"

# ── attempt 1: SSH key auth (preferred — no interaction needed) ───────────────

echo "[tunnel-up] trying key auth..."
_start_tunnel ssh \
  -o PreferredAuthentications=publickey \
  -o PubkeyAuthentication=yes \
  -o PasswordAuthentication=no \
  -o BatchMode=yes

if _wait_port 2>/dev/null; then
  exit 0
fi

# Key auth failed — clean up stale PID
kill "$(cat "${PID_FILE}" 2>/dev/null)" 2>/dev/null || true
rm -f "${PID_FILE}"
echo "[tunnel-up] key auth unavailable — falling back to password"

# ── attempt 2: password auth via sshpass (interactive prompt) ────────────────

if ! command -v sshpass &>/dev/null; then
  echo >&2
  echo "[tunnel-up] sshpass not found. Install it for password-based SSH:" >&2
  echo "  macOS:  brew install hudochenkov/sshpass/sshpass" >&2
  echo "  Linux:  apt-get install sshpass" >&2
  echo >&2
  echo "Or copy your public key to the server (recommended — no password needed):" >&2
  echo "  ssh-copy-id -p ${SSH_PORT} ${SSH_USER}@${SERVER_IP}" >&2
  exit 1
fi

# Prompt interactively — read -s does NOT echo the password
if [[ ! -t 0 ]]; then
  echo "[tunnel-up] ERROR: non-interactive terminal and key auth failed." >&2
  echo "[tunnel-up] Run 'ssh-copy-id -p ${SSH_PORT} ${SSH_USER}@${SERVER_IP}' to set up key auth." >&2
  exit 1
fi

echo
printf "[tunnel-up] SSH password for %s@%s: " "${SSH_USER}" "${SERVER_IP}"
read -r -s SSH_PASS
echo  # newline after hidden input

# Feed the password to sshpass via a file descriptor — never via argv or env
# (argv is visible in `ps`; env leaks to child processes)
_TMP_PASS_FD=$(mktemp)
printf '%s' "${SSH_PASS}" > "${_TMP_PASS_FD}"
unset SSH_PASS  # clear from memory immediately

_start_tunnel sshpass -f "${_TMP_PASS_FD}" ssh \
  -o PreferredAuthentications=password \
  -o PubkeyAuthentication=no

_WAIT_RESULT=0
_wait_port || _WAIT_RESULT=$?

# Always remove the temp password file
rm -f "${_TMP_PASS_FD}"

exit "${_WAIT_RESULT}"
