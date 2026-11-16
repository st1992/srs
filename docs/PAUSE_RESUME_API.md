# Pause / Resume API

Pod-local HTTP endpoints that temporarily replace a live call's audio with
silence (for example while a customer reads out card details) and then switch
back. Same pod-discovery model as `/v1/recording/split` and the Agent Assist
API: look up the owning pod via the Redis call locator (`locator.go`), then
call these endpoints directly on that pod's `http_listen_addr` (default port
`8080`).

Pausing doesn't stop capture. Each RTP packet the SBC sends is still written,
but its audio is replaced with μ-law silence (`0xFF`) of the same length. The
paused stretch therefore keeps its real duration in the recording, and later
audio stays aligned with the call's timeline.

## POST /v1/recording/pause

Starts writing silence in place of the call audio on every leg of the call.

```json
// Request
{ "call_id": "12344555" }

// 200 OK
{ "call_id": "12344555_438274632_47324923@10.10.10.153", "state": "recording", "paused": true }
```

## POST /v1/recording/resume

Switches every leg back to writing the call audio, into the same files (or
Agent Assist stream) that were in use during the pause.

```json
// Request
{ "call_id": "12344555" }

// 200 OK
{ "call_id": "12344555_438274632_47324923@10.10.10.153", "state": "recording", "paused": false }
```

## Request and response

| Field | Description |
|---|---|
| `call_id` (request) | Required. The full SIP Call-ID, or just its leading `_`-delimited prefix (e.g. `12344555` for `12344555_438274632_47324923@10.10.10.153`), same as `/v1/recording/split` and `/v1/agent-assist/*`. If several live calls share a prefix, the most recent one is used. |
| `call_id` (response) | Always the full, resolved Call-ID. |
| `state` | The call's current mode: `recording` or `agent_assist`. |
| `paused` | Whether the call is paused after this request. |

Both endpoints are **idempotent**: pausing a call that is already paused, or
resuming one that isn't paused, returns `200` with the current state and
changes nothing.

### Errors

Errors are returned as `{"error": "<message>"}`.

| Status | When |
|---|---|
| `400` | Body isn't valid JSON, has unknown fields, or `call_id` is missing/blank. |
| `404` | No live call matches `call_id`. |
| `405` | Method other than `POST`. |
| `409` | The call has already ended. |

## Behaviour

- **Same duration, silent content.** Every received PCMU packet is replaced
  with silence of identical length. DTMF and other non-PCMU packets are
  skipped exactly as when not paused.
- **Gaps stay gaps.** If the SBC sends no RTP for a while (paused or not),
  nothing is written for that time; pause doesn't generate extra audio.
- **Works in both modes.** In `recording` mode the silence goes into the
  `.ulaw` files; in `agent_assist` mode it is what's streamed to Dialogflow,
  so no transcripts are produced while paused, and the stream keeps running.
- **Carries over through mode changes.** The pause belongs to the call's legs,
  not to a segment. It stays in effect across `/v1/recording/split`, a
  re-INVITE, `/v1/agent-assist/start` and `/stop`, and an automatic fallback
  from Agent Assist to recording, until `/resume` is called or the call ends.
- **Applies to all legs.** Both the customer and the agent legs are silenced together.
- Each pause and resume is logged at Info as `recording paused` /
  `recording resumed` with `sipCallID` and `mode`.

## Pause intervals in segment metadata

Each segment's metadata JSON (uploaded to the metadata bucket alongside the
`.ulaw` files) lists the paused stretches under `segment.pauses`:

```json
"segment": {
  "sequence": 0,
  "mode": "recording",
  "start_time": "2026-11-16T10:00:00.000Z",
  "end_time": "2026-11-16T10:12:00.000Z",
  "pauses": [
    { "start_time": "2026-11-16T10:03:10.120Z", "end_time": "2026-11-16T10:04:02.481Z" }
  ]
}
```

- `pauses` is left out when the segment had no pause.
- A pause that is still running when a segment ends (split, Agent Assist
  start/stop, or call end) is closed at the segment's `end_time`. If the call
  continues paused, the next segment's `pauses` starts with an interval
  beginning at its own `start_time`. A pause spanning a split therefore shows
  up as two intervals that meet at the split.

## Examples

```bash
curl -s -X POST http://<recorder-pod>:8080/v1/recording/pause \
  -H 'Content-Type: application/json' \
  -d '{"call_id":"12344555"}'
```

```bash
curl -s -X POST http://<recorder-pod>:8080/v1/recording/resume \
  -H 'Content-Type: application/json' \
  -d '{"call_id":"12344555"}'
```
