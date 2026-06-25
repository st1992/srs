# siprec-stack Helm chart

End-to-end installation guide for the SIPREC recording stack on GKE.

## Architecture overview

```
SBC / call controller (internal VPC)
          │  SIP INVITE (SIPREC)  UDP/TCP 5060
          ▼
┌─────────────────────────────────────────────────────┐
│  GCP Internal Passthrough NLB  (L4, private VIP)    │
│  externalTrafficPolicy: Local → no SNAT              │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────┐
│  Kamailio node  (c2-standard-4, hostNetwork)          │
│  Deployment — replicas: 1, strategy: Recreate         │
│                                                       │
│  • Record-Route on every INVITE                       │
│  • dispatcher module  →  round-robin across recorders │
│  • OPTIONS health-checks  →  automatic failover       │
└──────────┬────────────────────────────┬──────────────┘
           │  SIP + SDP (re-routed)     │
           ▼                            ▼
┌──────────────────┐        ┌──────────────────┐
│  Recorder node 1  │        │  Recorder node N  │
│  c2-standard-4    │  ...   │  c2-standard-4    │
│  DaemonSet pod    │        │  DaemonSet pod    │
│  hostNetwork      │        │  hostNetwork      │
│  RTP 10000-30000  │        │  RTP 10000-30000  │
└──────────────────┘        └──────────────────┘
           │                            │
           └────────────┬───────────────┘
                        ▼
               GCS bucket  (recordings)
               Pub/Sub     (session events)
```

**Traffic path for a new SIPREC session:**

1. SBC sends `INVITE` with SIPREC multipart body to the NLB private VIP.
2. NLB forwards the packet directly to the Kamailio node (passthrough — source IP preserved).
3. Kamailio inserts `Record-Route`, selects a recorder via round-robin dispatcher, relays the `INVITE`.
4. Recorder responds `200 OK`; Kamailio relays back to the SBC.
5. All subsequent in-dialog requests (`re-INVITE`, `BYE`) traverse Kamailio via the Route set.
6. RTP media flows **directly** between the SBC and the recorder node IP (no Kamailio in the media path).

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

```bash
PROJECT=my-gcp-project
REGION=us-central1
REPO=siprec
IMAGE=${REGION}-docker.pkg.dev/${PROJECT}/${REPO}/siprec-recorder

# Authenticate Docker to Artifact Registry
gcloud auth configure-docker ${REGION}-docker.pkg.dev

# Build (from the repo root — where Dockerfile lives)
docker build \
  --build-arg BUILD_EXPIRES=$(date -d '+3 months' +%Y-%m-%d) \
  -t ${IMAGE}:latest \
  -t ${IMAGE}:$(git rev-parse --short HEAD) \
  .

docker push ${IMAGE}:latest
docker push ${IMAGE}:$(git rev-parse --short HEAD)
```

---

## Step 2 — Prepare cluster nodes

The chart uses **node selectors + taints** to isolate workloads.  
All nodes in this setup are `c2-standard-4` (4 vCPU, 16 GiB RAM).

### 2a — Kamailio node (one node)

```bash
KAMAILIO_NODE=gke-cluster-kamailio-pool-xxxx   # replace with actual node name

kubectl label node ${KAMAILIO_NODE} siprec-role=kamailio
kubectl taint node ${KAMAILIO_NODE} siprec-role=kamailio:NoSchedule
```

### 2b — Recorder nodes (one or more)

Repeat for **each** recorder node. The DaemonSet will place exactly one pod per labelled node.

```bash
for NODE in gke-cluster-recorder-pool-xxxx gke-cluster-recorder-pool-yyyy; do
  kubectl label node ${NODE} siprec-role=recorder
  kubectl taint node ${NODE} siprec-role=recorder:NoSchedule
done
```

Verify labels and taints:

```bash
kubectl get nodes -L siprec-role
kubectl describe node ${KAMAILIO_NODE} | grep -A5 Taints
```

---

## Step 3 — Configure `values.yaml`

Copy the shipped defaults and override as needed:

```bash
cp helm/values.yaml helm/my-values.yaml
```

Minimum required overrides in `my-values.yaml`:

```yaml
siprecRecorder:
  image:
    repository: us-central1-docker.pkg.dev/my-gcp-project/siprec/siprec-recorder
    tag: "latest"             # or a specific git-sha tag

  config:
    # GCS upload — where recordings land after each call
    gcsBucket: "my-recordings-bucket"
    gcsObjectPrefix: "siprec"

    # Pub/Sub event stream (optional)
    pubsubProjectId: "my-gcp-project"
    pubsubTopicId:   "siprec-events"
```

