#!/usr/bin/env bash
set -euo pipefail

# Mass-deploy hospital-agent-sidecar to all known agent VMs.
#
# Usage:
#   ./deploy/deploy-all.sh <hospital-url>
#   ./deploy/deploy-all.sh <hospital-url> --force     # redeploy even if already running
#   ./deploy/deploy-all.sh <hospital-url> --dry-run    # show what would be deployed
#
# LLM health check is auto-detected from openclaw.json on each VM.
# Tokens are auto-generated per VM using openssl rand.
# After deploy, prints a summary table with all credentials.

HOSPITAL_URL=${1:-}
FORCE=false
DRY_RUN=false

# Parse flags
shift || true
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=true ;;
    --dry-run) DRY_RUN=true ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

if [[ -z "$HOSPITAL_URL" ]]; then
  echo "Usage: $0 <hospital-url> [--force] [--dry-run]"
  echo ""
  echo "Example:"
  echo "  $0 http://10.160.0.24:4000"
  echo "  $0 http://10.160.0.24:4000 --dry-run"
  echo "  $0 http://10.160.0.24:4000 --force"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
RESULTS_FILE=$(mktemp)
trap "rm -f $RESULTS_FILE" EXIT

# VM registry: VM|ZONE|FRAMEWORK|STATE_DIR|SYSTEMD_UNIT|GATEWAY_PORT|LABEL
TARGETS=(
  "amit-design-product-claw|asia-south1-b|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Anya"
  "atlas-amit-prd|asia-south1-b|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Atlas"
  "ba-iris-amit|asia-south1-b|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Iris"
  "graphic-pitch-claw-amit|asia-south1-b|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Pitch"
  "engineering-claw|asia-south2-a|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Nova"
  "shail-openclaw-server|us-central1-b|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Shail"
  "rajans-openclaw|asia-south2-a|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Rajan"
  "nazara-v2|asia-south2-a|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|Vision"
  "internal-automations|asia-south2-a|openclaw|/home/themadme/.openclaw|openclaw-gateway.service|18789|InternalAuto"
  "hermes-design|asia-south1-b|hermes|/home/themadme/.hermes|hermes-gateway.service|18789|HermesDesign"
)

echo "============================================"
echo "  Hospital Agent Sidecar - Mass Deploy"
echo "============================================"
echo "Hospital URL: $HOSPITAL_URL"
echo "VMs: ${#TARGETS[@]}"
echo "Force: $FORCE"
echo "Dry run: $DRY_RUN"
echo ""

# Build binary once
echo "=== Building linux/amd64 ==="
cd "$PROJECT_DIR"
make build-linux
echo ""

DEPLOYED=0
SKIPPED=0
FAILED=0

for entry in "${TARGETS[@]}"; do
  IFS='|' read -r VM ZONE FRAMEWORK STATE_DIR SYSTEMD_UNIT GATEWAY_PORT LABEL <<< "$entry"

  echo "--- $VM ($LABEL) [$ZONE, $FRAMEWORK] ---"

  # Check if sidecar is already running (unless --force)
  if [[ "$FORCE" != "true" ]]; then
    STATUS=$(gcloud compute ssh \
      --tunnel-through-iap --quiet \
      --zone="$ZONE" \
      "themadme@${VM}" \
      --command="systemctl --user is-active hospital-sidecar.service 2>/dev/null || systemctl --user is-active hospital-agent-sidecar.service 2>/dev/null || echo inactive" \
      2>/dev/null || echo "ssh-failed")

    STATUS=$(echo "$STATUS" | tail -1 | tr -d '[:space:]')

    if [[ "$STATUS" == "active" ]]; then
      echo "  SKIP: already running"
      echo "$VM|$ZONE|$LABEL|SKIPPED|already-running|-|-" >> "$RESULTS_FILE"
      SKIPPED=$((SKIPPED + 1))
      echo ""
      continue
    fi
  fi

  # Generate unique credentials
  INBOUND_TOKEN=$(openssl rand -hex 16)
  API_KEY="ah_$(openssl rand -hex 16)"

  if [[ "$DRY_RUN" == "true" ]]; then
    echo "  DRY RUN: would deploy with token=${INBOUND_TOKEN:0:8}... api_key=${API_KEY:0:11}..."
    echo "$VM|$ZONE|$LABEL|DRY_RUN|-|$INBOUND_TOKEN|$API_KEY" >> "$RESULTS_FILE"
    echo ""
    continue
  fi

  # Deploy
  echo "  Deploying..."
  if SKIP_BUILD=true \
     FRAMEWORK="$FRAMEWORK" \
     STATE_DIR="$STATE_DIR" \
     SYSTEMD_UNIT="$SYSTEMD_UNIT" \
     GATEWAY_PORT="$GATEWAY_PORT" \
     "$SCRIPT_DIR/deploy.sh" "$VM" "$ZONE" "$INBOUND_TOKEN" "$API_KEY" "$HOSPITAL_URL" 2>&1; then
    echo "  OK"
    echo "$VM|$ZONE|$LABEL|OK|-|$INBOUND_TOKEN|$API_KEY" >> "$RESULTS_FILE"
    DEPLOYED=$((DEPLOYED + 1))
  else
    echo "  FAILED"
    echo "$VM|$ZONE|$LABEL|FAILED|deploy-error|-|-" >> "$RESULTS_FILE"
    FAILED=$((FAILED + 1))
  fi
  echo ""
done

# Summary
echo ""
echo "============================================"
echo "  DEPLOY SUMMARY"
echo "============================================"
echo "Deployed: $DEPLOYED | Skipped: $SKIPPED | Failed: $FAILED"
echo ""

printf "%-30s %-10s %-8s %-34s %s\n" "VM" "LABEL" "STATUS" "INBOUND_TOKEN" "API_KEY"
printf "%-30s %-10s %-8s %-34s %s\n" "---" "---" "---" "---" "---"

while IFS='|' read -r VM ZONE LABEL STATUS REASON TOKEN KEY; do
  printf "%-30s %-10s %-8s %-34s %s\n" "$VM" "$LABEL" "$STATUS" "${TOKEN:--}" "${KEY:--}"
done < "$RESULTS_FILE"

echo ""
echo "Save the tokens above -- needed for hospital server agent registration."
echo "============================================"
