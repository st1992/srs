#!/usr/bin/env bash
# =============================================================================
# run_stress.sh — Ramp-up stress test to find StreamLink's capacity ceiling
# =============================================================================
# Starts at STRESS_RATE_START calls/sec, then increments by STRESS_RATE_STEP
# every STRESS_RAMP_INTERVAL seconds until STRESS_RATE_MAX is reached.
# SIPp runs in infinite mode (-m 0) during the ramp; it is managed via the
# SIPp XML API or by launching separate processes per rate tier.
#
# Strategy: run one SIPp process per rate tier, collect stats, analyse.
#
# Usage:
#   ./scripts/run_stress.sh [options]
#
# Command-line options:
#   -t <ip>    Target IP                  (SIPP_TARGET_IP)
#   -P <port>  Target port                (SIPP_TARGET_PORT)
#   -i <ip>    Local IP                   (LOCAL_IP)
#   -s <rate>  Starting rate (cps)        (STRESS_RATE_START)
#   -e <rate>  Ending / max rate (cps)    (STRESS_RATE_MAX)
#   -x <step>  Rate increment per tier    (STRESS_RATE_STEP)
#   -n <sec>   Duration of each tier (s)  (STRESS_RAMP_INTERVAL)
#   -d <ms>    Call hold duration (ms)    (CALL_DURATION_MS)
#   -l <n>     Max concurrent calls       (STRESS_MAX_CONCURRENT)
#   -h         This help
#
# The script prints a per-tier summary table at the end showing:
#   Rate | Attempted | Completed | Failed | Avg RTD (ms)
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

# ---------------------------------------------------------------------------
# Parse CLI options
# ---------------------------------------------------------------------------

while getopts "t:P:i:s:e:x:n:d:l:h" opt; do
    case "$opt" in
        t) SIPP_TARGET_IP="$OPTARG" ;;
        P) SIPP_TARGET_PORT="$OPTARG" ;;
        i) LOCAL_IP="$OPTARG" ;;
        s) STRESS_RATE_START="$OPTARG" ;;
        e) STRESS_RATE_MAX="$OPTARG" ;;
        x) STRESS_RATE_STEP="$OPTARG" ;;
        n) STRESS_RAMP_INTERVAL="$OPTARG" ;;
        d) CALL_DURATION_MS="$OPTARG" ;;
        l) STRESS_MAX_CONCURRENT="$OPTARG" ;;
        h) grep '^#' "$0" | sed 's/^# \{0,2\}//' | sed '1,2d'; exit 0 ;;
        *) die "Unknown option. Use -h for help." ;;
    esac
done

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------

command -v "$SIPP_BIN" &>/dev/null || die "SIPp not found. Install: apt-get install sipp"
SCENARIO_FILE="$ROOT_DIR/scenarios/$SCENARIO"
[[ -f "$SCENARIO_FILE" ]] || die "Scenario file not found: $SCENARIO_FILE"

mkdir -p "$ROOT_DIR/$LOG_DIR"
RUN_TAG="stress_$(date '+%Y%m%d_%H%M%S')"
SUMMARY_FILE="$ROOT_DIR/$LOG_DIR/${RUN_TAG}_summary.txt"

log ""
log "=== SIPREC SIPp Stress / Ramp Test ==="
log "Target       : $SIPP_TARGET_IP:$SIPP_TARGET_PORT"
log "Local IP     : $LOCAL_IP  (media from $MEDIA_PORT_START)"
log "Scenario     : $SCENARIO"
log "Rate range   : $STRESS_RATE_START → $STRESS_RATE_MAX cps  (step $STRESS_RATE_STEP)"
log "Tier duration: ${STRESS_RAMP_INTERVAL}s"
log "Hold time    : ${CALL_DURATION_MS}ms"
log "Max conc.    : $STRESS_MAX_CONCURRENT"
log ""

# Each tier uses a different local port to avoid port conflicts when
# a previous tier's calls are still draining.
TIER_LOCAL_PORT=$LOCAL_PORT

printf "%-8s %-12s %-12s %-10s %-12s\n" \
    "Rate" "Attempted" "Completed" "Failed" "AvgRTD(ms)" | tee "$SUMMARY_FILE"
printf "%-8s %-12s %-12s %-10s %-12s\n" \
    "----" "---------" "---------" "------" "----------" | tee -a "$SUMMARY_FILE"

