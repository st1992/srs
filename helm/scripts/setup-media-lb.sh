#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# setup-media-lb.sh — provision the RTP media path for streamlink
#
# Why this exists
# ---------------
# The Kamailio Service (type=LoadBalancer, internal) only exposes UDP/5060 on
# the shared VIP 100.73.16.5. The SIPREC recorder advertises that same VIP in
# its SDP "c=" line and expects the SBC's RTP to come back to it on the
# configured rtpPortStart–rtpPortEnd range (default 10000–30000).
#
# A Kubernetes Service can't express 20 000 ports, and GKE doesn't surface
# GCP's port-range forwarding rules through the Service API. So we create
# that forwarding rule directly in GCP and share the VIP with the
# Service-managed Kamailio LB via --purpose=SHARED_LOADBALANCER_VIP.
#
# Topology after this script runs:
#
#   SBC ──UDP/5060─────────▶ 100.73.16.5 ──▶ kamailio-node:5060   (k8s Service)
#   SBC ──UDP/10000-30000──▶ 100.73.16.5 ──▶ recorder-node:<port> (this script)
#
# Health check
# ------------
# The backend service probes HTTP GET /healthz on the recorder's hostNetwork
# health port (default 8080). That endpoint is served by the recorder Go
# binary itself and returns 200 once the SIP UDP socket is bound — the
# direct analogue of the auto-generated healthCheckNodePort that
# externalTrafficPolicy: Local creates for the Kamailio Service.
#
# Prerequisites
# -------------
#   • gcloud authenticated, project set.
#   • Recorder DaemonSet nodes labelled siprec-role=recorder and in a node
#     pool you can name via --recorder-node-pool.
#   • 100.73.16.5 is free in the chosen subnet, OR already reserved with
#     --purpose=SHARED_LOADBALANCER_VIP.
#   • A VPC firewall rule allows GCP health-check ranges
#     (35.191.0.0/16, 130.211.0.0/22) to reach the recorder nodes on the
#     health port. Example:
#       gcloud compute firewall-rules create allow-recorder-hc \
#         --network=<NETWORK> \
#         --direction=INGRESS --action=ALLOW \
#         --rules=tcp:8080 \
#         --source-ranges=35.191.0.0/16,130.211.0.0/22 \
#         --target-tags=<recorder-node-tag>
#
# Idempotent: safe to re-run.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

# ── Defaults (override via flags or env) ────────────────────────────────────
PROJECT="${PROJECT:-}"
REGION="${REGION:-us-central1}"
NETWORK="${NETWORK:-default}"
SUBNET="${SUBNET:-default}"
CLUSTER="${CLUSTER:-}"
RECORDER_NODE_POOL="${RECORDER_NODE_POOL:-}"
VIP="${VIP:-100.73.16.5}"
PORT_RANGE="${PORT_RANGE:-10000-30000}"
HEALTH_PORT="${HEALTH_PORT:-8080}"
HEALTH_PATH="${HEALTH_PATH:-/healthz}"

NAME_PREFIX="${NAME_PREFIX:-streamlink-media}"
HC_NAME="${NAME_PREFIX}-hc"
BS_NAME="${NAME_PREFIX}-bs"
FR_NAME="${NAME_PREFIX}-fr"
ADDR_NAME="${NAME_PREFIX}-vip"

usage() {
  cat <<EOF
Usage: $0 --project PROJECT --cluster CLUSTER --recorder-node-pool POOL [options]

Required:
  --project              GCP project ID
  --cluster              GKE cluster name (used to discover recorder node instance groups)
  --recorder-node-pool   Name of the GKE node pool hosting recorder DaemonSet pods

Optional:
  --region        GCP region                 (default: ${REGION})
  --network       VPC network                (default: ${NETWORK})
  --subnet        VPC subnet (in --region)   (default: ${SUBNET})
  --vip           Shared internal LB VIP     (default: ${VIP})
  --port-range    UDP port range to forward  (default: ${PORT_RANGE})
  --health-port   Recorder /healthz TCP port (default: ${HEALTH_PORT})
  --health-path   Recorder health URL path   (default: ${HEALTH_PATH})
  --name-prefix   GCP resource name prefix   (default: ${NAME_PREFIX})

Environment variables of the same UPPER_CASE names are also honoured.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)             PROJECT="$2"; shift 2;;
    --region)              REGION="$2"; shift 2;;
    --network)             NETWORK="$2"; shift 2;;
    --subnet)              SUBNET="$2"; shift 2;;
    --cluster)             CLUSTER="$2"; shift 2;;
    --recorder-node-pool)  RECORDER_NODE_POOL="$2"; shift 2;;
    --vip)                 VIP="$2"; shift 2;;
    --port-range)          PORT_RANGE="$2"; shift 2;;
    --health-port)         HEALTH_PORT="$2"; shift 2;;
    --health-path)         HEALTH_PATH="$2"; shift 2;;
    --name-prefix)         NAME_PREFIX="$2"; HC_NAME="${NAME_PREFIX}-hc"; BS_NAME="${NAME_PREFIX}-bs"; FR_NAME="${NAME_PREFIX}-fr"; ADDR_NAME="${NAME_PREFIX}-vip"; shift 2;;
    -h|--help)             usage; exit 0;;
    *) echo "Unknown flag: $1" >&2; usage; exit 2;;
  esac
