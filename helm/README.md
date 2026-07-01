# siprec-stack Helm chart

End-to-end installation guide for the streamlink SIPREC recording stack on GKE.

## Architecture overview

```
SBC / call controller (internal VPC)
          │  SIP INVITE (SIPREC)  UDP 5060
          │  RTP media            UDP 10000-30000
          ▼
┌─────────────────────────────────────────────────────┐
│  GCP Internal Passthrough NLB  (L4, private VIP)    │
│  Frontend IP: 100.73.16.5                            │
│  externalTrafficPolicy: Local → no SNAT              │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────┐
│  Recorder node  (c2-standard-4, hostNetwork)          │
│  DaemonSet — one pod per labelled node                │
│                                                       │
│  • SIP   UDP 5060               (signalling)          │
│  • RTP   UDP 10000-30000        (media)               │
│  • sngrep available in-pod for live SIP capture       │
└──────────┬───────────────────────────────────────────┘
           │
           ▼
   GCS bucket  (recordings + metadata)
```

The recorder DaemonSet sits **directly behind the Internal NLB** — there is
no Kamailio proxy in front of it any more. SBCs send both SIP signalling and
RTP media to the LB VIP `100.73.16.5`; the LB forwards (passthrough,
source-IP preserved) straight to the recorder pod via hostNetwork.

