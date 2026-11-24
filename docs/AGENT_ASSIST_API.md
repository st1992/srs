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
| `agent_assist_location_id` | Default Dialogflow region; must match the conversation profile's region. Also selects the service endpoint. Default `global`. |
| `agent_assist_conversation_profile_id` | Default conversation profile; a `/start` request can override it together with the region. |
| `agent_assist_sample_rate_hertz` | Audio sample rate sent/received. Default `8000` (matches PCMU). |
| `agent_assist_send_queue_packets` | Per-leg outbound buffer of 20 ms packets that covers a stream reopen; when full, new packets are dropped with a warning and the leg keeps running. Default `1500` (~30 s). |
| `agent_assist_end_user_labels` / `agent_assist_human_agent_labels` | Which leg label (e.g. `inbound`/`outbound`) maps to which Dialogflow participant role. |

`gcp_credentials_file` is reused for Dialogflow auth (same credentials as
GCS). If `agent_assist_project_id` or `agent_assist_conversation_profile_id`
is empty, the client is disabled and `/start` returns an error, even if the
request names a profile.

### Regions

Both global and regional conversation profiles are supported.
The region (`agent_assist_location_id`, or `aa_region` on a `/start` request)
does two things: it goes into the resource name
(`projects/P/locations/L/conversationProfiles/ID`) *and* it selects the
Dialogflow host — `global` uses `dialogflow.googleapis.com`, every other region
uses `<region>-dialogflow.googleapis.com`. Both must agree, because the global
endpoint doesn't serve regional profiles; a regional profile addressed through
the global host fails with `NOT_FOUND` or `INVALID_ARGUMENT`.

Supported values: `global`, `us`, `us-central1`, `us-east1`, `us-west1`,
`northamerica-northeast1`, `europe-west1`, `europe-west2`, `europe-west3`,
`europe-west4`, `europe-west6`, `asia-southeast1`, `asia-southeast2`,
`asia-northeast1`, `asia-south1`, `australia-southeast1`. (`us` is a
multi-region and follows the same pattern: `us-dialogflow.googleapis.com`.)

The value isn't validated against that list, so a new Google region works
without a code change — but a typo only surfaces as a DNS error on the first
`/start` that uses it, which then fails within about 20 seconds. The resolved
endpoint is logged under `component=agent_assist` (at startup for the
configured region, on first use for any other), so check there first:

```
level=INFO component=agent_assist msg="agent assist dialogflow endpoint resolved"
  location=us-central1 endpoint=us-central1-dialogflow.googleapis.com:443
```

Two things worth knowing before picking a region:

- **Regional conversation profiles can't be created in the Agent Assist
  console** — it doesn't support regionalization. Create them via the API or
  `gcloud`; a profile made in the console is global no matter what was selected.
- **CCAI Transcription's data residency is narrower than the region list
  above** — it only covers EU, US, and North America (Canada). This is a voice
  path, so that applies here directly: configuring, say, `asia-south1` does not
  mean transcription data stays in that region.

See [Regionalization and data residency](https://cloud.google.com/agent-assist/docs/regionalization)
for the authoritative list.

## POST /v1/agent-assist/start

Closes the call's current recording segment and reroutes its legs to a new
Dialogflow conversation.

```json
// Request
{
  "call_id": "abc123",
  "metadata": { "ticket": "t1" },
  "aa_conversation_profile_id": "PROFILE_ID", // optional
  "aa_region": "us-central1"                  // optional
}

// 200 OK
{ "call_id": "abc123", "agent_assist_conversation_id": "conv-xyz", "state": "agent_assist" }
```

- `call_id` may be the full Call-ID or just its leading `_`-delimited prefix
  (e.g. `12344555` for `12344555_438274632_47324923@10.10.10.153`), same as
  `/v1/recording/split`. The response always echoes back the full Call-ID.
- `aa_conversation_profile_id` and `aa_region` are optional and pick the
  conversation profile for this conversation only. Send both or neither:

  | Request | Result |
  |---|---|
  | Both set | That profile and region are used |
  | Neither set (missing or blank) | Configured profile and region are used |
  | Only one set | It is ignored, the configured pair is used, and a warning is logged |
  | Both set, one malformed | `400`; the call keeps recording |
  | Both set, rejected by Google | `500`; the call keeps recording. There is no retry on the default profile |

  Pass the bare profile ID, not a `projects/...` name. The project and
  credentials always come from config, so the service account needs access to
  every profile used, and pods need outbound access to every region's endpoint.
- Recorder versions without these fields reject them with `400` (unknown
  field), so upgrade all pods before callers start sending them.
- The fields are ignored if the call is already in Agent Assist mode; to switch
  profiles, call `/stop` and then `/start`.
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

A call paused with `/v1/recording/pause` stays paused across these
transitions, and while in Agent Assist mode the silence is what's streamed to
Dialogflow. See [PAUSE_RESUME_API.md](PAUSE_RESUME_API.md).
