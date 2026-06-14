#!/usr/bin/env bash
# =============================================================================
# generate_pcap.sh — Generate a G.711 µ-law RTP PCAP for SIPp audio tests
# =============================================================================
# Creates a 30-second G.711 µ-law (PCMU) RTP PCAP file that SIPp replays
# when running siprec_uac_audio.xml.
#
# Requirements (one of):
#   Option A: ffmpeg  (recommended)
#   Option B: sox + tshark/tcpdump
#   Option C: sox alone (produces raw .ulaw; SIPp can wrap it as RTP with -mi)
#
# The script tries each tool in order and uses the first available.
#
# Output:
#   ../audio/g711_8k_mono.pcap   — PCAP for SIPp play_pcap_audio
#   ../audio/g711_8k_mono.ulaw   — raw PCMU file (fallback / for inspection)
#
# Usage:
#   ./scripts/generate_pcap.sh [duration_seconds]
#   ./scripts/generate_pcap.sh 60           # 60-second tone
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUDIO_DIR="$(dirname "$SCRIPT_DIR")/audio"

DURATION=${1:-30}
RAW_FILE="$AUDIO_DIR/g711_8k_mono.ulaw"
PCAP_FILE="$AUDIO_DIR/g711_8k_mono.pcap"

# RTP parameters that must match the SDP offer
RTP_PT=0            # PCMU payload type
RTP_CLOCK=8000      # Hz
PTIME=20            # ms per packet → 160 samples/packet
SRC_IP=127.0.0.1
SRC_PORT=20100
DST_IP=127.0.0.1
DST_PORT=20200

log() { echo "[$(date '+%H:%M:%S')] $*"; }
die() { echo "[ERROR] $*" >&2; exit 1; }

mkdir -p "$AUDIO_DIR"

# ---------------------------------------------------------------------------
# Step 1 — Generate raw G.711 µ-law audio
# ---------------------------------------------------------------------------

generate_raw_ulaw() {
    local out="$1"

    if command -v ffmpeg &>/dev/null; then
        log "Using ffmpeg to generate ${DURATION}s PCMU tone → $out"
        ffmpeg -y -loglevel error \
            -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=$DURATION" \
            -acodec pcm_mulaw -ar 8000 -ac 1 \
            -f mulaw "$out"
        return
    fi

    if command -v sox &>/dev/null; then
        log "Using sox to generate ${DURATION}s PCMU tone → $out"
        # Generate a 440 Hz tone, encode as G.711 µ-law raw
        sox -n -r 8000 -c 1 -e u-law -b 8 "$out" \
            synth "$DURATION" sine 440 2>/dev/null
        return
    fi

    die "Neither ffmpeg nor sox is installed. Install one:
  apt-get install ffmpeg
  apt-get install sox"
}

generate_raw_ulaw "$RAW_FILE"
log "Raw PCMU file: $RAW_FILE  ($(wc -c < "$RAW_FILE") bytes)"

# ---------------------------------------------------------------------------
# Step 2 — Wrap raw PCMU bytes into an RTP PCAP
# ---------------------------------------------------------------------------
# Each PCAP packet = 20 ms = 160 bytes of G.711 payload.
# We build the PCAP using Python's scapy (lightweight, usually available),
# falling back to a shell + hex approach if scapy is missing.

BYTES_PER_PKT=$(( RTP_CLOCK * PTIME / 1000 ))   # 160
TOTAL_BYTES=$(wc -c < "$RAW_FILE")
NUM_PKTS=$(( TOTAL_BYTES / BYTES_PER_PKT ))

log "Wrapping into RTP packets: ${NUM_PKTS} pkts × ${BYTES_PER_PKT} bytes (PT=$RTP_PT)"

