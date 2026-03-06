#!/usr/bin/env bash
# demo-30s.sh — 30-second live demo of the IIoT Edge Platform on k3s.
#
# What it does:
#   1. Ensures the SSH tunnel to the Kubernetes API is up
#   2. Sets KUBECONFIG to the dedicated iiot kubeconfig (never touches ~/.kube/config)
#   3. Prints a concise cluster/pod/ingress snapshot
#   4. Runs HTTP + HTTPS demo curls (401 without auth, 200 with auth)
#
# Usage:
#   bash scripts/demo-30s.sh
#   make demo
#
# The BasicAuth password is prompted interactively (read -s).
# It is NEVER stored in this script, in env vars, or in any file visible to git.
#
# Prerequisites:
#   - SSH access to the server (key auth recommended; password prompt as fallback)
#   - kubectl installed locally
#   - The tunnel script (scripts/tunnel-up.sh) available

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=demo.env
source "${SCRIPT_DIR}/demo.env"

KUBECONFIG_PATH="${HOME}/.kube/kubeconfig-iiot"

# ── helpers ──────────────────────────────────────────────────────────────────

PASS=0
FAIL=0

ok()   { echo "  [PASS] $*"; PASS=$((PASS+1)); }
fail() { echo "  [FAIL] $*"; FAIL=$((FAIL+1)); }
hdr()  { echo; echo "══════════════════════════════════════════════"; echo "  $*"; echo "══════════════════════════════════════════════"; }

check_http() {
  local label="$1" url="$2" want_code="$3"
  shift 3
  local code
  code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 8 "$@" "${url}" 2>/dev/null || echo "000")
  if [[ "${code}" == "${want_code}" ]]; then
    ok "${label} → HTTP ${code}"
  else
    fail "${label} → HTTP ${code} (expected ${want_code})"
  fi
}

# ── step 1: ensure tunnel ─────────────────────────────────────────────────────

hdr "STEP 1 — SSH tunnel to k3s API"
bash "${SCRIPT_DIR}/tunnel-up.sh"

# ── step 2: kubeconfig ────────────────────────────────────────────────────────

hdr "STEP 2 — Kubernetes access"
# shellcheck source=kube-env.sh
source "${SCRIPT_DIR}/kube-env.sh"
echo "  kubectl → $(kubectl cluster-info --request-timeout=8s 2>&1 | head -1)"

# ── step 3: cluster snapshot ──────────────────────────────────────────────────

hdr "STEP 3 — Cluster snapshot"

echo
echo "  [nodes]"
kubectl get nodes -o wide --request-timeout=8s 2>&1 | sed 's/^/    /'

echo
echo "  [pods — namespace: ${DEMO_NAMESPACE}]"
kubectl -n "${DEMO_NAMESPACE}" get pods -o wide --request-timeout=8s 2>&1 | sed 's/^/    /'

echo
echo "  [ingress]"
kubectl -n "${DEMO_NAMESPACE}" get ingress --request-timeout=8s 2>&1 | sed 's/^/    /'

echo
echo "  [traefik service]"
kubectl -n kube-system get svc traefik -o wide --request-timeout=8s 2>&1 | sed 's/^/    /'

# ── step 4: prompt for BasicAuth password ─────────────────────────────────────

hdr "STEP 4 — HTTP / HTTPS demo checks"

echo
echo "  The BasicAuth password is stored on the server at:"
echo "    /opt/iiot/secrets/basic-auth-password.txt"
echo
# read -s does not echo the password — it is never stored in a variable
# that persists beyond this script's lifetime
if [[ -t 0 ]]; then
  read -r -s -p "  Enter BasicAuth password for user '${DEMO_USER}' (or Ctrl-C to skip curl tests): " DEMO_PASS
  echo
else
  echo "  [non-interactive mode — skipping curl auth tests]"
  DEMO_PASS=""
fi

# ── step 4a: HTTP tests ───────────────────────────────────────────────────────

echo
echo "  --- HTTP (port 80) ---"
check_http "GET /healthz   no-auth " "http://${SERVER_IP}/healthz" "401"
check_http "GET /readyz    no-auth " "http://${SERVER_IP}/readyz"  "401"

if [[ -n "${DEMO_PASS}" ]]; then
  check_http "GET /healthz   auth    " "http://${SERVER_IP}/healthz" "200" \
    -u "${DEMO_USER}:${DEMO_PASS}"
  check_http "GET /readyz    auth    " "http://${SERVER_IP}/readyz"  "200" \
    -u "${DEMO_USER}:${DEMO_PASS}"
fi

# ── step 4b: HTTPS tests (self-signed cert — -k required) ────────────────────

echo
echo "  --- HTTPS (port 443, self-signed cert — curl -k) ---"
check_http "GET /healthz   no-auth " "https://${SERVER_IP}/healthz" "401"

if [[ -n "${DEMO_PASS}" ]]; then
  check_http "GET /healthz   auth    " "https://${SERVER_IP}/healthz" "200" \
    -u "${DEMO_USER}:${DEMO_PASS}"
  check_http "GET /readyz    auth    " "https://${SERVER_IP}/readyz"  "200" \
    -u "${DEMO_USER}:${DEMO_PASS}"
fi

# ── step 4c: POST a telemetry reading through the ingress ─────────────────────

if [[ -n "${DEMO_PASS}" ]]; then
  echo
  echo "  --- POST telemetry (through ingress, authenticated) ---"
  NOW=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  PAYLOAD='{"device_id":"demo-device","timestamp":"'"${NOW}"'","temperature_c":22.5,"pressure_hpa":1013.0,"humidity_pct":55.0,"status":"ok"}'
  POST_CODE=$(curl -sk -o /tmp/demo-post.out -w "%{http_code}" \
    --max-time 8 \
    -X POST "https://${SERVER_IP}/api/v1/telemetry" \
    -u "${DEMO_USER}:${DEMO_PASS}" \
    -H "Content-Type: application/json" \
    -d "${PAYLOAD}" 2>/dev/null || echo "000")
  if [[ "${POST_CODE}" == "202" ]]; then
    ok "POST /api/v1/telemetry → HTTP ${POST_CODE}  body: $(cat /tmp/demo-post.out)"
  else
    fail "POST /api/v1/telemetry → HTTP ${POST_CODE}  body: $(cat /tmp/demo-post.out 2>/dev/null)"
  fi
fi

# ── summary ───────────────────────────────────────────────────────────────────

echo
echo "══════════════════════════════════════════════"
if [[ ${FAIL} -eq 0 ]]; then
  echo "  DEMO OK — ${PASS} checks passed, ${FAIL} failed"
else
  echo "  DEMO INCOMPLETE — ${PASS} passed, ${FAIL} FAILED"
fi
echo "══════════════════════════════════════════════"
echo

[[ ${FAIL} -eq 0 ]]
