# cx-streamlink Helm chart

Deploys a Streamlink signaling proxy and a configurable pool of host-network
Streamlink recorders on GKE.

## Architecture

```text
Recording source
  |
  | SIP/UDP 5060
  v
cx-streamlink-proxy internal passthrough NLB (one VIP)
  |
  | round-robin to stable Kubernetes Service DNS
  v
cx-streamlink-rec-<instance> Service (one NLB VIP per recorder)
  |                         ^
  | SIP/UDP 5060            | RTP on the configured even UDP ports
  v                         |
One-replica Deployment -----+
  |
  +-- one PVC for recording staging
  +-- GCS upload through Workload Identity or a credentials Secret
```

Each `recorder.instances[]` entry renders one one-replica Deployment, one
recorder-specific ConfigMap, one `ReadWriteOnce` PVC by default, and one
internal passthrough `LoadBalancer` Service with a reserved VIP. Scale the
recorder pool by adding instances, not by increasing an individual
Deployment's replica count.

Recorder pods use the host network and are co-located on one node by default.
Each instance has a unique SIP listen port, while all instances share the
node-wide even RTP port pool from `10000` through `30000`.

The proxy uses dispatcher round-robin selection for new dialogs. Dispatcher
entries are generated from recorder Service FQDNs, and in-dialog requests
return to the same recorder.

## Resource names

Resource names are fixed and do not include the Helm release name:

- recorder Deployment and Service: `cx-streamlink-rec-<instance>`;
- recorder ConfigMap: `cx-streamlink-rec-<instance>-config`;
- recorder PVC: `cx-streamlink-rec-<instance>-recordings`;
- recorder ServiceAccount: `cx-streamlink-rec`;
- proxy Deployment and Service: `cx-streamlink-proxy`; and
- proxy ConfigMap: `cx-streamlink-proxy-config`.

Only one release of this chart can be installed in a namespace because two
releases would attempt to own the same fixed names.

## Prerequisites

- Helm 3.14 or newer.
- A GKE cluster with sufficient NodePort capacity.
- One reserved regional internal IPv4 address for the proxy.
- One reserved regional internal IPv4 address per recorder instance.
- A default StorageClass, or an explicit recorder storage class.
- Firewall reachability from recording sources to UDP/5060 on the proxy VIP
  and UDP/10000-30000 on every recorder VIP and recorder backend node.
- No other host-network workload using the configured recorder SIP or RTP
  ports on the selected node.
- GCS permissions configured with Workload Identity or a Secret if recordings
  are uploaded.

Reserve the VIPs in the same region and subnet as the cluster:

```bash
gcloud compute addresses create cx-streamlink-proxy-vip \
  --region=REGION \
  --subnet=SUBNET \
  --addresses=10.20.0.100

gcloud compute addresses create cx-streamlink-rec-recorder-1-vip \
  --region=REGION \
  --subnet=SUBNET \
  --addresses=100.73.16.5
```

## Configure

Create an override file rather than editing the defaults:

```yaml
proxy:
  replicaCount: 1
  service:
    loadBalancerIP: "10.20.0.100"
    loadBalancerSourceRanges:
      - "10.10.0.0/16"

recorder:
  instances:
    - name: recorder-1
      loadBalancerIP: "100.73.16.5"
      sipPort: 5060
    - name: recorder-2
      loadBalancerIP: "100.73.16.64"
      sipPort: 5061
      persistence:
        size: 50Gi

  hostNetwork: true
  coLocateOnSameNode: true

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
    rtpPortEnd: 30000
    gcsBucket: "my-recordings-bucket"
    gcsMetadataBucket: "my-metadata-bucket"
```

On GKE 1.33.1 or later with subsetting enabled, the internal load balancer
class can be selected explicitly:

```yaml
proxy:
  service:
    loadBalancerClass: networking.gke.io/l4-regional-internal

recorder:
  service:
    loadBalancerClass: networking.gke.io/l4-regional-internal
```

Leave `loadBalancerClass` empty on older clusters; the
`networking.gke.io/load-balancer-type: Internal` annotation is enabled by
default.

## Host-network RTP routing

GKE supports at most 100 declared ports on a LoadBalancer Service, so the chart
does not declare all 10,001 even ports in `10000-30000`. Instead, each recorder
Service declares SIP plus five RTP ports. More than five total Service ports
makes GKE configure the internal passthrough Network Load Balancer forwarding
rule in all-ports mode.

