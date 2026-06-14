#!/usr/bin/env bash
# =============================================================================
# run_stress_monitored.sh — Ramp stress test + per-tier CPU/memory reporting
# =============================================================================
# Same ramp logic as run_stress.sh but also samples the StreamLink process
# (CPU %, RSS MB) every 0.5 seconds and prints a combined summary table:
#
#   Rate  | Calls | Ok  | Fail | AvgCPU | PeakCPU | AvgMem | PeakMem
#   ------+-------+-----+------+--------+---------+--------+--------
#   10cps |   150 | 150 |    0 |   2.1% |    4.3% |  32 MB |  33 MB
#   ...
#
# Usage:
#   ./scripts/run_stress_monitored.sh [options]
#
# Options (same as run_stress.sh):
#   -t <ip>    Target IP               (SIPP_TARGET_IP)
#   -P <port>  Target port             (SIPP_TARGET_PORT)
#   -i <ip>    Local IP                (LOCAL_IP)
#   -s <cps>   Starting rate           (STRESS_RATE_START)
#   -e <cps>   Ending / max rate       (STRESS_RATE_MAX)
#   -x <step>  Rate increment          (STRESS_RATE_STEP)
#   -n <sec>   Duration per tier (s)   (STRESS_RAMP_INTERVAL)
#   -d <ms>    Call hold duration (ms) (CALL_DURATION_MS)
#   -l <n>     Max concurrent calls    (STRESS_MAX_CONCURRENT)
#   -P2 <pid>  Force StreamLink PID    (auto-detected by default)
#   -h         Help
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
now_ms() { date +%s%3N; }   # milliseconds since epoch

# ---------------------------------------------------------------------------
# Parse CLI options
# ---------------------------------------------------------------------------
SL_PID_OVERRIDE=""
while getopts "t:P:i:s:e:x:n:d:l:2:h" opt; do
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
        2) SL_PID_OVERRIDE="$OPTARG" ;;
        h) grep '^#' "$0" | sed 's/^# \{0,2\}//' | sed '1,2d'; exit 0 ;;
        *) die "Unknown option. Use -h for help." ;;
    esac
done

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
command -v "$SIPP_BIN" &>/dev/null || die "SIPp not found. Install: apt-get install sip-tester"
SCENARIO_FILE="$ROOT_DIR/scenarios/$SCENARIO"
[[ -f "$SCENARIO_FILE" ]] || die "Scenario file not found: $SCENARIO_FILE"

# Find or validate StreamLink PID
if [[ -n "$SL_PID_OVERRIDE" ]]; then
    SL_PID="$SL_PID_OVERRIDE"
else
    SL_PID=$(pgrep -f siprec-recorder 2>/dev/null | tail -1 || true)
fi

if [[ -z "$SL_PID" ]] || ! kill -0 "$SL_PID" 2>/dev/null; then
    log "WARNING: StreamLink process not found — CPU/memory columns will be 0."
    log "         Start it with: ./siprec-recorder -config config.yaml"
    SL_PID=""
fi

[[ -n "$SL_PID" ]] && log "Monitoring StreamLink PID: $SL_PID"

mkdir -p "$ROOT_DIR/$LOG_DIR"
RUN_TAG="stress_mon_$(date '+%Y%m%d_%H%M%S')"
MONITOR_RAW="$ROOT_DIR/$LOG_DIR/${RUN_TAG}_monitor.csv"
SUMMARY_FILE="$ROOT_DIR/$LOG_DIR/${RUN_TAG}_summary.txt"

# ---------------------------------------------------------------------------
# Background resource monitor
# ---------------------------------------------------------------------------
# Writes:  epoch_ms,cpu_pct,mem_pct,rss_mb
# cpu_pct  = %CPU from ps (instantaneous, may briefly exceed 100 on multi-core)
# mem_pct  = %MEM from ps (resident set / total RAM)
# rss_mb   = RSS in MiB

echo "epoch_ms,cpu_pct,mem_pct,rss_mb" > "$MONITOR_RAW"

