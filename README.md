# SIPREC Recorder — Control API

The recorder exposes a small pod-local HTTP API for controlling an active SIPREC
recording session: switching between default file recording and Google Agent
Assist, and pausing/resuming whichever of those two flows is currently active.

- Base URL: `http://<pod-ip>:<http_listen_addr port>` (default `0.0.0.0:8080`, see `config.example.yaml`)
- All endpoints accept and return `application/json`
- Calls are identified end-to-end by the original SIP `Call-ID` (`call_id`) — no other identifier is needed
- The API is **pod-local**: a call's session only lives in the pod that answered its INVITE. An external router/gateway is expected to resolve which pod owns a `call_id` (via the Redis-backed call locator) before calling these endpoints.

## Authentication

If `api_auth_token` is set in config, every request must include one of:

```
Authorization: Bearer <api_auth_token>
```
or
```
X-API-Token: <api_auth_token>
```

If `api_auth_token` is empty, the API is unauthenticated. Requests that fail auth get `401 Unauthorized`.

## Call flow

```
              POST /v1/agent-assist/start
   recording ─────────────────────────────► agent_assist
              ◄─────────────────────────────
              POST /v1/agent-assist/stop  (or automatic fallback on stream error)

   POST /v1/pause   → freezes whichever flow is currently active (no file/segment rotation, no upload)
   POST /v1/resume  → restores it exactly as it was
```

- Switching **into** Agent Assist closes and uploads the current recording file(s) plus a segment metadata JSON.
- Switching **out of** Agent Assist (via `/stop`, automatic fallback, or call end) uploads **metadata only** — Agent Assist mode never writes a local recording file, so there is nothing else to upload.
- Ending the call while in default recording mode uploads the final recording file(s) plus metadata, same as any other recording→end transition.
- Pause/resume never rotates segments, never creates new files/conversations, and never triggers a GCS upload — it only stops/resumes feeding RTP to whichever sink (file or Agent Assist stream) is currently installed.
- `POST /v1/agent-assist/start` and `/stop` are rejected with `409` while a call is paused — resume first.

## Endpoints

### `GET /healthz`

Liveness check. No auth required.

**Response** `200 OK`
```json
{ "status": "ok" }
```

---

### `POST /v1/agent-assist/start`

Switches the call from default recording to Google Agent Assist. Calling it again while already in Agent Assist mode **restarts** it: the current conversation is gracefully completed (and its segment metadata JSON written/uploaded) and a brand-new conversation is started immediately — the call never bounces back through recording mode in between.

**Request**
```json
{
  "call_id": "abc123@example.com",
  "metadata": { "ticket": "T-4821" }
}
```
| Field | Type | Required | Notes |
|---|---|---|---|
| `call_id` | string | yes | SIP Call-ID of the session |
| `metadata` | object | no | Forwarded to Dialogflow as virtual agent parameters and recorded on the segment |

**Response** `200 OK`
```json
{
  "call_id": "abc123@example.com",
  "agent_assist_conversation_id": "conv-xyz",
  "state": "agent_assist"
}
```

**Errors**
| Status | When |
|---|---|
| `400` | missing/empty `call_id`, malformed JSON |
| `401` | missing/invalid auth |
| `404` | no session found for `call_id` |
| `409` | call is paused, or call is closed |
| `500` | Agent Assist backend error (e.g. Dialogflow conversation creation failed) |

---

### `POST /v1/agent-assist/stop`

Switches the call back from Agent Assist to default recording (a fresh recording file/segment is started — it does not resume the pre-Agent-Assist file). Idempotent: calling it while already in recording mode returns the current state without error.

**Request**
```json
{ "call_id": "abc123@example.com" }
```

**Response** `200 OK`
```json
{
  "call_id": "abc123@example.com",
  "agent_assist_conversation_id": "conv-xyz",
  "state": "recording"
}
```
`agent_assist_conversation_id` is the ID of the conversation that was just ended.