### Internal NLB configuration (already enabled in defaults)

The chart ships with the GKE internal passthrough NLB annotation set:

```yaml
kamailio:
  service:
    type: LoadBalancer
    port: 5060
    annotations:
      networking.gke.io/load-balancer-type: "Internal"
```

Optional overrides:

```yaml
kamailio:
  service:
    annotations:
      networking.gke.io/load-balancer-type: "Internal"

      # Restrict which subnet the NLB frontend IP is allocated from.
      # Useful when the cluster has multiple subnets.
      networking.gke.io/internal-load-balancer-subnet: "siprec-subnet"

      # Allow-list the SBC CIDRs so only authorised sources can reach port 5060.
      # Accepts a comma-separated list of CIDR blocks.
      networking.gke.io/load-balancer-source-ranges: "10.10.0.0/16,172.20.0.0/14"
```

> **Why passthrough?**  
> `externalTrafficPolicy: Local` (set in the template) instructs GKE to skip SNAT on the NLB. Packets arrive at the Kamailio node with the **real SBC source IP** in the IP header — essential for SIP Via / Contact header validation and firewall rules. The NLB only sends traffic to nodes that have a ready endpoint, so there is no cross-node hop.

---

## Step 4 — Create the namespace

```bash
kubectl create namespace siprec
```

---

## Step 5 — Install the chart

```bash
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

## Step 6 — Verify the deployment

### Pods

```bash
# Kamailio — should show 1/1 Running
kubectl get pods -n siprec -l app.kubernetes.io/name=kamailio

# SIPREC recorders — should show one Running pod per recorder node
kubectl get pods -n siprec -l app.kubernetes.io/name=siprec-recorder -o wide
```

### Internal NLB IP

```bash
kubectl get svc -n siprec siprec-kamailio
# EXTERNAL-IP column shows the private VIP assigned by GCP (takes ~60 s on first install)
```

The `EXTERNAL-IP` is a private RFC-1918 address reachable from within the VPC
(and from on-prem via Cloud VPN / Interconnect).

### Kamailio dispatcher list

The init-container writes the recorder pod IPs to `dispatcher.list`.  Verify it was populated:

```bash
KAMAILIO_POD=$(kubectl get pod -n siprec -l app.kubernetes.io/name=kamailio -o name | head -1)

kubectl exec -n siprec ${KAMAILIO_POD} -- cat /etc/kamailio/dispatcher.list
# Expected format (one line per recorder pod):
#   1 sip:10.0.1.5:5060 0 0
#   1 sip:10.0.1.6:5060 0 0
```

### Kamailio logs

```bash
kubectl logs -n siprec -l app.kubernetes.io/name=kamailio --follow
```

Look for lines like:
```
SIPREC session <call-id> dispatched to recorder sip:10.0.1.5:5060
```

### Recorder logs

```bash
# Tail all recorder pods simultaneously
kubectl logs -n siprec -l app.kubernetes.io/name=siprec-recorder --follow --prefix
```

---

## Step 7 — Point your SBC at the NLB

Configure your SBC / call controller to send SIPREC `INVITE` requests to:

```
sip:<NLB-private-VIP>:5060
```

where `<NLB-private-VIP>` is the `EXTERNAL-IP` from Step 6.

For RTP, the SBC must be able to reach each **recorder node IP** directly on UDP ports `10000–30000`. The recorder advertises its own node IP in SDP (auto-detected via the primary interface when `hostNetwork: true`).

If the SBC is on-premises:
- Ensure Cloud VPN or Interconnect is configured between the on-prem segment and the GKE VPC.
- Add firewall rules in GCP for UDP `5060` (SIP) and `10000–30000` (RTP) from the SBC source CIDRs.

---

## GCP firewall rules

The NLB health-check probes originate from `35.191.0.0/16` and `130.211.0.0/22`. These rules allow health checks and SIP/RTP traffic:

```bash
GCP_PROJECT=my-gcp-project
VPC_NETWORK=default                   # replace with your VPC name
SBC_CIDR="10.10.0.0/16"              # replace with your SBC address range