monitor_loop() {
    local pid="$1"
    local outfile="$2"
    while kill -0 "$pid" 2>/dev/null; do
        local ts
        ts=$(date +%s%3N)
        # ps -p PID -o pcpu= -o pmem= -o rss=  → three space-separated numbers
        read -r cpu mem rss < <(ps -p "$pid" -o pcpu= -o pmem= -o rss= 2>/dev/null || echo "0 0 0")
        local rss_mb
        rss_mb=$(awk "BEGIN{printf \"%.1f\", $rss/1024}")
        echo "${ts},${cpu},${mem},${rss_mb}" >> "$outfile"
        sleep 0.5
    done
}

if [[ -n "$SL_PID" ]]; then
    monitor_loop "$SL_PID" "$MONITOR_RAW" &
    MONITOR_PID=$!
else
    MONITOR_PID=""
fi

# ---------------------------------------------------------------------------
# Stress ramp
# ---------------------------------------------------------------------------
log ""
log "=== SIPREC SIPp Stress + Resource Monitor ==="
log "Target       : $SIPP_TARGET_IP:$SIPP_TARGET_PORT"
log "Local IP     : $LOCAL_IP  (media from $MEDIA_PORT_START)"
log "Scenario     : $SCENARIO"
log "Rate range   : $STRESS_RATE_START → $STRESS_RATE_MAX cps  (step $STRESS_RATE_STEP)"
log "Tier duration: ${STRESS_RAMP_INTERVAL}s"
log "Hold time    : ${CALL_DURATION_MS}ms"
log "Max conc.    : $STRESS_MAX_CONCURRENT"
[[ -n "$SL_PID" ]] && log "Monitor PID  : $SL_PID  → $MONITOR_RAW"
log ""

TIER_LOCAL_PORT=$LOCAL_PORT
TIER=1
CURRENT_RATE=$STRESS_RATE_START

# Per-tier timing table (start_ms|end_ms|rate|logbase)
TIER_TIMING_FILE=$(mktemp /tmp/tier_timing.XXXXXX)

while [[ $CURRENT_RATE -le $STRESS_RATE_MAX ]]; do

    CALLS_THIS_TIER=$(( CURRENT_RATE * STRESS_RAMP_INTERVAL ))
    [[ $CALLS_THIS_TIER -lt 10 ]] && CALLS_THIS_TIER=10

    LOGBASE="$ROOT_DIR/$LOG_DIR/${RUN_TAG}_tier${TIER}_r${CURRENT_RATE}"

    log "Tier $TIER  │  rate=${CURRENT_RATE} cps  │  calls=${CALLS_THIS_TIER}  │  port=${TIER_LOCAL_PORT}"

    TIER_START_MS=$(now_ms)

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
        -timer_resol     1     \
        -max_sched_loops 10000 \
        -max_recv_loops  10000 \
        -trace_err  \
        -trace_stat \
        -error_file "${LOGBASE}_errors.log" \
        -stf        "${LOGBASE}_stats.csv"  \
        -recv_timeout 30000 \
        -nd \
        >"${LOGBASE}_run.log" 2>&1
    SIPP_EXIT=$?
    set -e

    TIER_END_MS=$(now_ms)

    # Record timing for this tier
    echo "${TIER_START_MS}|${TIER_END_MS}|${CURRENT_RATE}|${LOGBASE}" >> "$TIER_TIMING_FILE"

    TIER_LOCAL_PORT=$(( TIER_LOCAL_PORT + 1 ))
    CURRENT_RATE=$(( CURRENT_RATE + STRESS_RATE_STEP ))
    TIER=$(( TIER + 1 ))

    [[ $CURRENT_RATE -le $STRESS_RATE_MAX ]] && sleep 2
done

# Stop monitor
if [[ -n "$MONITOR_PID" ]]; then
    kill "$MONITOR_PID" 2>/dev/null || true
    wait "$MONITOR_PID" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Aggregate and print combined report
# ---------------------------------------------------------------------------
python3 - "$TIER_TIMING_FILE" "$MONITOR_RAW" "$SUMMARY_FILE" <<'PYEOF'
import sys, csv, os

timing_file  = sys.argv[1]
monitor_file = sys.argv[2]
summary_file = sys.argv[3]