TIER=1
CURRENT_RATE=$STRESS_RATE_START

while [[ $CURRENT_RATE -le $STRESS_RATE_MAX ]]; do

    # Calls per tier: rate × duration (seconds) with a minimum of 10
    CALLS_THIS_TIER=$(( CURRENT_RATE * STRESS_RAMP_INTERVAL ))
    [[ $CALLS_THIS_TIER -lt 10 ]] && CALLS_THIS_TIER=10

    LOGBASE="$ROOT_DIR/$LOG_DIR/${RUN_TAG}_tier${TIER}_r${CURRENT_RATE}"

    log "Tier $TIER: rate=$CURRENT_RATE cps, calls=$CALLS_THIS_TIER, port=$TIER_LOCAL_PORT"

    # Run SIPp; capture exit code without aborting the script (we want all tiers)
    set +e
    "$SIPP_BIN" "$SIPP_TARGET_IP:$SIPP_TARGET_PORT" \
        -sf  "$SCENARIO_FILE" \
        -i   "$LOCAL_IP" \
        -p   "$TIER_LOCAL_PORT" \
        -mp  "$MEDIA_PORT_START" \
        -l   "$STRESS_MAX_CONCURRENT" \
        -m   "$CALLS_THIS_TIER" \
        -r   "$CURRENT_RATE" \
        -d   "$CALL_DURATION_MS" \
        -trace_err  \
        -trace_stat \
        -error_file "${LOGBASE}_errors.log" \
        -stf        "${LOGBASE}_stats.csv"  \
        -recv_timeout 30000 \
        -nd \
        >"${LOGBASE}_run.log" 2>&1
    SIPP_EXIT=$?
    set -e

    # Parse last row of stats CSV
    ATTEMPTED=0; COMPLETED=0; FAILED=0; AVG_RTD="N/A"
    if [[ -f "${LOGBASE}_stats.csv" ]]; then
        # SIPp CSV columns (semicolon-separated):
        # StartTime;LastResetTime;CurrentTime;ElapsedTime(P);...;
        # SuccessfulCall(P);FailedCall(P);...;ResponseTime1(P);...
        # Column indices vary by SIPp version; use awk to find by header name.
        ATTEMPTED=$(awk -F';' 'NR==1{for(i=1;i<=NF;i++)if($i~/TotalCallCreated/)col=i} NR>1{v=$col} END{print v+0}' "${LOGBASE}_stats.csv")
        COMPLETED=$(awk -F';' 'NR==1{for(i=1;i<=NF;i++)if($i~/SuccessfulCall/)col=i} NR>1{v=$col} END{print v+0}' "${LOGBASE}_stats.csv")
        FAILED=$(awk -F';' 'NR==1{for(i=1;i<=NF;i++)if($i~/FailedCall/)col=i} NR>1{v=$col} END{print v+0}' "${LOGBASE}_stats.csv")
        AVG_RTD=$(awk -F';' 'NR==1{for(i=1;i<=NF;i++)if($i~/ResponseTime1\(P\)/)col=i} NR>1{v=$col} END{if(v!="")printf "%.1f",v; else print "N/A"}' "${LOGBASE}_stats.csv")
    fi

    STATUS_MARK="ok"
    [[ $SIPP_EXIT -ne 0 ]] && STATUS_MARK="FAIL(exit $SIPP_EXIT)"

    printf "%-8s %-12s %-12s %-10s %-12s  %s\n" \
        "${CURRENT_RATE}cps" "$ATTEMPTED" "$COMPLETED" "$FAILED" "$AVG_RTD" "$STATUS_MARK" \
        | tee -a "$SUMMARY_FILE"

    # Stagger local port to avoid TIME_WAIT conflicts from previous tier
    TIER_LOCAL_PORT=$(( TIER_LOCAL_PORT + 1 ))

    CURRENT_RATE=$(( CURRENT_RATE + STRESS_RATE_STEP ))
    TIER=$(( TIER + 1 ))

    # Brief cool-down before next tier
    [[ $CURRENT_RATE -le $STRESS_RATE_MAX ]] && sleep 2
done

echo
log "Stress test complete. Full summary: $SUMMARY_FILE"
log "Per-tier logs and stats: $ROOT_DIR/$LOG_DIR/${RUN_TAG}_tier*"
