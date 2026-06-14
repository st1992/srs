#!/usr/bin/env bash
# =============================================================================
# run_smoke.sh — Single-call sanity check against StreamLink
# =============================================================================
# Sends exactly ONE SIPREC INVITE, verifies the full signaling flow
# (INVITE → 200 OK → ACK → BYE → 200 OK), and prints SIPp's result.
#
# Usage:
#   ./scripts/run_smoke.sh [options]
#
# Environment overrides (export before running):
#   SIPP_TARGET_IP   SIPP_TARGET_PORT   LOCAL_IP   LOCAL_PORT
#   CALL_DURATION_MS SCENARIO           SIPP_BIN
#
# Example (non-local target):
#   SIPP_TARGET_IP=35.238.183.188 ./scripts/run_smoke.sh
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=../config.env
source "$ROOT_DIR/config.env"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log()  { echo "[$(date '+%H:%M:%S')] $*"; }
die()  { echo "[ERROR] $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------

command -v "$SIPP_BIN" &>/dev/null || die "SIPp not found. Install it: apt-get install sipp"

SCENARIO_FILE="$ROOT_DIR/scenarios/$SCENARIO"
[[ -f "$SCENARIO_FILE" ]] || die "Scenario file not found: $SCENARIO_FILE"

mkdir -p "$ROOT_DIR/$LOG_DIR"
LOGBASE="$ROOT_DIR/$LOG_DIR/smoke_$(date '+%Y%m%d_%H%M%S')"

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

log "=== SIPREC SIPp Smoke Test ==="
log "Target  : $SIPP_TARGET_IP:$SIPP_TARGET_PORT"
log "Local   : $LOCAL_IP:$LOCAL_PORT"
log "Scenario: $SCENARIO"
log "Duration: ${CALL_DURATION_MS} ms"
log "Logs    : $LOGBASE.*"
echo

"$SIPP_BIN" "$SIPP_TARGET_IP:$SIPP_TARGET_PORT" \
    -sf "$SCENARIO_FILE" \
    -i  "$LOCAL_IP" \
    -p  "$LOCAL_PORT" \
    -mp "$MEDIA_PORT_START" \
    -l  1 \
    -m  1 \
    -r  1 \
    -d  "$CALL_DURATION_MS" \
    -trace_msg \
    -trace_err \
    -message_file "${LOGBASE}_messages.log" \
    -error_file   "${LOGBASE}_errors.log"   \
    -recv_timeout 15000 \
    -timeout      60s   \
    2>&1 | tee "${LOGBASE}_run.log"

STATUS=${PIPESTATUS[0]}

echo
if [[ $STATUS -eq 0 ]]; then
    log "✓ Smoke test PASSED (exit $STATUS)"
else
    log "✗ Smoke test FAILED (exit $STATUS)"
    log "  Check: ${LOGBASE}_errors.log"
fi
exit $STATUS