**Errors**
| Status | When |
|---|---|
| `400` | missing/empty `call_id`, malformed JSON |
| `401` | missing/invalid auth |
| `404` | no session found for `call_id` |
| `409` | call is paused, or call is not currently in `agent_assist` mode |

Note: a stream failure on the Agent Assist side (e.g. Dialogflow disconnect) triggers this same transition automatically, with no client action required.

---

### `POST /v1/pause`

Pauses whichever flow is currently active (default recording or Agent Assist) without closing the current segment, rotating any file/conversation, or uploading anything. Idempotent: pausing an already-paused call is a no-op.

**Request**
```json
{ "call_id": "abc123@example.com" }
```

**Response** `200 OK`
```json
{
  "call_id": "abc123@example.com",
  "state": "recording",
  "paused": true
}
```
`state` reflects the underlying mode (`recording` or `agent_assist`), unaffected by pause.

**Errors**
| Status | When |
|---|---|
| `400` | missing/empty `call_id`, malformed JSON |
| `401` | missing/invalid auth |
| `404` | no session found for `call_id` |
| `409` | call has already ended |

---

### `POST /v1/resume`

Restores the sink (file or Agent Assist stream) that was active immediately before the matching `/v1/pause` call. Idempotent: resuming a call that isn't paused is a no-op.

**Request**
```json
{ "call_id": "abc123@example.com" }
```

**Response** `200 OK`
```json
{
  "call_id": "abc123@example.com",
  "state": "recording",
  "paused": false
}
```

**Errors**
| Status | When |
|---|---|
| `400` | missing/empty `call_id`, malformed JSON |
| `401` | missing/invalid auth |
| `404` | no session found for `call_id` |
| `409` | call has already ended |

## Configuration

See `config.example.yaml` for the full set of options. API-relevant fields:

| Field | Default | Purpose |
|---|---|---|
| `http_listen_addr` | `0.0.0.0:8080` | Address this API listens on |
| `api_auth_token` | *(empty)* | Bearer token required on every request; unset disables auth |
| `api_advertise_ip` | *(auto-detected)* | IP stored (as `ip:port`, paired with the `http_listen_addr` port) in the Redis call locator so an external router can find the pod owning a `call_id` |
| `agent_assist_project_id` / `agent_assist_conversation_profile_id` | *(empty)* | Required to enable `/v1/agent-assist/*`; if unset, start requests fail with a disabled-client error |

## Docker

Build the image:

```sh
docker build -t siprec-recorder:latest .
```

Run it on the host network, pointing at a mounted GCP service account key via
`GOOGLE_APPLICATION_CREDENTIALS` (Application Default Credentials — used for both
GCS uploads and Agent Assist/Dialogflow when `gcp_credentials_file` is left empty
in config):

```sh
docker run -d \
  --name siprec-recorder \
  --network host \
  -v /path/to/service-account.json:/secrets/gcp-credentials.json:ro \
  -e GOOGLE_APPLICATION_CREDENTIALS=/secrets/gcp-credentials.json \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -v siprec-recordings:/app/recordings \
  siprec-recorder:latest
```

- `--network host` shares the host's network stack directly (no `-p` mappings needed or honored) — SIP (`5060/udp`), the RTP range (`10000-11000/udp`, must match `rtp_port_start`/`rtp_port_end` in `config.yaml`), and the control API (`8080/tcp`) all bind straight to the host. This matches the `hostNetwork: true` deployment mode the recorder's SIP Contact/media-IP auto-detection assumes in Kubernetes (see `server.go`'s `detectMediaIP`).
- `--network host` is Linux-only; it isn't supported the same way on Docker Desktop for Mac/Windows — use `-p` port mappings there instead.
- Mount your own `config.yaml` over the image's default (`config.example.yaml`) to set `gcs_bucket`, `agent_assist_project_id`, etc.
- The `siprec-recordings` named volume persists `/app/recordings` (local `.ulaw`/metadata files awaiting or bypassing GCS upload) across container restarts.
