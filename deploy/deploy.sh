#!/usr/bin/env bash
set -euo pipefail

# Deploy hospital-sidecar to a target VM via gcloud IAP tunnel.
#
# Usage:
#   ./deploy/deploy.sh <vm-name> <zone> <inbound-token> <api-key> <hospital-url>
#
# Environment overrides:
#   STATE_DIR             (default: /home/themadme/.openclaw)
#   SYSTEMD_UNIT          (default: openclaw-gateway.service)
#   FRAMEWORK             (default: openclaw)
#   GATEWAY_PORT          (default: 18789)
#   AGENT_PORT            (default: 18793)
#   HEARTBEAT_INTERVAL    (default: 60)
#   LLM_HEALTH_URL        (optional, auto-detected from openclaw.json by the sidecar)
#   LLM_HEALTH_AUTH       (optional, auto-detected from openclaw.json by the sidecar)
#   SKIP_BUILD            (set to "true" to skip cross-compile, used by deploy-all.sh)

VM=${1:-}
ZONE=${2:-}
INBOUND_TOKEN=${3:-}
API_KEY=${4:-}
HOSPITAL_URL=${5:-}

STATE_DIR=${STATE_DIR:-/home/themadme/.openclaw}
SYSTEMD_UNIT=${SYSTEMD_UNIT:-openclaw-gateway.service}
FRAMEWORK=${FRAMEWORK:-openclaw}
GATEWAY_PORT=${GATEWAY_PORT:-18789}
AGENT_PORT=${AGENT_PORT:-18793}
HEARTBEAT_INTERVAL=${HEARTBEAT_INTERVAL:-60}
LLM_HEALTH_URL=${LLM_HEALTH_URL:-}
LLM_HEALTH_AUTH=${LLM_HEALTH_AUTH:-}

if [[ -z "$VM" || -z "$ZONE" || -z "$INBOUND_TOKEN" || -z "$API_KEY" || -z "$HOSPITAL_URL" ]]; then
  echo "Usage: $0 <vm> <zone> <inbound-token> <api-key> <hospital-url>"
  echo ""
  echo "Example:"
  echo "  $0 internal-automations asia-south2-a tok123 ah_abc123 http://10.160.0.24:4000"
  echo ""
  echo "Hermes example:"
  echo "  FRAMEWORK=hermes STATE_DIR=/home/themadme/.hermes SYSTEMD_UNIT=hermes-gateway.service \\"
  echo "    $0 hermes-design asia-south1-b tok456 ah_def456 http://10.160.0.24:4000"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BIN="$PROJECT_DIR/bin/hospital-sidecar-linux-amd64"
UNIT="$PROJECT_DIR/internal/setup/hospital-sidecar.service"

if [[ "${SKIP_BUILD:-}" != "true" ]]; then
  echo "=== Building linux/amd64 ==="
  cd "$PROJECT_DIR"
  make build-linux
fi

if [[ ! -f "$BIN" ]]; then
  echo "ERROR: Binary not found at $BIN"
  exit 1
fi

echo "=== Copying to $VM ($ZONE) ==="
gcloud compute scp \
  --tunnel-through-iap \
  --zone="$ZONE" \
  "$BIN" "$UNIT" \
  "themadme@${VM}:/tmp/"

echo "=== Installing on $VM ==="
gcloud compute ssh \
  --tunnel-through-iap \
  --zone="$ZONE" \
  "themadme@${VM}" \
  --command="
    set -e

    # Create directories
    mkdir -p \$HOME/.local/bin \$HOME/.config/systemd/user \$HOME/.config/hospital-agent-sidecar

    # Install binary (new name: hospital-sidecar)
    install -m 0755 /tmp/hospital-sidecar-linux-amd64 \$HOME/.local/bin/hospital-sidecar

    # Install systemd unit (new name: hospital-sidecar.service)
    install -m 0644 /tmp/hospital-sidecar.service \$HOME/.config/systemd/user/hospital-sidecar.service

    # Stop old units from previous versions
    systemctl --user stop hospital-agent-sidecar.service 2>/dev/null || true
    systemctl --user disable hospital-agent-sidecar.service 2>/dev/null || true
    systemctl --user stop hospital-agent.service 2>/dev/null || true
    systemctl --user disable hospital-agent.service 2>/dev/null || true

    # Write env file
    cat > \$HOME/.config/hospital-agent-sidecar/agent.env <<EOF
HOSPITAL_AGENT_PORT=$AGENT_PORT
HOSPITAL_AGENT_INBOUND_TOKEN=$INBOUND_TOKEN
HOSPITAL_AGENT_API_KEY=$API_KEY
HOSPITAL_AGENT_HOSPITAL_URL=$HOSPITAL_URL
HOSPITAL_AGENT_STATE_DIR=$STATE_DIR
HOSPITAL_AGENT_SYSTEMD_UNIT=$SYSTEMD_UNIT
HOSPITAL_AGENT_FRAMEWORK=$FRAMEWORK
HOSPITAL_AGENT_GATEWAY_PORT=$GATEWAY_PORT
HOSPITAL_AGENT_HEARTBEAT_INTERVAL=$HEARTBEAT_INTERVAL
HOSPITAL_AGENT_NAME=$VM
$([ "$FRAMEWORK" = "openclaw" ] && echo "HOSPITAL_AGENT_GATEWAY_URL=http://localhost:$GATEWAY_PORT")
$([ -n "$LLM_HEALTH_URL" ] && echo "HOSPITAL_AGENT_LLM_HEALTH_URL=$LLM_HEALTH_URL")
$([ -n "$LLM_HEALTH_AUTH" ] && echo "HOSPITAL_AGENT_LLM_HEALTH_AUTH=$LLM_HEALTH_AUTH")
EOF
    chmod 0600 \$HOME/.config/hospital-agent-sidecar/agent.env

    # Enable linger (keep user services alive after logout)
    loginctl enable-linger themadme 2>/dev/null || true

    # Reload and restart
    systemctl --user daemon-reload
    systemctl --user enable hospital-sidecar.service
    systemctl --user restart hospital-sidecar.service

    # Wait and verify
    sleep 2
    echo '--- Status ---'
    systemctl --user status hospital-sidecar.service --no-pager || true
    echo ''
    echo '--- Healthz ---'
    curl -sS --max-time 3 http://127.0.0.1:$AGENT_PORT/healthz && echo
  "

echo "=== Deploy complete ==="