generate_pcap_python() {
    python3 - <<PYEOF
import struct, time, sys, os

RAW  = "$RAW_FILE"
OUT  = "$PCAP_FILE"
PT   = $RTP_PT
PTIME_MS = $PTIME
CLOCK    = $RTP_CLOCK
SRC_IP   = "$SRC_IP"
SRC_PORT = $SRC_PORT
DST_IP   = "$DST_IP"
DST_PORT = $DST_PORT
PKT_BYTES = $BYTES_PER_PKT

def ip_cksum(header):
    if len(header) % 2:
        header += b'\\x00'
    s = 0
    for i in range(0, len(header), 2):
        s += (header[i] << 8) + header[i+1]
    s = (s >> 16) + (s & 0xFFFF)
    s += s >> 16
    return ~s & 0xFFFF

def ip4(src, dst):
    parts_s = list(map(int, src.split('.')))
    parts_d = list(map(int, dst.split('.')))
    return bytes(parts_s), bytes(parts_d)

src_bytes, dst_bytes = ip4(SRC_IP, DST_IP)

with open(RAW, 'rb') as f:
    payload_all = f.read()

num_pkts = len(payload_all) // PKT_BYTES

# PCAP global header
PCAP_MAGIC   = 0xa1b2c3d4
PCAP_VER_MAJ = 2
PCAP_VER_MIN = 4
PCAP_THISZONE = 0
PCAP_SIGFIGS  = 0
PCAP_SNAPLEN  = 65535
PCAP_NETWORK  = 1   # LINKTYPE_ETHERNET

pcap_global = struct.pack('<IHHiIII',
    PCAP_MAGIC, PCAP_VER_MAJ, PCAP_VER_MIN,
    PCAP_THISZONE, PCAP_SIGFIGS, PCAP_SNAPLEN, PCAP_NETWORK)

# Ethernet header (dummy MAC addresses, EtherType=IPv4)
ETH_HDR = b'\\x00' * 6 + b'\\x00' * 6 + b'\\x08\\x00'

ssrc      = 0xABCD1234
seq       = 0
timestamp = 0
base_ts   = int(time.time())

packets = []

for i in range(num_pkts):
    rtp_payload = payload_all[i*PKT_BYTES:(i+1)*PKT_BYTES]

    # RTP header (12 bytes)
    rtp_hdr = struct.pack('!BBHII',
        0x80,          # V=2 P=0 X=0 CC=0
        PT & 0x7F,     # M=0, PT
        seq & 0xFFFF,
        timestamp & 0xFFFFFFFF,
        ssrc)
    rtp_pkt = rtp_hdr + rtp_payload

    # UDP header
    udp_len = 8 + len(rtp_pkt)
    udp_hdr = struct.pack('!HHHH', SRC_PORT, DST_PORT, udp_len, 0)

    # IPv4 header (no options)
    total_len = 20 + udp_len
    ip_hdr_no_cksum = struct.pack('!BBHHHBBH4s4s',
        0x45, 0, total_len,
        i & 0xFFFF, 0, 64, 17, 0,
        src_bytes, dst_bytes)
    cksum = ip_cksum(ip_hdr_no_cksum)
    ip_hdr = struct.pack('!BBHHHBBH4s4s',
        0x45, 0, total_len,
        i & 0xFFFF, 0, 64, 17, cksum,
        src_bytes, dst_bytes)

    # UDP checksum = 0 (optional)
    udp_hdr = struct.pack('!HHHH', SRC_PORT, DST_PORT, udp_len, 0)

    frame = ETH_HDR + ip_hdr + udp_hdr + rtp_pkt

    # PCAP packet record header
    pkt_sec  = base_ts + (i * PTIME_MS // 1000)
    pkt_usec = (i * PTIME_MS % 1000) * 1000
    pkt_hdr  = struct.pack('<IIII', pkt_sec, pkt_usec, len(frame), len(frame))
    packets.append(pkt_hdr + frame)

    seq       = (seq + 1) & 0xFFFF
    timestamp = (timestamp + PKT_BYTES) & 0xFFFFFFFF

with open(OUT, 'wb') as f:
    f.write(pcap_global)
    for p in packets:
        f.write(p)

print(f"Wrote {num_pkts} RTP packets to {OUT}")
PYEOF
}

if command -v python3 &>/dev/null; then
    generate_pcap_python
    log "PCAP written: $PCAP_FILE"
else
    log "WARNING: python3 not found; PCAP not generated."
    log "         Only raw .ulaw file is available: $RAW_FILE"
    log "         Install python3 or use tshark/editcap to build the PCAP manually."
    log ""
    log "  Manual instructions:"
    log "    1.  Convert .ulaw to WAV:  sox -t ul -r 8000 -c 1 $RAW_FILE $AUDIO_DIR/tone.wav"
    log "    2.  Open Wireshark, record one RTP stream, replace payload with the WAV."
    log "    3.  Or use rtptools:  rtpsend -T -f $RAW_FILE -s 127.0.0.1/20200 ..."
fi

echo
log "Files in $AUDIO_DIR:"
ls -lh "$AUDIO_DIR"