# ---- Load monitor samples --------------------------------------------------
samples = []   # list of (epoch_ms, cpu, mem, rss_mb)
try:
    with open(monitor_file) as f:
        rdr = csv.DictReader(f)
        for row in rdr:
            try:
                samples.append((
                    int(row['epoch_ms']),
                    float(row['cpu_pct']),
                    float(row['mem_pct']),
                    float(row['rss_mb']),
                ))
            except (ValueError, KeyError):
                pass
except FileNotFoundError:
    pass

def stats_in_window(start_ms, end_ms):
    """Return (avg_cpu, peak_cpu, avg_mem, peak_mem, avg_rss, peak_rss) for samples in [start, end]."""
    window = [(c, m, r) for (ts, c, m, r) in samples if start_ms <= ts <= end_ms]
    if not window:
        return (0, 0, 0, 0, 0, 0)
    cpus = [x[0] for x in window]
    mems = [x[1] for x in window]
    rsss = [x[2] for x in window]
    return (
        sum(cpus)/len(cpus), max(cpus),
        sum(mems)/len(mems), max(mems),
        sum(rsss)/len(rsss), max(rsss),
    )

# ---- Load tier timings and SIPp stats --------------------------------------
header = (
    f"{'Rate':<8} {'Calls':>6} {'OK':>6} {'Fail':>5} "
    f"{'AvgCPU':>8} {'PeakCPU':>9} {'AvgMem':>8} {'PeakMem':>9} {'AvgRSS':>8} {'PeakRSS':>9}"
)
sep = "-" * len(header)

rows = []
with open(timing_file) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        start_ms, end_ms, rate, logbase = line.split('|')
        start_ms, end_ms, rate = int(start_ms), int(end_ms), int(rate)

        # SIPp stats CSV
        attempted = completed = failed = 0
        stats_csv = logbase + '_stats.csv'
        if os.path.exists(stats_csv):
            with open(stats_csv) as sc:
                rdr = csv.DictReader(sc, delimiter=';')
                last = None
                for r in rdr:
                    last = r
                if last:
                    def col(name, d=last):
                        for k in d:
                            if name.lower() in k.lower():
                                try: return int(float(d[k]))
                                except: pass
                        return 0
                    attempted = col('TotalCallCreated')
                    completed = col('SuccessfulCall(C)')
                    failed    = col('FailedCall(C)')

        ac, pc, am, pm, ar, pr = stats_in_window(start_ms, end_ms)

        rows.append({
            'rate': rate, 'attempted': attempted,
            'completed': completed, 'failed': failed,
            'avg_cpu': ac, 'peak_cpu': pc,
            'avg_mem': am, 'peak_mem': pm,
            'avg_rss': ar, 'peak_rss': pr,
            'duration_s': (end_ms - start_ms) / 1000,
        })

# ---- Print table -----------------------------------------------------------
output_lines = [
    "",
    "=" * len(header),
    "  SIPREC Stress Test — Resource Summary",
    "=" * len(header),
    header,
    sep,
]

for r in rows:
    line = (
        f"{str(r['rate'])+'cps':<8} {r['attempted']:>6} {r['completed']:>6} {r['failed']:>5} "
        f"{r['avg_cpu']:>7.1f}% {r['peak_cpu']:>8.1f}% "
        f"{r['avg_mem']:>7.2f}% {r['peak_mem']:>8.2f}% "
        f"{r['avg_rss']:>6.0f} MB {r['peak_rss']:>7.0f} MB"
    )
    output_lines.append(line)

output_lines += [sep, ""]

# Additional per-tier detail
output_lines.append("  Per-tier detail:")
output_lines.append(f"  {'Rate':<8} {'Duration':>10} {'ActualCPS':>10}")
for r in rows:
    actual_cps = r['attempted'] / r['duration_s'] if r['duration_s'] > 0 else 0
    output_lines.append(f"  {str(r['rate'])+'cps':<8} {r['duration_s']:>9.1f}s {actual_cps:>9.1f}")

output_lines.append("")

text = "\n".join(output_lines)
print(text)

with open(summary_file, 'w') as f:
    f.write(text + "\n")

print(f"Summary saved to: {summary_file}")
PYEOF

rm -f "$TIER_TIMING_FILE"

log "Done. Full monitor data: $MONITOR_RAW"
