#!/usr/bin/env bash
# server-maintenance.sh — k3s kine SQLite WAL checkpoint and VACUUM.
#
# PURPOSE
#   Over time, the kine SQLite WAL file can grow large (especially after
#   periods of high controller churn). This script checkpoints and compacts
#   the database to recover disk space and restore read performance.
#
# WHEN TO RUN
#   - When state.db-wal is significantly larger than state.db.
#   - When kubectl commands are slow or timing out.
#   - After removing controllers that caused heavy write churn (e.g. cert-manager).
#
# HOW TO RUN
#   1. SSH into the server:
#        ssh <SSH_USER_HERE>@<SERVER_IP_HERE>
#   2. Copy or paste this script onto the server, or pipe it via ssh.
#   3. Run as root (or with sudo).
#
# SAFETY
#   - This script STOPS k3s before touching the database.
#   - k3s is RESTARTED when the script finishes (or on error via trap).
#   - Never run SQLite tools against the live database while k3s is running.
#
# NOTE: Do not embed passwords in this script.

set -euo pipefail

# ── configuration ─────────────────────────────────────────────────────────────

SERVER_IP=<SERVER_IP_HERE>
DB_PATH="/var/lib/rancher/k3s/server/db/state.db"

# ── helpers ───────────────────────────────────────────────────────────────────

log()  { echo "[maintenance] $*"; }
fail() { echo "[maintenance] ERROR: $*" >&2; exit 1; }

# Ensure k3s is restarted even if the script exits early.
_restart_k3s() {
  log "Restarting k3s..."
  systemctl start k3s
  log "k3s started. Waiting for API server to become ready..."
  for i in $(seq 1 30); do
    if systemctl is-active --quiet k3s; then
      log "k3s is active."
      return 0
    fi
    sleep 2
  done
  fail "k3s did not become active within 60s after restart."
}
trap _restart_k3s EXIT

# ── pre-flight checks ─────────────────────────────────────────────────────────

if [[ "$(id -u)" -ne 0 ]]; then
  fail "This script must be run as root (sudo)."
fi

if ! command -v sqlite3 &>/dev/null; then
  fail "sqlite3 is not installed. Install it with: apt-get install -y sqlite3"
fi

if [[ ! -f "${DB_PATH}" ]]; then
  fail "Database file not found: ${DB_PATH}"
fi

log "Database path: ${DB_PATH}"

# ── show sizes before ─────────────────────────────────────────────────────────

log "--- Sizes BEFORE maintenance ---"
ls -lh "${DB_PATH}" "${DB_PATH}-wal" 2>/dev/null || log "(WAL file does not exist — already clean)"
du -sh "$(dirname "${DB_PATH}")"

# ── stop k3s ──────────────────────────────────────────────────────────────────

log "Stopping k3s..."
systemctl stop k3s

# Brief pause to let the process release file locks
sleep 2

if systemctl is-active --quiet k3s; then
  fail "k3s is still active after stop — aborting to avoid database corruption."
fi

log "k3s stopped."

# ── WAL checkpoint ────────────────────────────────────────────────────────────

log "Running WAL checkpoint (TRUNCATE mode)..."
sqlite3 "${DB_PATH}" "PRAGMA wal_checkpoint(TRUNCATE);"
log "WAL checkpoint complete."

# ── VACUUM ────────────────────────────────────────────────────────────────────

log "Running VACUUM (this may take a moment for large databases)..."
sqlite3 "${DB_PATH}" "VACUUM;"
log "VACUUM complete."

# ── show sizes after ──────────────────────────────────────────────────────────

log "--- Sizes AFTER maintenance ---"
ls -lh "${DB_PATH}" "${DB_PATH}-wal" 2>/dev/null || log "(WAL file is gone — fully checkpointed)"
du -sh "$(dirname "${DB_PATH}")"

# ── k3s restart (via trap) ────────────────────────────────────────────────────
# The EXIT trap calls _restart_k3s automatically.

log "Maintenance complete. k3s will now restart."