Because a Kubernetes `Service` cannot express a 20 000-port UDP range, the
RTP forwarding rule on `100.73.16.5` must be provisioned out-of-band against
the same shared VIP (see [RTP forwarding rule](#rtp-forwarding-rule) below).

**Traffic path for a new SIPREC session:**

1. SBC sends `INVITE` with the SIPREC multipart body to `100.73.16.5:5060`.
2. NLB forwards the packet (passthrough) to a Ready recorder pod.
3. Recorder answers `200 OK` with `c=IN IP4 100.73.16.5` in SDP so the SBC
   sends RTP back to the same shared VIP.
4. RTP packets traverse the RTP forwarding rule on the shared VIP and land
   on the same recorder node by destination port.

---

## Prerequisites

| Tool | Minimum version | Install |
|---|---|---|
| `gcloud` CLI | 460+ | https://cloud.google.com/sdk/docs/install |
| `kubectl` | 1.28+ | `gcloud components install kubectl` |
| `helm` | 3.14+ | https://helm.sh/docs/intro/install/ |
| Docker (or `docker buildx`) | 24+ | https://docs.docker.com/engine/install/ |

You need:
- A running GKE Standard cluster (Autopilot does not support `hostNetwork: true`).
- A GCP Artifact Registry (or any OCI registry) to push the recorder image.
- `roles/container.developer` + `roles/artifactregistry.writer` on the build principal.

---

## Step 1 — Build and push the SIPREC recorder image

The runtime image is Debian-slim with `sngrep` pre-installed for in-pod SIP
debugging (`kubectl exec -it <pod> -- sngrep -d any port 5060`).

```bash
PROJECT=my-gcp-project
REGION=us-central1
REPO=siprec
IMAGE=${REGION}-docker.pkg.dev/${PROJECT}/${REPO}/siprec-recorder

gcloud auth configure-docker ${REGION}-docker.pkg.dev

docker build \
  --build-arg BUILD_EXPIRES=$(date -d '+3 months' +%Y-%m-%d) \
  -t ${IMAGE}:latest \
  -t ${IMAGE}:$(git rev-parse --short HEAD) \
  .

docker push ${IMAGE}:latest
docker push ${IMAGE}:$(git rev-parse --short HEAD)
```

---

## Step 2 — Prepare the recorder node

The chart uses **node selectors + taints** to isolate the workload. This
setup targets a single recorder node (`c2-standard-4`, 4 vCPU / 16 GiB).

```bash
RECORDER_NODE=gke-cluster-recorder-pool-xxxx   # replace with actual node name

kubectl label node ${RECORDER_NODE} siprec-role=recorder
kubectl taint node ${RECORDER_NODE} siprec-role=recorder:NoSchedule
```

Verify:

```bash
kubectl get nodes -L siprec-role
kubectl describe node ${RECORDER_NODE} | grep -A5 Taints
```

To scale out later, label additional nodes the same way — the DaemonSet
will place one pod per labelled node automatically.

---

## Step 3 — Reserve the shared LB VIP

`100.73.16.5` is used by both the SIP `Service` (managed by this chart)
**and** the out-of-band RTP forwarding rule. Reserve it with the
`SHARED_LOADBALANCER_VIP` purpose so both rules can coexist on the same IP:

```bash
gcloud compute addresses create streamlink-media-vip \
  --region=${REGION} \
  --subnet=<your-subnet> \
  --addresses=100.73.16.5 \
  --purpose=SHARED_LOADBALANCER_VIP
```

---

## Step 4 — Configure `values.yaml`

Copy the shipped defaults and override as needed:

```bash
cp helm/values.yaml helm/my-values.yaml
```

Minimum required overrides:

```yaml
siprecRecorder:
  image:
    repository: us-central1-docker.pkg.dev/my-gcp-project/siprec/siprec-recorder
    tag: "latest"             # or a specific git-sha tag

  service:
    loadBalancerIP: "100.73.16.5"

  config:
    mediaIP: "100.73.16.5"
    gcsBucket: "my-recordings-bucket"
    gcsMetadataBucket: "my-metadata-bucket"
```

### Optional NLB annotations

```yaml
siprecRecorder:
  service:
    annotations:
      networking.gke.io/load-balancer-type: "Internal"
      networking.gke.io/internal-load-balancer-subnet: "siprec-subnet"
      networking.gke.io/load-balancer-source-ranges: "10.10.0.0/16,172.20.0.0/14"
```

> **Why passthrough?**  
> `externalTrafficPolicy: Local` (set in the template) instructs GKE to skip
> SNAT on the NLB so packets arrive at the recorder with the real SBC source
> IP. The NLB only sends traffic to nodes that have a Ready pod, so there is
> no cross-node hop.

---

## Step 5 — Install the chart

```bash
kubectl create namespace siprec

helm install siprec ./helm \
  --namespace siprec \
  --values helm/my-values.yaml \
  --wait --timeout 5m
```

To upgrade later:

```bash
helm upgrade siprec ./helm \
  --namespace siprec \
  --values helm/my-values.yaml \
  --wait --timeout 5m
```

---

## Step 6 — Provision the RTP forwarding rule

A k8s `Service` can't express a 20 000-port UDP range, so the RTP forwarder
must be created with `gcloud` against the same shared VIP. Example:

```bash
# Unmanaged instance group containing the recorder node(s)
gcloud compute backend-services create streamlink-rtp-bs \
  --region=${REGION} \
  --load-balancing-scheme=INTERNAL \
  --protocol=UDP \
  --health-checks=streamlink-sip-hc

gcloud compute backend-services add-backend streamlink-rtp-bs \
  --region=${REGION} \
  --instance-group=<recorder-ig> \
  --instance-group-zone=<zone>

gcloud compute forwarding-rules create streamlink-rtp-fr \
  --region=${REGION} \
  --load-balancing-scheme=INTERNAL \
  --network=<vpc> --subnet=<subnet> \
  --address=100.73.16.5 \
  --ip-protocol=UDP \
  --ports=10000-30000 \
  --backend-service=streamlink-rtp-bs
```

---

## Step 7 — Verify the deployment

```bash
kubectl get pods -n siprec -l app.kubernetes.io/name=siprec-recorder -o wide
kubectl get svc  -n siprec
# The siprec-recorder Service should show EXTERNAL-IP = 100.73.16.5
```

Tail logs:

```bash
kubectl logs -n siprec -l app.kubernetes.io/name=siprec-recorder --follow --prefix
```

Live SIP capture inside a pod:

```bash
POD=$(kubectl get pod -n siprec -l app.kubernetes.io/name=siprec-recorder -o name | head -1)
kubectl exec -it -n siprec ${POD} -- sngrep -d any port 5060
```

---

## Step 8 — Point your SBC at the LB

Configure your SBC / call controller to send SIPREC `INVITE` requests to:

```
sip:100.73.16.5:5060
```

RTP is automatically targeted at the same address because the recorder
stamps `mediaIP: 100.73.16.5` into its SDP answer.

If the SBC is on-premises:
- Ensure Cloud VPN or Interconnect connects the on-prem segment to the GKE VPC.
- Add GCP firewall rules for UDP `5060` (SIP) and `10000–30000` (RTP) from the SBC source CIDRs.

---

## GCP firewall rules

NLB health-check probes originate from `35.191.0.0/16` and `130.211.0.0/22`:

```bash
GCP_PROJECT=my-gcp-project
VPC_NETWORK=default                  # replace with your VPC name
SBC_CIDR="10.10.0.0/16"             # replace with your SBC address range

gcloud compute firewall-rules create allow-siprec-healthcheck \
  --project=${GCP_PROJECT} \
  --network=${VPC_NETWORK} \
  --allow=udp:5060 \
  --source-ranges="35.191.0.0/16,130.211.0.0/22" \
  --target-tags=recorder

gcloud compute firewall-rules create allow-siprec-sip \
  --project=${GCP_PROJECT} \
  --network=${VPC_NETWORK} \
  --allow=udp:5060 \
  --source-ranges="${SBC_CIDR}" \
  --target-tags=recorder

gcloud compute firewall-rules create allow-siprec-rtp \
  --project=${GCP_PROJECT} \
  --network=${VPC_NETWORK} \
  --allow=udp:10000-30000 \
  --source-ranges="${SBC_CIDR}" \
  --target-tags=recorder
```

> GKE adds the node-pool network tag automatically. Confirm with:
> ```bash
> gcloud compute instances describe <node-name> --format='value(tags.items)'
> ```
> If the tag is different, replace `recorder` in the rules above.

---

## Uninstall

```bash
helm uninstall siprec --namespace siprec
kubectl delete namespace siprec
```

Node labels and taints are **not** removed automatically:

```bash
kubectl label node ${RECORDER_NODE} siprec-role-
kubectl taint node ${RECORDER_NODE} siprec-role=recorder:NoSchedule-
```

---

## Values reference

| Key | Default | Description |
|---|---|---|
| `siprecRecorder.image.repository` | `…/cx-streamlink-rec` | Recorder container image |
| `siprecRecorder.image.tag` | `dev` | Recorder image tag |
| `siprecRecorder.nodeSelector` | `siprec-role: recorder` | Where the DaemonSet runs |
| `siprecRecorder.resources.requests.cpu` | `2000m` | CPU request |
| `siprecRecorder.resources.requests.memory` | `11Gi` | Memory request |
| `siprecRecorder.resources.limits.cpu` | `2000m` | CPU limit |
| `siprecRecorder.resources.limits.memory` | `11Gi` | Memory limit |
| `siprecRecorder.recordingsHostPath` | `/var/lib/siprec/recordings` | Node-local recording staging path |
| `siprecRecorder.apiPort` | `8080` | Host-networked pod-local Agent Assist API port |
| `siprecRecorder.service.type` | `LoadBalancer` | Service type fronting SIP |
| `siprecRecorder.service.port` | `5060` | SIP port |
| `siprecRecorder.service.loadBalancerIP` | `100.73.16.5` | Pre-reserved shared LB VIP |
| `siprecRecorder.service.annotations` | GKE internal NLB | Service annotations |
| `siprecRecorder.config.mediaIP` | `100.73.16.5` | IP advertised in SDP for RTP |
| `siprecRecorder.config.rtpPortStart` | `10000` | RTP port range start |
| `siprecRecorder.config.rtpPortEnd` | `30000` | RTP port range end |
| `siprecRecorder.config.gcsBucket` | `cx-siprec-audio-raw` | GCS bucket for recordings |
| `siprecRecorder.config.gcsMetadataBucket` | `cx-siprec-metadata` | GCS bucket for per-call metadata |
| `siprecRecorder.config.redisAddr` | `redis:6379` | Redis endpoint for `loc:<Call ID>` ownership keys |
| `siprecRecorder.config.httpListenAddr` | `0.0.0.0:8080` | Pod-local Agent Assist API listen address |
| `siprecRecorder.config.agentAssistProjectID` | empty | Google Cloud project for Agent Assist |
| `siprecRecorder.config.agentAssistConversationProfileID` | empty | Agent Assist conversation profile ID |
| `siprecRecorder.config.agentAssistSendQueuePackets` | `250` | Per-leg bounded queue depth for async Agent Assist sends |
