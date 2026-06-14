#!/usr/bin/env bash
# =============================================================================
# run_load.sh — Sustained SIPREC load test
# =============================================================================
# Drives a configurable call rate against StreamLink until the target call
# count is reached, then prints a statistics summary.
#
# Usage:
#   ./scripts/run_load.sh [options]
#
# Command-line options (override config.env defaults):
#   -t <ip>       Target IP           (SIPP_TARGET_IP)
#   -P <port>     Target port         (SIPP_TARGET_PORT)
#   -i <ip>       Local IP            (LOCAL_IP)
#   -p <port>     Local SIP port      (LOCAL_PORT)
#   -r <cps>      Call rate / sec     (LOAD_RATE)
#   -l <n>        Max concurrent      (LOAD_MAX_CONCURRENT)
#   -m <n>        Total calls         (LOAD_CALLS)
#   -d <ms>       Call hold duration  (CALL_DURATION_MS)
#   -s <scenario> Scenario filename   (SCENARIO)
#   -h            This help
#
# Examples:
#   # Run with defaults from config.env
#   ./scripts/run_load.sh
#
#   # 200 calls at 20 CPS, 15-second hold, against a remote server
#   ./scripts/run_load.sh -t 35.238.183.188 -r 20 -m 200 -d 15000
#
#   # Full RTP audio test
#   ./scripts/run_load.sh -s siprec_uac_audio.xml -r 5 -m 50
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=../config.env
source "$ROOT_DIR/config.env"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { echo "[$(date '+%H:%M:%S')] $*"; }
die() { echo "[ERROR] $*" >&2; exit 1; }

usage() {
    grep '^#' "$0" | sed 's/^# \{0,2\}//' | sed '1,2d'
    exit 0
}

# ---------------------------------------------------------------------------
# Parse CLI options (override config.env values)
# ---------------------------------------------------------------------------

while getopts "t:P:i:p:r:l:m:d:s:h" opt; do
    case "$opt" in
        t) SIPP_TARGET_IP="$OPTARG" ;;
        P) SIPP_TARGET_PORT="$OPTARG" ;;
        i) LOCAL_IP="$OPTARG" ;;
        p) LOCAL_PORT="$OPTARG" ;;
        r) LOAD_RATE="$OPTARG" ;;
        l) LOAD_MAX_CONCURRENT="$OPTARG" ;;
        m) LOAD_CALLS="$OPTARG" ;;
        d) CALL_DURATION_MS="$OPTARG" ;;
        s) SCENARIO="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------

command -v "$SIPP_BIN" &>/dev/null || die "SIPp not found. Install it: apt-get install sipp"

SCENARIO_FILE="$ROOT_DIR/scenarios/$SCENARIO"
[[ -f "$SCENARIO_FILE" ]] || die "Scenario file not found: $SCENARIO_FILE"

# Warn if audio scenario is used without a pcap file
if [[ "$SCENARIO" == *"audio"* ]]; then
    PCAP_FILE="$ROOT_DIR/$AUDIO_PCAP"
    if [[ ! -f "$PCAP_FILE" ]]; then
        log "WARNING: Audio scenario selected but PCAP file not found: $PCAP_FILE"
        log "         Run ./scripts/generate_pcap.sh first, or switch to siprec_uac.xml"
    fi
fi

# Estimate required media port range: each call needs 4 ports (2 streams × RTP+RTCP)
REQUIRED_PORTS=$(( LOAD_MAX_CONCURRENT * 4 ))
log "Port range needed: $MEDIA_PORT_START – $(( MEDIA_PORT_START + REQUIRED_PORTS - 1 ))"
log "Verify this does NOT overlap StreamLink's rtp_port_start–rtp_port_end in config.yaml"

mkdir -p "$ROOT_DIR/$LOG_DIR"
LOGBASE="$ROOT_DIR/$LOG_DIR/load_$(date '+%Y%m%d_%H%M%S')"

# ---------------------------------------------------------------------------
# Display test plan
# ---------------------------------------------------------------------------

log ""
log "=== SIPREC SIPp Load Test ==="
log "Target        : $SIPP_TARGET_IP:$SIPP_TARGET_PORT"
log "Local         : $LOCAL_IP:$LOCAL_PORT  (media from $MEDIA_PORT_START)"
log "Scenario      : $SCENARIO"
log "Call rate     : $LOAD_RATE calls/sec"
log "Max concurrent: $LOAD_MAX_CONCURRENT"
log "Total calls   : $LOAD_CALLS"
log "Hold duration : ${CALL_DURATION_MS} ms"
log "Log prefix    : $LOGBASE"
log ""

# ---------------------------------------------------------------------------
# Run SIPp
# ---------------------------------------------------------------------------

"$SIPP_BIN" "$SIPP_TARGET_IP:$SIPP_TARGET_PORT" \
    -sf  "$SCENARIO_FILE" \
    -i   "$LOCAL_IP" \
    -p   "$LOCAL_PORT" \
    -mp  "$MEDIA_PORT_START" \
    -l   "$LOAD_MAX_CONCURRENT" \
    -m   "$LOAD_CALLS" \
    -r   "$LOAD_RATE" \
    -d   "$CALL_DURATION_MS" \
    -trace_stat \
    -trace_err  \
    -error_file "${LOGBASE}_errors.log"  \
    -stf        "${LOGBASE}_stats.csv"   \
    -recv_timeout 30000 \
    2>&1 | tee "${LOGBASE}_run.log"

STATUS=${PIPESTATUS[0]}

# ---------------------------------------------------------------------------
# Quick summary from stats CSV
# ---------------------------------------------------------------------------

STATS_FILE="${LOGBASE}_stats.csv"
if [[ -f "$STATS_FILE" ]]; then
    echo
    log "--- Statistics summary (last row) ---"
    # Print header and last data row in a readable format
    awk -F';' '
        NR==1 { for(i=1;i<=NF;i++) header[i]=$i; next }
        { for(i=1;i<=NF;i++) last[i]=$i }
        END {
            printf "%-35s %s\n", "Metric", "Value"
            printf "%-35s %s\n", "------", "-----"
            for(i=1;i<=length(header);i++)
                printf "%-35s %s\n", header[i], last[i]
        }
    ' "$STATS_FILE"
fi

echo
if [[ $STATUS -eq 0 ]]; then
    log "✓ Load test PASSED (exit $STATUS)"
else
    log "✗ Load test FAILED (exit $STATUS)"
    log "  Check: ${LOGBASE}_errors.log"
fi
exit $STATUS
