# Logging & Monitoring Reference

Living reference for `siprec-recorder`'s logs and the alerts that should exist
on top of them in GCP Cloud Logging / Cloud Monitoring. Update this file
whenever a log call site, `event` value, or config field changes — it's the
source of truth for what alert policies should exist, not just what the code
currently emits.

**Status legend** used below: ✅ implemented in code · ⬜ not yet configured in
GCP (needs to be created by whoever owns the monitoring project).

## 1. How logs reach GCP

The binary logs structured JSON to stdout (`logging.go`, `newLogger`). On
GKE, the node's Cloud Logging agent scrapes container stdout automatically —
no client library or sidecar needed. Two JSON keys are remapped from slog's
defaults specifically so Cloud Logging parses them as first-class fields
instead of opaque `jsonPayload`:

| slog default | emitted as | why |
|---|---|---|
| `level` | `severity` | Cloud Logging only promotes a field named exactly `severity` to `LogEntry.severity`; anything else stays `DEFAULT` and can't be filtered/alerted on by severity. |
| `msg` | `message` | Not a hard requirement, but Log Explorer's summary-line heuristic looks for this key. |
| `time` | `time` (unchanged) | Already matches Cloud Logging's special `time` field; slog already formats it RFC3339. |

Because `severity` is promoted out of `jsonPayload`, **filter on `severity=`**,
not `jsonPayload.severity=`. Everything else (`event`, `sipCallID`, `reason`,
`file`, etc.) stays inside `jsonPayload` and is filtered as
`jsonPayload.<field>=`.

Base filter for every query below (GKE resource, adjust names to your cluster):

```
resource.type="k8s_container"
resource.labels.container_name="cx-streamlink-rec"
resource.labels.namespace_name="<your-namespace>"
```

**Kamailio proxy note (⬜ not done):** the SIP proxy sidecar logs to syslog by
default inside its container (`Dockerfile.kamailio`), not stdout — it needs
its own redirect before it shows up in Cloud Logging at all. Out of scope of
this doc until that's wired up.

## 2. Log levels

Defined in `logging.go`. Six levels map onto GCP's `LogSeverity` enum
(Alert/Emergency deliberately omitted — those are infra-paging severities,
not something app code should decide to emit):

| Level | slog value | GCP severity | Use for |
|---|---|---|---|
| Trace | -8 | `DEBUG` | reserved, currently unused |
| Debug | -4 | `DEBUG` | raw SDP / rs-metadata bodies on parse failure (interop debugging — see PII note below) |
| Info | 0 | `INFO` | normal lifecycle: call established/ended, upload succeeded, port auto-detect |
| Notice | 2 | `NOTICE` | *(reserved — no current call sites; use for non-fatal-but-worth-a-note conditions, e.g. GCS bucket not configured)* |
| Warn | 4 | `WARNING` | recoverable problems: metadata parse failure, one failed upload attempt, no RTP received in grace period |
| Error | 8 | `ERROR` | request-scoped failures: bad SDP, RTP port exhaustion, upload retries exhausted |
| Critical | 12 | `CRITICAL` | silent data-loss / process-integrity failures: recording died mid-call, stale/leaked session, recovered panic |

Minimum emitted level is set by `log_level` in config (default `info`) — see
§4. `Validate()` rejects unparseable values at startup (`config.go`).

**PII caution:** `Debug`-level logs for SDP/rs-metadata parse failures
(`server.go`, on the `sdp_parse_failed` / metadata-parse-warn paths) include
the raw offending body, which can carry call phone numbers. Don't leave
`log_level: debug` on indefinitely in production; treat Cloud Logging
retention/access controls for this service accordingly if you do.

## 3. Structured `event` field

Lifecycle log lines carry an `event` field so alerts filter on a stable value
instead of matching message text (which can be reworded without anyone
thinking to update an alert). Full catalog, in rough call-flow order:

| `event` | Severity | Meaning | Key fields | Emitted from |
|---|---|---|---|---|
| `call_established` | INFO | SIPREC INVITE accepted, recording started | `files`, `sip_headers`, `siprec_metadata` | `server.go` onInvite success |
| `call_rejected` | WARNING/ERROR | INVITE rejected; `reason` distinguishes cause | `reason` ∈ `not_siprec`, `sdp_extract_failed`, `sdp_parse_failed`, `wrong_media_section_count`, `recorder_setup_failed`, `sdp_combine_failed` | `server.go` onInvite failure paths |
| `call_ended` | INFO | BYE received, session closed normally | `sip_headers`, `siprec_metadata`, `bye_metadata` | `server.go` onBye |
| `port_exhausted` | ERROR | No free RTP port in configured range | `err` | `server.go` onInvite |
| `recording_stalled` | **CRITICAL** | A leg's RTP read/write died while the call may still be active — silent audio loss | `err`, `label`, `file` | `rtp.go` run() |
| `no_rtp_received` | WARNING | Leg negotiated media but got zero packets within `rtp_no_media_timeout_sec` | `timeout_sec`, `label`, `file` | `rtp.go` watchdog |
| `session_stale` | **CRITICAL** | Session open longer than `max_call_duration_hours` with no BYE (leaked session / lost BYE) — flagged only, never auto-terminated | `sipCallID`, `age_hours` | `server.go` staleSessionReaper |
| `panic_recovered` | **CRITICAL** | A goroutine/handler panicked and was recovered instead of crashing the process | `source`, `panic`, `stack` | `logging.go` recoverAndLog (wraps onInvite/onAck/onBye/onOptions and the RTP goroutine) |
| `upload_succeeded` | INFO | File uploaded to GCS | `attempt`, `file`, `object` | `gcs.go` process() |
| `upload_failed` | WARNING | One upload attempt failed, will retry | `attempt`, `err` | `gcs.go` process() |
| `upload_exhausted` | ERROR | All retry attempts failed; file left on local disk for next sweep | `file` | `gcs.go` process() |