GKE's generated firewall rule permits only the ports explicitly listed on the
Service. A separate VPC ingress firewall rule must allow UDP/10000-30000 from
the recording-source ranges to the recorder backend nodes.

Because recorder pods share the host network:

- every instance must use a unique `sipPort`;
- RTP ports are allocated from one node-wide shared pool;
- two active recorder sockets cannot use the same RTP port simultaneously; and
- `coLocateOnSameNode: true` keeps all recorder instances on one node. Set it to
  `false` only when each node is prepared to receive the all-ports VIP traffic.

The recorder Services continue to expose SIP on port 5060. Kubernetes maps
that stable Service port to each instance's host SIP port, so proxy dispatcher
destinations do not need instance-specific SIP ports.

## GCP authentication

For Workload Identity, set the GCP service-account email:

```yaml
recorder:
  gcp:
    workloadIdentity:
      gcpServiceAccount: cx-streamlink-rec@PROJECT.iam.gserviceaccount.com
    credentialsSecret:
      name: ""
```

Alternatively, create a Kubernetes Secret and configure its key:

```bash
kubectl create secret generic cx-streamlink-rec-gcp-credentials \
  --namespace=voice \
  --from-file=key.json=./service-account.json
```

```yaml
recorder:
  gcp:
    workloadIdentity:
      gcpServiceAccount: ""
    credentialsSecret:
      name: cx-streamlink-rec-gcp-credentials
      key: key.json
```

## Install or upgrade

```bash
helm upgrade --install streamlink ./helm \
  --namespace=voice \
  --create-namespace \
  --values=helm/my-values.yaml \
  --wait \
  --timeout=10m
```

Point recording sources at the `cx-streamlink-proxy` VIP on UDP/5060. The
proxy forwards signaling to the recorder Services, and each recorder advertises
its own VIP and allocated RTP ports in the SDP answer.

## Breaking upgrade from chart versions before 0.5.0

Version 0.5.0 changes public values keys, resource names, labels, selectors,
annotations, the recorder ServiceAccount, and default GCS bucket names.
Treat the upgrade as a workload replacement, not an in-place rollout.

Before upgrading:

1. Back up, snapshot, upload, or copy recordings from every existing PVC.
   Newly named PVCs do not adopt existing data automatically.
2. Set `recorder.config.gcsBucket` and `gcsMetadataBucket` explicitly if the
   existing bucket names must be retained.
3. Bind the GCP IAM service account to the new `cx-streamlink-rec` Kubernetes
   ServiceAccount.
4. Plan a maintenance window for replacing Deployments and immutable selector
   labels.
5. Ensure old LoadBalancer Services release their reserved VIPs before the new
   Services claim them. Avoid running both resource sets with the same VIPs.
6. Translate existing override files to the `recorder` and `proxy` values
   roots before rendering or upgrading.

Use `helm diff upgrade` where available and inspect the planned PVC and Service
replacements before applying the upgrade.

## Scaling

To add recording capacity, append another item to `recorder.instances`, reserve
its VIP, assign a unique `sipPort`, and upgrade the release. All co-located
recorders share the same RTP range and node capacity. Scale proxy replicas with
`proxy.replicaCount`.

## Verify

Render and lint before deployment:

```bash
helm lint ./helm
helm template streamlink ./helm --namespace=voice --values=helm/my-values.yaml
```

Inspect the running resources:

```bash
kubectl get deploy,pod,pvc,svc -n voice
kubectl get svc -n voice -l app.kubernetes.io/name=cx-streamlink-rec
kubectl logs -n voice -l app.kubernetes.io/name=cx-streamlink-proxy --follow
```

Each recorder Service should show six UDP ports. This is expected: GKE uses
those six declarations to create an all-ports forwarding rule. Verify that rule
in GCP and separately verify the UDP/10000-30000 VPC firewall rule.

The proxy ConfigMap should contain one dispatcher line per recorder:

```bash
kubectl get configmap cx-streamlink-proxy-config -n voice \
  -o jsonpath='{.data.dispatcher\.list}'
```

The repository's load-test scenarios can be used to confirm alternating
recorder selection, in-dialog routing, and RTP arrival through each recorder
VIP.

## Uninstall

```bash
helm uninstall streamlink --namespace=voice
```

PVCs created directly by this chart are removed with the Helm release. Ensure
recordings are uploaded or retain/copy the volumes before uninstalling.
