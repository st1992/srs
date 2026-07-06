# siprec-stack Helm chart

End-to-end installation guide for the streamlink SIPREC recording stack on GKE.

## Architecture overview

```
SBC / call controller (internal VPC)
          │  SIP INVITE (SIPREC)  UDP 5060
          │  RTP media            UDP 10000-30000
          ▼
┌─────────────────────────────────────────────────────┐
│  Per-node GCP Internal Passthrough NLBs             │
│  Frontend IPs: one pre-reserved VIP per node        │
│  externalTrafficPolicy: Local → no SNAT             │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────┐
│  Recorder node  (c2-standard-4, hostNetwork)          │
│  Node-pinned DaemonSet pod                            │
│                                                       │
│  • SIP   UDP 5060               (signalling)          │
│  • RTP   UDP 10000-30000        (media)               │
│  • sngrep available in-pod for live SIP capture       │
└──────────┬───────────────────────────────────────────┘
           │
           ▼
   GCS bucket  (recordings + metadata)
```

Each recorder pod sits **directly behind its own Internal NLB VIP** — there is
no Kamailio proxy in front of it any more. SBCs send both SIP signalling and
RTP media to the VIP assigned to the target recorder node; that VIP forwards
(passthrough, source-IP preserved) straight to the node-pinned recorder pod via
hostNetwork.

Because a Kubernetes `Service` cannot express a 20 000-port UDP range, the
RTP forwarding rule for each per-node VIP must be provisioned out-of-band
against the same VIP used by that node's SIP `Service` (see
[RTP forwarding rule](#rtp-forwarding-rule) below).

**Traffic path for a new SIPREC session:**

1. SBC sends `INVITE` with the SIPREC multipart body to the assigned node VIP
   on UDP/5060.
2. That node's NLB forwards the packet (passthrough) to the matching Ready
   recorder pod.
3. Recorder answers `200 OK` with the same node VIP in SDP so the SBC sends
   RTP back to that VIP.
4. RTP packets traverse that node VIP's RTP forwarding rule and land on the
   same recorder node by destination port.

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

## Step 2 — Prepare the recorder nodes

The chart uses **node selectors + taints** to isolate the workload. This
setup targets a static list of recorder nodes (`c2-standard-4`, 4 vCPU /
16 GiB).

```bash
RECORDER_NODE=gke-cluster-recorder-pool-xxxx   # repeat per recorder node

kubectl label node ${RECORDER_NODE} siprec-role=recorder
kubectl taint node ${RECORDER_NODE} siprec-role=recorder:NoSchedule
```

Verify:

```bash
kubectl get nodes -L siprec-role
kubectl describe node ${RECORDER_NODE} | grep -A5 Taints
```

Each node must also be listed in `siprecRecorder.nodes` with its
`kubernetes.io/hostname` value and pre-reserved VIP. To scale out later, add
another node to that values list, reserve another VIP, and label/taint the
node the same way.

---

## Step 3 — Reserve the per-node LB VIPs

Each `siprecRecorder.nodes[].loadBalancerIP` is used by that node's SIP
`Service` (managed by this chart) **and** its out-of-band RTP forwarding rule.
Reserve every VIP with the `SHARED_LOADBALANCER_VIP` purpose so both rules can
coexist on the same IP:

```bash
gcloud compute addresses create streamlink-recorder-1-vip \
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

  nodes:
    - name: recorder-1
      hostname: gke-cluster-recorder-pool-xxxx
      loadBalancerIP: "100.73.16.5"
    - name: recorder-2
      hostname: gke-cluster-recorder-pool-yyyy
      loadBalancerIP: "100.73.16.6"

  config:
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

A k8s `Service` can't express a 20 000-port UDP range, so create one RTP
forwarder per configured node VIP. Example for `recorder-1`:

```bash
# Unmanaged instance group containing only the recorder-1 VM.
gcloud compute backend-services create streamlink-recorder-1-rtp-bs \
  --region=${REGION} \
  --load-balancing-scheme=INTERNAL \
  --protocol=UDP \
  --health-checks=streamlink-sip-hc

gcloud compute backend-services add-backend streamlink-recorder-1-rtp-bs \
  --region=${REGION} \
  --instance-group=<recorder-1-ig> \
  --instance-group-zone=<zone>

gcloud compute forwarding-rules create streamlink-recorder-1-rtp-fr \
  --region=${REGION} \
  --load-balancing-scheme=INTERNAL \
  --network=<vpc> --subnet=<subnet> \
  --address=100.73.16.5 \
  --ip-protocol=UDP \
  --ports=10000-30000 \
  --backend-service=streamlink-recorder-1-rtp-bs
```

Repeat this pattern for every `siprecRecorder.nodes[]` entry, using that
entry's VIP and a backend that contains only the matching recorder node.

---

## Step 7 — Verify the deployment

```bash
kubectl get pods -n siprec -l app.kubernetes.io/name=siprec-recorder -o wide
kubectl get svc  -n siprec
# Each siprec-recorder-* Service should show its configured per-node VIP.
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

Configure your SBC / call controller to send each SIPREC `INVITE` request to
the VIP for the recorder node that should handle that call:

```
sip:<node-specific-vip>:5060
```

RTP is automatically targeted at the same address because the node-specific
recorder config stamps that VIP into its SDP answer.

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
| `siprecRecorder.nodes[].name` | `recorder-1` | Stable suffix for per-node resources |
| `siprecRecorder.nodes[].hostname` | `replace-with-recorder-node-hostname` | Node `kubernetes.io/hostname` target |
| `siprecRecorder.nodes[].loadBalancerIP` | `100.73.16.5` | Pre-reserved per-node Internal LB VIP |
| `siprecRecorder.resources.requests.cpu` | `2000m` | CPU request |
| `siprecRecorder.resources.requests.memory` | `11Gi` | Memory request |
| `siprecRecorder.resources.limits.cpu` | `2000m` | CPU limit |
| `siprecRecorder.resources.limits.memory` | `11Gi` | Memory limit |
| `siprecRecorder.recordingsHostPath` | `/var/lib/siprec/recordings` | Node-local recording staging path |
| `siprecRecorder.service.type` | `LoadBalancer` | Service type fronting SIP |
| `siprecRecorder.service.port` | `5060` | SIP port |
| `siprecRecorder.service.annotations` | GKE internal NLB | Service annotations |
| `siprecRecorder.config.rtpPortStart` | `10000` | RTP port range start |
| `siprecRecorder.config.rtpPortEnd` | `30000` | RTP port range end |
| `siprecRecorder.config.gcsBucket` | `cx-siprec-audio-raw` | GCS bucket for recordings |
| `siprecRecorder.config.gcsMetadataBucket` | `cx-siprec-metadata` | GCS bucket for per-call metadata |

kubectl get nodes -L kubernetes.io/hostname

kubectl label node <node-1> siprec-role=recorder --overwrite
kubectl taint node <node-1> siprec-role=recorder:NoSchedule --overwrite

kubectl label node <node-2> siprec-role=recorder --overwrite
kubectl taint node <node-2> siprec-role=recorder:NoSchedule --overwrite