## 4. Config reference

All in `config.go` / `config.example.yaml` / `helm/values.yaml`
(`recorder.config.*`):

| Field | YAML key | Default | Effect |
|---|---|---|---|
| `LogLevel` | `log_level` | `info` | Minimum severity emitted (§2) |
| `RTPNoMediaTimeoutSec` | `rtp_no_media_timeout_sec` | `5` | Grace period before `no_rtp_received`; `<=0` disables |
| `MaxCallDurationHours` | `max_call_duration_hours` | `12` | Age threshold for `session_stale`; `<=0` disables |
| `StaleSessionCheckIntervalSec` | `stale_session_check_interval_sec` | `300` | How often the reaper scans for stale sessions |

## 5. Alert policies

None of these exist in GCP yet — this is the build list. Each is a Cloud
Logging **log-based metric** (counter) + a Cloud Monitoring **alerting
policy** on that metric. Ordered by priority.

### 5.1 Page immediately (data loss / process integrity)

| ⬜ | Name | Filter | Suggested condition |
|---|---|---|---|
| ⬜ | `recorder-critical` | `severity="CRITICAL"` | Any occurrence in 1 min |
| ⬜ | `recorder-recording-stalled` | `jsonPayload.event="recording_stalled"` | Any occurrence (redundant with above, but useful as its own metric to see stall rate over time separate from other Criticals) |
| ⬜ | `recorder-session-stale` | `jsonPayload.event="session_stale"` | Any occurrence |
| ⬜ | `recorder-panic` | `jsonPayload.event="panic_recovered"` | Any occurrence |

### 5.2 Investigate soon (capacity / degradation)

| ⬜ | Name | Filter | Suggested condition |
|---|---|---|---|
| ⬜ | `recorder-error-rate` | `severity="ERROR"` | Rate > N per 5 min (tune N to baseline traffic) |
| ⬜ | `recorder-port-exhausted` | `jsonPayload.event="port_exhausted"` | Any occurrence — widen `rtp_port_start`/`rtp_port_end` or scale out |
| ⬜ | `recorder-upload-exhausted` | `jsonPayload.event="upload_exhausted"` | Any occurrence — GCS reachability or permissions problem, recordings piling up on local disk |
| ⬜ | `recorder-no-rtp` | `jsonPayload.event="no_rtp_received"` | Rate > baseline — SBC/network interop issue |
| ⬜ | `recorder-call-rejected` | `jsonPayload.event="call_rejected"` | Rate > baseline, break down by `jsonPayload.reason` — misconfigured SBC vs. a real bug |

### 5.3 Business-outage sentinel (absence of signal)

| ⬜ | Name | Filter | Suggested condition |
|---|---|---|---|
| ⬜ | `recorder-no-calls` | `jsonPayload.event="call_established"` | **Zero** occurrences during an expected-traffic window. This catches "process looks healthy, nothing is erroring, but no calls are actually landing" — the failure mode error-rate alerts miss entirely. |

### 5.4 Reference only (don't alert, useful for dashboards)

`call_established`, `call_ended`, `upload_succeeded`, `upload_failed` (single
attempt failures are expected/retried — only `upload_exhausted` should page).

## 6. Known gaps / follow-ups (not in scope of the logging work done so far)

- ⬜ GCP-side log-based metrics + alert policies above are not created yet.
- ⬜ `recorder-deployment.yaml` liveness/readiness probes are `kill -0 1` —
  proves the process exists, not that the SIP socket is healthy. Should be
  a real UDP/SIP OPTIONS-based check.
- ⬜ Kamailio proxy logs aren't reaching Cloud Logging (syslog, not stdout).
- ⬜ No disk-usage metric on the recordings PVC; if GCS is unreachable long
  enough, local disk fills. `upload_exhausted` is the current early warning,
  but a direct disk-usage alert would catch it earlier.

## 7. Quick debugging recipe

Bump `log_level: debug` in the relevant recorder's ConfigMap (or
`config.yaml` locally), restart the pod, reproduce, then check Log Explorer
with the base filter from §1 plus `jsonPayload.sipCallID="<call-id>"` to pull
every line for one call across INVITE/RTP/upload. Revert to `info` afterward
— see the PII caution in §2.
