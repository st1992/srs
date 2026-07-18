# agentassist-check

Standalone smoke test for Dialogflow's `BidiStreamingAnalyzeContent` API. It
sends the exact same request shape as `agentassist.go` — `CreateConversation`
-> `CreateParticipant` -> a bidi stream with a `Config` message followed by
audio chunks — with none of the SIP/RTP/Kubernetes machinery around it, so a
conversation-profile or IAM/network problem can be isolated in seconds
instead of a full rebuild-and-redeploy cycle.

## Build

```bash
go build -o agentassist-check ./agentassist-check/
```

## Run

Against the real cluster's identity, run this from a shell that already has
that Workload Identity / ADC context (e.g. the debug pod used earlier), or
pass `-credentials-file` pointing at a service account key.

```bash
./agentassist-check \
  -project=adtgcp-ent-dev-ccs-tlcm-fe10 \
  -location=us-central1 \
  -profile=wpooK6HETcOne9L60VjVeA \
  -role=HUMAN_AGENT \
  -encoding=MULAW \
  -sample-rate=8000
```

By default it streams 25 chunks (~0.5s) of silence. To replay real recorded
audio instead, pass `-audio-file` pointing at a raw `.ulaw` (or `.pcm` for
LINEAR16) file — e.g. one of the recordings already written to
`recorder.config.recordingDir` on a live pod.

## Using it to isolate the "Internal error encountered" issue

1. Run it once against the current profile (`wpooK6HETcOne9L60VjVeA`) — it
   should reproduce the same `RECV ERROR: rpc error: code = Internal desc =
   Internal error encountered.` seen in the recorder logs, confirming the
   issue isn't specific to the recorder's Go code.
2. Create (or point `-profile` at) a bare-bones conversation profile with
   `useBidiStreaming: true` and no `generators` / `CONVERSATION_SUMMARIZATION`
   features, then rerun. If that succeeds, it confirms the Generators /
   summarization feature combination on the original profile is what's
   triggering the backend error — solid grounds for a Google Cloud Support
   case scoped to that profile.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-project` | *(required)* | GCP project ID |
| `-profile` | *(required)* | Conversation profile ID |
| `-location` | `global` | Dialogflow location; non-`global` values switch to the regional API endpoint |
| `-role` | `HUMAN_AGENT` | Participant role: `HUMAN_AGENT` or `END_USER` |
| `-encoding` | `MULAW` | Audio encoding: `MULAW` or `LINEAR16` |
| `-sample-rate` | `8000` | Audio sample rate (Hz) |
| `-audio-file` | *(none)* | Raw audio bytes to stream; omit to stream silence |
| `-chunks` | `25` | Number of ~20ms silence chunks when `-audio-file` is omitted |
| `-credentials-file` | *(none)* | Path to a service account JSON key; omit to use ADC/Workload Identity |
