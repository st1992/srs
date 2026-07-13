# siprec-stack Helm chart

Deploys a Kamailio SIPREC signaling proxy and a configurable pool of
streamlink SIPREC recorders on GKE.

## Architecture

```text
SIPREC source
  |
  | SIP/UDP 5060
  v
Kamailio internal passthrough NLB (one VIP)
  |
  | round-robin to stable Kubernetes Service DNS
  v
Recorder instance Service (one internal passthrough NLB VIP per recorder)
  |                         ^
  | SIP/UDP 5060            | RTP on configured even UDP ports
  v                         |
One-replica Deployment -----+
  |
  +-- one PVC for recording staging
  +-- GCS upload through Workload Identity or a credentials Secret
```

Each `siprecRecorder.instances[]` entry renders:

- one one-replica `Deployment`;
- one recorder-specific `ConfigMap`;
- one `ReadWriteOnce` PVC by default; and
- one internal passthrough `LoadBalancer` Service with a reserved VIP.

Recorder Deployments are not hostname-pinned and do not use host networking,
so multiple recorder pods can run on the same node when resources permit.
Each Service has a unique selector and routes only to its recorder Deployment.
Scale the recorder pool by adding instances, not by increasing an individual
Deployment's replica count.

Kamailio uses dispatcher algorithm `4` (round-robin in Kamailio 6.0) for new
dialogs. Dispatcher entries are generated from recorder Service FQDNs. The
selected recorder URI is stored in Kamailio's Record-Route parameter so ACK,
BYE, and other in-dialog requests return to the same recorder.

## RTP port limit

GKE supports at most 100 ports on an internal LoadBalancer Service. One port is
used by SIP/5060, leaving at most 99 RTP ports. The recorder allocates even RTP
ports, so the chart exposes every even port in the inclusive
`rtpPortStart`-`rtpPortEnd` range.

The defaults expose 99 RTP ports (`10000, 10002, ..., 10196`). A SIPREC session
uses two ports, one per media leg, providing capacity for approximately 49
simultaneous sessions per recorder.

When more than five ports are declared, GKE configures the NLB forwarding rule
for all ports. Kubernetes still routes only the 100 Service ports declared by
this chart, and GKE firewall automation only opens those declared ports. Each
recorder Service also consumes 100 NodePorts, so account for the cluster's
NodePort range when adding many recorder instances.

## Prerequisites

- Helm 3.14 or newer.
- A GKE cluster with enough NodePort capacity.
- One reserved regional internal IPv4 address for Kamailio.
- One reserved regional internal IPv4 address per recorder instance.
- A default StorageClass, or an explicit recorder storage class.
- Firewall reachability from SIPREC sources to UDP/5060 on the Kamailio VIP
  and to the configured RTP UDP ports on every recorder VIP.
- GCS permissions configured with Workload Identity or a Secret if recordings
  are uploaded.

Reserve the VIPs in the same region and subnet as the cluster:

```bash
gcloud compute addresses create siprec-kamailio-vip \
  --region=REGION \
  --subnet=SUBNET \
  --addresses=10.20.0.100

gcloud compute addresses create siprec-recorder-1-vip \
  --region=REGION \
  --subnet=SUBNET \
  --addresses=100.73.16.5
```

## Configure

Create an override file rather than editing the defaults:

```yaml
kamailio:
  replicaCount: 1
  service:
    loadBalancerIP: "10.20.0.100"
    loadBalancerSourceRanges:
      - "10.10.0.0/16"

siprecRecorder:
  instances:
    - name: recorder-1
      loadBalancerIP: "100.73.16.5"
    - name: recorder-2
      loadBalancerIP: "100.73.16.64"
      persistence:
        size: 50Gi

  # Schedule recorder pods only on c2-standard-4 nodes.
  nodeSelector:
    node.kubernetes.io/instance-type: c2-standard-4

  persistence:
    enabled: true
    accessModes:
      - ReadWriteOnce
    size: 20Gi
    storageClassName: ""

  service:
    loadBalancerSourceRanges:
      - "10.10.0.0/16"

  config:
    rtpPortStart: 10000
    rtpPortEnd: 10196
    gcsBucket: "my-recordings-bucket"
    gcsMetadataBucket: "my-metadata-bucket"
```

Both RTP range endpoints must be even. The inclusive range can contain no more
than 99 even ports. Helm rendering fails early if these constraints are
violated.

On GKE 1.33.1 or later with subsetting enabled, the internal load balancer
class can be selected explicitly:

```yaml
kamailio:
  service:
    loadBalancerClass: networking.gke.io/l4-regional-internal

siprecRecorder:
  service:
    loadBalancerClass: networking.gke.io/l4-regional-internal
```

Leave `loadBalancerClass` empty on older clusters; the
`networking.gke.io/load-balancer-type: Internal` annotation is enabled by
default.

## GCP authentication

For Workload Identity, set the GCP service-account email:

```yaml
siprecRecorder:
  gcp:
    workloadIdentity:
      gcpServiceAccount: siprec-recorder@PROJECT.iam.gserviceaccount.com
    credentialsSecret:
      name: ""
```

Alternatively, create a Kubernetes Secret and configure its key:

```bash
kubectl create secret generic siprec-gcp-credentials \
  --namespace=voice \
  --from-file=key.json=./service-account.json
```

```yaml
siprecRecorder:
  gcp:
    workloadIdentity:
      gcpServiceAccount: ""
    credentialsSecret:
      name: siprec-gcp-credentials
      key: key.json
```

## Install or upgrade

```bash
helm upgrade --install siprec ./helm \
  --namespace=voice \
  --create-namespace \
  --values=helm/my-values.yaml \
  --wait \
  --timeout=10m
```

Point SIPREC sources at the Kamailio VIP on UDP/5060. Do not target recorder
VIPs for signaling in the normal path. Kamailio forwards signaling to the
recorder Services, and each recorder advertises its own VIP and allocated RTP
ports in the SDP answer.

## Scaling

To add recording capacity, append another item to
`siprecRecorder.instances`, reserve its VIP, and upgrade the release. The chart
creates a new Deployment, Service, ConfigMap, and PVC without changing existing
recorder identities.

Kamailio starts with one replica and can be scaled through
`kamailio.replicaCount`. Dispatcher round-robin counters are local to each
Kamailio process, so multiple replicas provide balanced per-proxy distribution,
not one globally shared strict sequence.

The default recorder resource request is 2 CPU and 11 GiB. Multiple recorders
can share a node only when its allocatable resources satisfy all pod requests;
tune `siprecRecorder.resources` for the actual workload and node size.

## Verify

Render and lint before deployment:

```bash
helm lint ./helm
helm template siprec ./helm --namespace=voice --values=helm/my-values.yaml
```

Inspect the running resources:

```bash
kubectl get deploy,pod,pvc,svc -n voice
kubectl get svc -n voice -l app.kubernetes.io/name=siprec-recorder
kubectl logs -n voice -l app.kubernetes.io/name=kamailio --follow
```

The Kamailio ConfigMap should contain one dispatcher line per recorder:

```bash
kubectl get configmap siprec-kamailio-config -n voice \
  -o jsonpath='{.data.dispatcher\.list}'
```

The repository's `sipp-load-test` scenarios can be used to confirm alternating
recorder selection, in-dialog routing, and RTP arrival through each recorder
VIP.

## Uninstall

```bash
helm uninstall siprec --namespace=voice
```

PVCs created directly by this chart are removed with the Helm release. Ensure
recordings are uploaded or retain/copy the volumes before uninstalling.
