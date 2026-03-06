#!/usr/bin/env bash
# kube-env.sh — Set KUBECONFIG to the dedicated iiot cluster kubeconfig.
#
# This script NEVER touches ~/.kube/config. It creates a separate
# ~/.kube/kubeconfig-iiot file if it does not already exist, fetching
# it from the server via SSH (key auth preferred).
#
# Usage (source it — do not execute it):
#   source scripts/kube-env.sh
#
# The SSH tunnel must be running before kubectl will work:
#   scripts/tunnel-up.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=demo.env
source "${SCRIPT_DIR}/demo.env"

KUBECONFIG_PATH="${HOME}/.kube/kubeconfig-iiot"

_fetch_kubeconfig() {
  echo "[kube-env] kubeconfig not found — fetching from server..."
  mkdir -p "${HOME}/.kube"

  # Try key auth first (preferred, non-interactive)
  if ssh \
      -o PreferredAuthentications=publickey \
      -o PubkeyAuthentication=yes \
      -o PasswordAuthentication=no \
      -o BatchMode=yes \
      -o StrictHostKeyChecking=no \
      -o ConnectTimeout=10 \
      -p "${SSH_PORT}" \
      "${SSH_USER}@${SERVER_IP}" \
      "cat /etc/rancher/k3s/k3s.yaml" > "${KUBECONFIG_PATH}.tmp" 2>/dev/null; then
    echo "[kube-env] fetched via key auth"
  elif command -v sshpass &>/dev/null && [[ -t 0 ]]; then
    # Fall back to interactive password prompt
    printf "[kube-env] SSH password for %s@%s: " "${SSH_USER}" "${SERVER_IP}"
    read -r -s _KUBE_SSH_PASS
    echo
    _TMP_PASS_FD=$(mktemp)
    printf '%s' "${_KUBE_SSH_PASS}" > "${_TMP_PASS_FD}"
    unset _KUBE_SSH_PASS
    sshpass -f "${_TMP_PASS_FD}" ssh \
      -o PreferredAuthentications=password \
      -o PubkeyAuthentication=no \
      -o StrictHostKeyChecking=no \
      -o ConnectTimeout=10 \
      -p "${SSH_PORT}" \
      "${SSH_USER}@${SERVER_IP}" \
      "cat /etc/rancher/k3s/k3s.yaml" > "${KUBECONFIG_PATH}.tmp"
    rm -f "${_TMP_PASS_FD}"
    echo "[kube-env] fetched via password auth"
  else
    echo "[kube-env] ERROR: key auth failed and sshpass is unavailable or non-interactive." >&2
    echo "[kube-env] Install key auth: ssh-copy-id -p ${SSH_PORT} ${SSH_USER}@${SERVER_IP}" >&2
    rm -f "${KUBECONFIG_PATH}.tmp"
    return 1
  fi

  # Rewrite server URL → local tunnel endpoint (never expose 6443 publicly)
  sed "s|https://127.0.0.1:6443|https://127.0.0.1:${TUNNEL_LOCAL_PORT}|g" \
    "${KUBECONFIG_PATH}.tmp" > "${KUBECONFIG_PATH}"

  rm -f "${KUBECONFIG_PATH}.tmp"
  chmod 600 "${KUBECONFIG_PATH}"
  echo "[kube-env] kubeconfig written to ${KUBECONFIG_PATH} (chmod 600)"
}

if [[ ! -f "${KUBECONFIG_PATH}" ]]; then
  _fetch_kubeconfig
fi

export KUBECONFIG="${KUBECONFIG_PATH}"
echo "[kube-env] KUBECONFIG=${KUBECONFIG}"