done

: "${PROJECT:?--project is required}"
: "${CLUSTER:?--cluster is required}"
: "${RECORDER_NODE_POOL:?--recorder-node-pool is required}"

gcloud config set project "$PROJECT" >/dev/null

echo "▶ Project=$PROJECT  Region=$REGION  Network=$NETWORK  Subnet=$SUBNET"
echo "▶ VIP=$VIP  Ports=UDP/$PORT_RANGE  HealthCheck=HTTP ${HEALTH_PATH} on TCP/${HEALTH_PORT}"
echo

# ── 1. Reserve the shared VIP (idempotent) ──────────────────────────────────
# The k8s Service that fronts Kamailio also references this address via
# spec.loadBalancerIP. Reserving it with SHARED_LOADBALANCER_VIP lets two
# forwarding rules (one from k8s, one from this script) attach to it.
if ! gcloud compute addresses describe "$ADDR_NAME" --region="$REGION" >/dev/null 2>&1; then
  echo "→ Reserving internal address $ADDR_NAME=$VIP (purpose=SHARED_LOADBALANCER_VIP)"
  gcloud compute addresses create "$ADDR_NAME" \
    --region="$REGION" \
    --subnet="$SUBNET" \
    --addresses="$VIP" \
    --purpose=SHARED_LOADBALANCER_VIP
else
  echo "✓ Address $ADDR_NAME already reserved"
fi

# ── 2. Health check (HTTP GET /healthz on the recorder's hostPort) ──────────
if ! gcloud compute health-checks describe "$HC_NAME" --region="$REGION" >/dev/null 2>&1; then
  echo "→ Creating regional HTTP health check $HC_NAME → ${HEALTH_PATH} on TCP/${HEALTH_PORT}"
  gcloud compute health-checks create http "$HC_NAME" \
    --region="$REGION" \
    --port="$HEALTH_PORT" \
    --request-path="$HEALTH_PATH" \
    --check-interval=10s \
    --timeout=5s \
    --healthy-threshold=2 \
    --unhealthy-threshold=3
else
  echo "✓ Health check $HC_NAME exists"
fi

# ── 3. Backend service (UDP, internal passthrough) ──────────────────────────
if ! gcloud compute backend-services describe "$BS_NAME" --region="$REGION" >/dev/null 2>&1; then
  echo "→ Creating regional backend service $BS_NAME"
  gcloud compute backend-services create "$BS_NAME" \
    --region="$REGION" \
    --load-balancing-scheme=INTERNAL \
    --protocol=UDP \
    --health-checks-region="$REGION" \
    --health-checks="$HC_NAME"
else
  echo "✓ Backend service $BS_NAME exists"
fi

# ── 4. Attach recorder-node instance groups ────────────────────────────────
# GKE creates one managed instance group per zone per node pool, named
# gke-<cluster>-<pool>-<hash>-grp. Discover and attach all of them.
echo "→ Discovering instance groups for node pool '$RECORDER_NODE_POOL'…"
mapfile -t IGS < <(gcloud compute instance-groups list \
  --filter="name~'^gke-.*-${RECORDER_NODE_POOL}-' AND region:($REGION)" \
  --format="value(name,zone)")

if [[ ${#IGS[@]} -eq 0 ]]; then
  echo "✗ No instance groups found for pool $RECORDER_NODE_POOL in $REGION" >&2
  exit 1
fi

for entry in "${IGS[@]}"; do
  IG_NAME="$(awk '{print $1}' <<<"$entry")"
  IG_ZONE="$(awk '{print $2}' <<<"$entry")"
  IG_ZONE="${IG_ZONE##*/}"

  if gcloud compute backend-services describe "$BS_NAME" --region="$REGION" \
        --format="value(backends.group)" | grep -q "/$IG_NAME$"; then
    echo "  ✓ $IG_NAME ($IG_ZONE) already attached"
  else
    echo "  → Attaching $IG_NAME ($IG_ZONE)"
    gcloud compute backend-services add-backend "$BS_NAME" \
      --region="$REGION" \
      --instance-group="$IG_NAME" \
      --instance-group-zone="$IG_ZONE"
  fi
done

# ── 5. Forwarding rule on the shared VIP, UDP port range ────────────────────
if ! gcloud compute forwarding-rules describe "$FR_NAME" --region="$REGION" >/dev/null 2>&1; then
  echo "→ Creating forwarding rule $FR_NAME on $VIP UDP/$PORT_RANGE"
  gcloud compute forwarding-rules create "$FR_NAME" \
    --region="$REGION" \
    --load-balancing-scheme=INTERNAL \
    --network="$NETWORK" \
    --subnet="$SUBNET" \
    --address="$VIP" \
    --ip-protocol=UDP \
    --ports="$PORT_RANGE" \
    --backend-service="$BS_NAME" \
    --backend-service-region="$REGION"
else
  echo "✓ Forwarding rule $FR_NAME exists"
fi

echo
echo "✅ Media LB ready: SBC may send UDP/$PORT_RANGE to $VIP → recorder nodes."
echo "   Health: GET http://<recorder-node>:${HEALTH_PORT}${HEALTH_PATH} (served by the recorder pod)."