# Allow GKE health-check probes to reach Kamailio
gcloud compute firewall-rules create allow-kamailio-healthcheck \
  --project=${GCP_PROJECT} \
  --network=${VPC_NETWORK} \
  --allow=tcp:5060,udp:5060 \
  --source-ranges="35.191.0.0/16,130.211.0.0/22" \
  --target-tags=kamailio

# Allow SBCs → Kamailio (SIP)
gcloud compute firewall-rules create allow-siprec-sip \
  --project=${GCP_PROJECT} \
  --network=${VPC_NETWORK} \
  --allow=udp:5060,tcp:5060 \
  --source-ranges="${SBC_CIDR}" \
  --target-tags=kamailio

# Allow SBCs → recorders (RTP media)
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
> If the tag is different, replace `kamailio` / `recorder` in the rules above.

---

## Adding or removing recorder nodes

Because the Kamailio dispatcher list is generated at **pod startup** by the init-container (DNS lookup of the headless Service), you must restart Kamailio whenever the set of recorder pods changes.

```bash
# After adding new recorder nodes and confirming their pods are Running:
kubectl rollout restart deployment/siprec-kamailio -n siprec

# Watch the restart:
kubectl rollout status deployment/siprec-kamailio -n siprec
```

Kamailio will then pick up the new pod IPs at startup.  
During the brief restart window (~5–10 s) the NLB health-check marks the Kamailio endpoint unhealthy and the NLB stops forwarding — no calls are dropped mid-flight.

---

## Upgrading Kamailio

The Deployment uses `strategy: Recreate`.  A `helm upgrade` or `kubectl rollout restart` will:

1. Terminate the existing pod (allowing in-flight SIP dialogs to drain for up to 60 s via `terminationGracePeriodSeconds`).
2. Start the new pod (init-container re-resolves recorder IPs).
3. NLB health-check marks the new pod healthy; traffic resumes.

---

## Uninstall

```bash
helm uninstall siprec --namespace siprec
kubectl delete namespace siprec
```

Node labels and taints are **not** removed automatically:

```bash
kubectl label node ${KAMAILIO_NODE} siprec-role-
kubectl taint node ${KAMAILIO_NODE} siprec-role=kamailio:NoSchedule-

for NODE in <recorder-nodes>; do
  kubectl label node ${NODE} siprec-role-
  kubectl taint node ${NODE} siprec-role=recorder:NoSchedule-
done
```

---

## Values reference

| Key | Default | Description |
|---|---|---|
| `kamailio.image.repository` | `ghcr.io/kamailio/kamailio` | Kamailio container image |
| `kamailio.image.tag` | `6.0.7-bookworm` | Image tag |
| `kamailio.children` | `8` | Kamailio worker processes (2× vCPU) |
| `kamailio.modulePath` | `/usr/lib/x86_64-linux-gnu/kamailio/modules/` | Module path (change for arm64) |
| `kamailio.resources.requests.cpu` | `3500m` | CPU request |
| `kamailio.resources.requests.memory` | `12Gi` | Memory request |
| `kamailio.resources.limits.cpu` | `4` | CPU limit |
| `kamailio.resources.limits.memory` | `14Gi` | Memory limit |
| `kamailio.service.type` | `LoadBalancer` | Service type |
| `kamailio.service.port` | `5060` | SIP port |
| `kamailio.service.annotations` | GKE internal NLB | Service annotations |
| `siprecRecorder.image.repository` | `your-registry/siprec-recorder` | **Must set** |
| `siprecRecorder.image.tag` | `latest` | Recorder image tag |
| `siprecRecorder.resources.requests.cpu` | `3800m` | CPU request (c2-standard-4) |
| `siprecRecorder.resources.requests.memory` | `13Gi` | Memory request |
| `siprecRecorder.resources.limits.cpu` | `4` | CPU limit |
| `siprecRecorder.resources.limits.memory` | `15Gi` | Memory limit |
| `siprecRecorder.recordingsHostPath` | `/var/lib/siprec/recordings` | Node-local recording staging path |
| `siprecRecorder.config.rtpPortStart` | `10000` | RTP port range start |
| `siprecRecorder.config.rtpPortEnd` | `30000` | RTP port range end |
| `siprecRecorder.config.gcsBucket` | `""` | GCS bucket (empty = local disk only) |
| `siprecRecorder.config.pubsubProjectId` | `""` | GCP project for Pub/Sub |
| `siprecRecorder.config.pubsubTopicId` | `""` | Pub/Sub topic |
