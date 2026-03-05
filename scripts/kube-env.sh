#!/usr/bin/env bash
# kube-env.sh — Set KUBECONFIG to the dedicated iiot cluster kubeconfig.
#
# This script NEVER touches ~/.kube/config. It creates a separate
# ~/.kube/kubeconfig-iiot file if it does not already exist.
#
# Usage (source it, don't execute it):
#   source scripts/kube-env.sh
#
# After sourcing, kubectl and KUBECONFIG point to the iiot cluster.
# The SSH tunnel must be running first (scripts/tunnel-up.sh).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=demo.env
source "${SCRIPT_DIR}/demo.env"

# Resolve KUBECONFIG_PATH (demo.env uses ${HOME} which needs eval)
KUBECONFIG_PATH="${HOME}/.kube/kubeconfig-iiot"

_fetch_kubeconfig() {
  echo "[kube-env] kubeconfig not found — fetching from server (no secrets printed)..."
  mkdir -p "${HOME}/.kube"

  # Use sshpass if SSHPASS is in env; otherwise use key auth
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
    -o StrictHostKeyChecking=no \
    -o ConnectTimeout=15 \
    "${AUTH_ARGS[@]}" \
    -p "${SSH_PORT}" \
    "${SSH_USER}@${SERVER_IP}" \
    "cat /etc/rancher/k3s/k3s.yaml" > "${KUBECONFIG_PATH}.tmp"

  # Rewrite server URL to go through local tunnel — never expose 6443 publicly
  sed 's|https://127.0.0.1:6443|https://127.0.0.1:'"${TUNNEL_LOCAL_PORT}"'|g' \
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
