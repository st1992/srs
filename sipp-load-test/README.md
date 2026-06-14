# SIPREC SIPp Load Test

End-to-end load testing for the **StreamLink SIPREC recorder** using [SIPp](https://sipp.sourceforge.net/).  
SIPp simulates a **Session Recording Client (SRC / SBC)** sending SIPREC INVITEs to StreamLink (the Session Recording Server / SRS).

---

## Directory structure

```
sipp-load-test/
├── README.md                       ← you are here
├── config.env                      ← all tuneable parameters
├── scenarios/
│   ├── siprec_uac.xml              ← signaling-only (highest CPS)
│   └── siprec_uac_audio.xml        ← signaling + RTP audio (full pipeline)
├── scripts/
│   ├── run_smoke.sh                ← single-call sanity check
│   ├── run_load.sh                 ← sustained load test
│   ├── run_stress.sh               ← ramp-up stress test
│   └── generate_pcap.sh            ← generate G.711 audio PCAP for audio tests
├── audio/                          ← generated PCAP files go here
└── logs/                           ← SIPp output (auto-created at runtime)
```

---

## Prerequisites

### 1. Install SIPp

```bash
# Debian / Ubuntu
sudo apt-get install sipp

# CentOS / RHEL
sudo yum install sipp

# macOS (Homebrew)
brew install sipp

# Verify installation
sipp -v
```

For the **audio scenario** (`siprec_uac_audio.xml`) SIPp must be compiled with PCAP support.  
Check: `sipp -v 2>&1 | grep -i pcap` — should print `pcap`.

### 2. Make scripts executable

```bash
chmod +x sipp-load-test/scripts/*.sh
```

### 3. Start StreamLink

```bash
# From the streamlink root
go run . -config config.yaml
```

Verify it is listening:
```bash
nc -u -z 127.0.0.1 5060 && echo "SIP port open"
```

---

## Quick start

All commands are run from the **`sipp-load-test/`** directory.

```bash
cd sipp-load-test
```

### Smoke test (1 call)

Verifies the full SIPREC signaling flow against a local StreamLink:

```bash
./scripts/run_smoke.sh
```

Expected output: `✓ Smoke test PASSED`

After the smoke test, check that StreamLink created two `.ulaw` files:
```bash
ls ../recordings/*.ulaw
```

### Sustained load test (default profile)

Runs 500 calls at 10 CPS with a 10-second hold time:

```bash
./scripts/run_load.sh
```

Override any parameter on the command line:

```bash
# 200 calls at 20 CPS, 15-second hold, against a remote server
./scripts/run_load.sh -t 35.238.183.188 -r 20 -m 200 -d 15000

# See all options
./scripts/run_load.sh -h
```

### Stress / ramp test

Ramps from 5 CPS to 100 CPS in 5-CPS steps (configurable), running each tier for 30 seconds:

```bash
./scripts/run_stress.sh
```

Prints a per-tier summary table:

```
Rate     Attempted    Completed    Failed     AvgRTD(ms)
----     ---------    ---------    ------     ----------
5cps     150          150          0          12.3        ok
10cps    300          298          2          14.7        ok
15cps    450          441          9          18.1        ok
...
```

Adjust the ramp parameters:
```bash
./scripts/run_stress.sh -s 10 -e 50 -x 10 -n 60
#  start=10 cps, max=50 cps, step=10, 60s per tier
```

---

## Configuration

Edit `config.env` to change defaults for all scripts:

| Variable | Default | Purpose |
|---|---|---|
| `SIPP_TARGET_IP` | `127.0.0.1` | StreamLink IP |
| `SIPP_TARGET_PORT` | `5060` | StreamLink SIP UDP port |
| `LOCAL_IP` | `127.0.0.1` | SIPp signaling IP |
| `LOCAL_PORT` | `5080` | SIPp SIP UDP port |
| `MEDIA_PORT_START` | `20100` | First SIPp RTP port |
| `CALL_DURATION_MS` | `10000` | Hold time per call (ms) |
| `SCENARIO` | `siprec_uac.xml` | Scenario file to use |
| `LOAD_CALLS` | `500` | Total calls (load test) |
| `LOAD_RATE` | `10` | Calls per second (load test) |
| `LOAD_MAX_CONCURRENT` | `50` | Simultaneous calls cap |
| `STRESS_RATE_START` | `5` | Starting CPS (stress test) |
| `STRESS_RATE_MAX` | `100` | Max CPS (stress test) |
| `STRESS_RATE_STEP` | `5` | CPS increment per tier |
| `STRESS_RAMP_INTERVAL` | `30` | Seconds per tier |
| `LOG_DIR` | `./logs` | Output directory |

### Port planning

Each concurrent call consumes **4 UDP ports** (2 streams × RTP + RTCP).  
With `LOAD_MAX_CONCURRENT=50` you need ports `20100–20299`.

**StreamLink** uses `rtp_port_start–rtp_port_end` (default `10000–20000`).  
**SIPp** must use a **non-overlapping** range — the default `20100+` is fine.

---

## Scenarios

### `siprec_uac.xml` — Signaling only

Best for measuring **maximum calls per second (CPS)** and server signaling capacity.

- Sends `INVITE` with `Require: siprec` and `Contact: +sip.src`
- Body: `multipart/mixed` — dual-stream SDP (sendonly) + valid rs-metadata XML
- Completes `100 Trying → 200 OK → ACK → pause → BYE → 200 OK`
- **No RTP** — StreamLink receives valid signaling but creates empty `.ulaw` files

```bash
./scripts/run_load.sh -s siprec_uac.xml -r 50 -m 1000 -l 100
```

### `siprec_uac_audio.xml` — With RTP audio

Exercises the **full recording pipeline**: signaling + RTP reception + file writes.

- Same signaling as above
- After ACK: streams G.711 µ-law audio from a PCAP to StreamLink's RTP ports
- StreamLink writes non-empty `.ulaw` files
- Requires SIPp with PCAP support and a pre-generated audio file

#### Set up the audio PCAP

```bash
# Generate a 30-second G.711 test tone
./scripts/generate_pcap.sh 30

# Verify
ls -lh audio/g711_8k_mono.pcap
```

Requirements for `generate_pcap.sh`:
- `ffmpeg` **or** `sox` — to generate the raw G.711 audio
- `python3` — to wrap it into an RTP PCAP (no extra packages required)

#### Run with audio

```bash
./scripts/run_load.sh -s siprec_uac_audio.xml -r 5 -m 50 -d 15000
```

Use a lower rate than the signaling-only scenario because each call actively
writes to disk.

---

## Understanding the SIPREC INVITE

Each SIPp call sends an `INVITE` with this structure:

```
INVITE sip:35.238.183.188:5060 SIP/2.0
Require: siprec
Contact: <sip:src@...;+sip.src>             ← SIPREC SRC marker
Content-Type: multipart/mixed;boundary="siprec-boundary"

--siprec-boundary
Content-Type: application/sdp               ← Part 1: dual-stream SDP

v=0
o=sipp 0 0 IN IP4 <local_ip>
m=audio <port1> RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendonly
a=label:inbound                             ← inbound leg label
m=audio <port2> RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendonly
a=label:outbound                            ← outbound leg label

--siprec-boundary
Content-Type: application/rs-metadata+xml   ← Part 2: RFC 7865 metadata
Content-Disposition: recording-session

<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns='urn:ietf:params:xml:ns:recording:1'>
  <session session_id="sess-N"> ...
  <participant participant_id="caller-N"> ...
  <stream stream_id="stream-in-N" ...> <label>inbound</label> ...
  ...
</recording>
--siprec-boundary--
```

StreamLink's `siprec_helpers.go` extracts the SDP, allocates two RTP ports,
and answers with a `200 OK` containing a combined `recvonly` SDP.

---

## Logs and output

Each run produces timestamped files in `logs/`:

| File | Contents |
|---|---|
| `*_run.log` | SIPp screen output (call rates, counters) |
| `*_stats.csv` | Per-period statistics (parse with spreadsheet) |
| `*_errors.log` | Unexpected SIP responses / timeouts |
| `*_messages.log` | Full SIP message trace (smoke test only) |

Read the stats CSV to extract metrics like:
- `SuccessfulCall` — calls that completed BYE cleanly
- `FailedCall` — calls that hit errors or timeouts
- `ResponseTime1` — INVITE → 200 OK latency distribution

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `FAILED calls` in stats | StreamLink rejects INVITEs | Check StreamLink logs; confirm `Require: siprec` is parsed correctly |
| Timeout on `recv 200` | StreamLink not running / wrong IP | Verify with `netcat -u` or `nmap -sU -p 5060` |
| `port already in use` | SIPp local port conflict | Change `LOCAL_PORT` or `MEDIA_PORT_START` in config.env |
| Audio scenario fails | SIPp missing PCAP support | Rebuild SIPp with `--enable-pcap`; or use signaling-only scenario |
| `play_pcap_audio` errors | PCAP file not found | Run `./scripts/generate_pcap.sh` |
| High `FailedCall` count under load | Port exhaustion on StreamLink | Widen `rtp_port_start–rtp_port_end` in `config.yaml` |
| `[media_port+2]` not replaced | Old SIPp version | Upgrade to SIPp ≥ 3.4 |

---

## Key metrics to watch during load tests

- **CPS (calls per second)** — max sustainable without `FailedCall` increase
- **INVITE → 200 OK latency** — `ResponseTime1` in stats CSV; target < 50 ms
- **Concurrent calls** — watch StreamLink's goroutine count and port usage
- **Disk I/O** — `iostat -x 1` while running audio tests; streaming writes can saturate disk
- **Memory** — each open session holds two RTP buffers; watch RSS under high concurrency
