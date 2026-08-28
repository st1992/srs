# Agent Assist API

Pod-local HTTP endpoints that reroute a live call's audio between recording
and Google Agent Assist (Dialogflow ES). Same pod-discovery model as
`/v1/recording/split`: look up the owning pod via the Redis call locator
(`locator.go`), then call these endpoints directly on that pod.

## Config

Set in `config.yaml` (see `config.example.yaml`):

| Field | Purpose |
|---|---|
| `agent_assist_project_id` | GCP project hosting the Dialogflow ES agent. |
| `agent_assist_location_id` | Dialogflow region. Default `global`. |
| `agent_assist_conversation_profile_id` | Conversation profile to use. |
| `agent_assist_sample_rate_hertz` | Audio sample rate sent/received. Default `8000` (matches PCMU). |
| `agent_assist_send_queue_packets` | Per-leg outbound buffer depth before `WriteRTPPayload` starts erroring. Default `250`. |
| `agent_assist_end_user_labels` / `agent_assist_human_agent_labels` | Which leg label (e.g. `inbound`/`outbound`) maps to which Dialogflow participant role. |

`gcp_credentials_file` is reused for Dialogflow auth (same credentials as
GCS). If `agent_assist_project_id` or `agent_assist_conversation_profile_id`
is empty, the client is disabled and `/start` returns an error.

## POST /v1/agent-assist/start

Closes the call's current recording segment and reroutes its legs to a new
Dialogflow conversation.

```json
// Request
{ "call_id": "abc123", "metadata": { "ticket": "t1" } }

// 200 OK
{ "call_id": "abc123", "agent_assist_conversation_id": "conv-xyz", "state": "agent_assist" }
```

- `call_id` may be the full Call-ID or just its leading `_`-delimited prefix
  (e.g. `12344555` for `12344555_438274632_47324923@10.10.10.153`), same as
  `/v1/recording/split`. The response always echoes back the full Call-ID.
- Idempotent: calling `/start` again while already in Agent Assist mode
  returns the existing conversation without opening a new one.
- `404` if the call isn't found; `409` if the call already ended, or if a
  concurrent state transition raced it out from under recording mode.

## POST /v1/agent-assist/stop

Ends the call's Dialogflow conversation and reroutes its legs back to new
recording file sinks (starting a new segment).

```json
// Request
{ "call_id": "abc123" }

// 200 OK
{ "call_id": "abc123", "agent_assist_conversation_id": "conv-xyz", "state": "recording" }
```

- `call_id` accepts the same full-or-prefix Call-ID as `/start`.
- Idempotent: calling `/stop` while already recording returns the current
  state without error.
- Also triggered automatically if the Dialogflow stream fails mid-call (the
  recorder falls back to recording rather than silently dropping audio).

## Interaction with `/v1/recording/split`

A call has one mode at a time: `recording` or `agent_assist`. Calling
`POST /v1/recording/split` while a call is in `agent_assist` mode returns
`409 Conflict` — stop Agent Assist first. A re-INVITE arriving mid-Agent-Assist
is a no-op for the audio routing (it just logs) rather than disturbing the
live stream.

No RTP is dropped across either transition: sinks are swapped in place on
the existing leg sockets, the same mechanism `/v1/recording/split` uses.
