# hospital-agent-sidecar

Go sidecar binary that runs alongside OpenClaw/Hermes agent gateways. Watches the agent process, detects crashes, runs deterministic pattern matching (L0) for immediate repairs, and pushes crash context to Agent Hospital server for centralized AI diagnosis.

Zero external Go dependencies. Single static binary (~6MB).

## Architecture

```
                          AGENT VM (e.g. hermes-design, internal-automations)
 +-------------------------------------------------------------------------------------------+
 |                                                                                           |
 |   +---------------------+         +--------------------------------------------------+   |
 |   | openclaw-gateway     |         | hospital-agent-sidecar (Go, port 18792, systemd)  |   |
 |   | or hermes-gateway    |         |                                                  |   |
 |   | (systemd, port 18789)|         |  Watcher          polls systemctl every 5s       |   |
 |   |                      |<--------|  Collector         journalctl, dmesg, disk, mem  |   |
 |   |  LLM sessions        |  watch  |  Diagnoser (L0)    pattern match (exit codes)    |   |
 |   |  Slack/Discord/WA    |         |  Repairer          6 whitelisted actions         |   |
 |   |  Cron jobs            |         |  Heartbeat         60s ticker + crash listener  |   |
 |   +---------------------+         |                                                  |   |
 |            ^                       |  Endpoints:                                      |   |
 |            |                       |    GET  /healthz    (no auth)                     |   |
 |            | restart /             |    GET  /health     (bearer token)                |   |
 |            | kill-port /           |    POST /repair     (bearer token)                |   |
 |            | kill-zombies          +--------------------------------------------------+   |
 |            |                                    |                                         |
 +------------|------------------------------------|-----------------------------------------+
              |                                    |
              |  systemctl --user restart           |  HTTP (x-api-key header)
              |  pkill / lsof+kill                  |
              |                                    v
                               +-------------------------------------------+
                               |  Agent Hospital Server                     |
                               |  (agent-hospital repo, Node.js)            |
                               |                                           |
                               |  POST /api/v1/heartbeat                   |
                               |    <- status, metrics, callbackUrl        |
                               |    -> ack, inline commands                |
                               |                                           |
                               |  POST /api/v1/heartbeat/crash             |
                               |    <- crash context, L0 diagnosis,        |
                               |       local repair results                |
                               |    -> AI diagnosis + repair commands      |
                               |                                           |
                               |  POST /api/v1/heartbeat/repair-result     |
                               |    <- results of hospital commands        |
                               |    -> ack                                 |
                               |                                           |
                               |  AI Engine: diagnoseLogs() + planRepair() |
                               |    via Claude API (centralized, secure)   |
                               |                                           |
                               |  Absence Detector (30s interval)          |
                               |    no heartbeat > 180s? probe sidecar    |
                               |    unreachable? -> Slack alert            |
                               +-------------------------------------------+
```

## Crash Recovery Flow

```
T+0s   Gateway crashes (systemd unit goes "failed")
T+5s   Sidecar watcher detects via systemctl poll
T+5s   Collector gathers: exit code, journalctl, dmesg, disk, memory, log tail
T+5s   L0 pattern match (deterministic, instant, no external deps):
         exit 137 -> OOM -> kill-zombies + restart
         ENOSPC   -> disk full -> clear-logs + clear-cache + restart
         EADDRINUSE -> port conflict -> kill-port + restart
         unknown  -> restart-gateway
T+5s   Execute L0 repairs locally (immediate, no network needed)
T+10s  Restart succeeds, 30s grace period
T+40s  Push crash context to Hospital Server
T+65s  Hospital AI diagnosis received (via Claude API on server side)
       Hospital responds with additional commands if local fix was insufficient
```

## Installation

### From source

```bash
git clone git@github.com:Clawies/hospital-agent-sidecar.git
cd hospital-agent-sidecar
make build-linux    # cross-compile for linux/amd64
```

### Deploy to a VM

The deploy script handles everything: build, SCP, systemd install, and verification.

```bash
./deploy/deploy.sh <vm-name> <zone> <inbound-token> <api-key> <hospital-url>
```

**OpenClaw example:**
```bash
./deploy/deploy.sh internal-automations asia-south2-a \
  $(openssl rand -hex 16) \
  ah_$(openssl rand -hex 16) \
  http://10.160.0.24:4000
```

**Hermes example:**
```bash
FRAMEWORK=hermes \
STATE_DIR=/home/themadme/.hermes \
SYSTEMD_UNIT=hermes-gateway.service \
  ./deploy/deploy.sh hermes-design asia-south1-b \
    $(openssl rand -hex 16) \
    ah_$(openssl rand -hex 16) \
    http://10.160.0.24:4000
```

### Manual install

1. Copy the binary to the target VM:
   ```bash
   scp bin/hospital-agent-sidecar-linux-amd64 user@vm:~/.local/bin/hospital-agent-sidecar
   chmod +x ~/.local/bin/hospital-agent-sidecar
   ```

2. Create the env file at `~/.config/hospital-agent-sidecar/agent.env`:
   ```
   HOSPITAL_AGENT_PORT=18792
   HOSPITAL_AGENT_INBOUND_TOKEN=<generate with: openssl rand -hex 16>
   HOSPITAL_AGENT_API_KEY=<from hospital server>
   HOSPITAL_AGENT_HOSPITAL_URL=http://<hospital-ip>:4000
   HOSPITAL_AGENT_STATE_DIR=/home/user/.openclaw
   HOSPITAL_AGENT_SYSTEMD_UNIT=openclaw-gateway.service
   HOSPITAL_AGENT_FRAMEWORK=openclaw
   HOSPITAL_AGENT_GATEWAY_PORT=18789
   HOSPITAL_AGENT_GATEWAY_URL=http://localhost:18789
   HOSPITAL_AGENT_HEARTBEAT_INTERVAL=60
   HOSPITAL_AGENT_NAME=my-agent-vm
   ```

3. Install the systemd unit:
   ```bash
   cp deploy/hospital-agent-sidecar.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now hospital-agent-sidecar.service
   ```

4. Verify:
   ```bash
   systemctl --user status hospital-agent-sidecar
   curl http://localhost:18792/healthz
   ```

## Auth

| Direction | Method | Token |
|-----------|--------|-------|
| Sidecar -> Hospital | `x-api-key` header | `HOSPITAL_AGENT_API_KEY` |
| Hospital -> Sidecar | `Authorization: Bearer` | `HOSPITAL_AGENT_INBOUND_TOKEN` |

## Repair Whitelist

Each action has 60s cooldown and max 3 attempts per incident.

| Action | What it does |
|--------|-------------|
| `restart-gateway` | `systemctl --user reset-failed + restart`, verify active |
| `kill-zombies` | `pkill -9` framework processes (openclaw or hermes aware) |
| `clear-logs` | Delete logs >7d, truncate >100MB |
| `clear-disk-cache` | Prune sessions/completions/cache >30d |
| `emergency-disk` | Delete 50MB pre-allocated reserve file |
| `kill-port` | `lsof + kill -9` on gateway port |

## Config (env vars)

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `HOSPITAL_AGENT_PORT` | `18792` | no | Sidecar listen port |
| `HOSPITAL_AGENT_INBOUND_TOKEN` | - | yes | Bearer token for inbound requests |
| `HOSPITAL_AGENT_API_KEY` | - | yes | x-api-key for hospital server |
| `HOSPITAL_AGENT_HOSPITAL_URL` | - | yes | Hospital server base URL |
| `HOSPITAL_AGENT_STATE_DIR` | `/home/themadme/.openclaw` | no | Agent runtime home dir |
| `HOSPITAL_AGENT_SYSTEMD_UNIT` | `openclaw-gateway.service` | no | Systemd unit to watch |
| `HOSPITAL_AGENT_FRAMEWORK` | `openclaw` | no | `openclaw` or `hermes` |
| `HOSPITAL_AGENT_GATEWAY_PORT` | `18789` | no | Gateway port (for kill-port repair) |
| `HOSPITAL_AGENT_GATEWAY_URL` | _(empty)_ | no | Gateway HTTP URL for health ping (empty = skip, set for OpenClaw) |
| `HOSPITAL_AGENT_HEARTBEAT_INTERVAL` | `60` | no | Seconds between heartbeats |
| `HOSPITAL_AGENT_NAME` | hostname | no | Human-readable agent name |
| `HOSPITAL_AGENT_LLM_HEALTH_URL` | _(empty)_ | no | LLM provider health URL (empty = skip) |
| `HOSPITAL_AGENT_LLM_HEALTH_AUTH` | _(empty)_ | no | Auth header for LLM health check |

### LLM Health Check Examples

The sidecar can monitor any OpenAI-compatible LLM provider. Set `LLM_HEALTH_URL` to the
provider's `/v1/models` endpoint and `LLM_HEALTH_AUTH` to the appropriate credentials.

| Provider | LLM_HEALTH_URL | LLM_HEALTH_AUTH |
|----------|---------------|-----------------|
| Local claude-max-api proxy | `http://localhost:3456/v1/models` | _(none needed)_ |
| OpenRouter | `https://openrouter.ai/api/v1/models` | `Bearer sk-or-v1-...` |
| OpenAI | `https://api.openai.com/v1/models` | `Bearer sk-...` |
| Anthropic (via proxy) | `http://localhost:3456/v1/models` | _(none needed)_ |
| Any OpenAI-compatible | `https://your-endpoint/v1/models` | `Bearer your-key` |
| Anthropic (direct) | `https://api.anthropic.com/v1/models` | `x-api-key sk-ant-...` |

Auth header formats:
- `Bearer sk-or-...` -- sets `Authorization: Bearer sk-or-...`
- `x-api-key sk-ant-...` -- sets `x-api-key: sk-ant-...`
- `sk-or-...` (bare key) -- auto-wrapped as `Authorization: Bearer sk-or-...`

Detects: unreachable (timeout), auth failure (401/403), credits exhausted (429), server error (5xx).

## Project Structure

```
cmd/hospital-agent/main.go        entrypoint, signal handling
internal/
  config/config.go                 env var loading, validation
  auth/auth.go                     bearer token middleware
  server/server.go                 HTTP server, dependency wiring
  handlers/handlers.go             /healthz, /health, /repair endpoints
  collector/collector.go           crash context collection (journalctl, dmesg, disk, mem)
  diagnosis/
    diagnoser.go                   L0 orchestrator
    pattern.go                     deterministic pattern matching
  repair/repair.go                 whitelist, cooldown, exec
  watcher/watcher.go               systemd unit polling, crash detection
  heartbeat/heartbeat.go           hospital push (heartbeat, crash, repair-result)
deploy/
  deploy.sh                        cross-compile + SCP + systemd install
  hospital-agent-sidecar.service    user systemd unit
```

## Related Repos

| Repo | Purpose |
|------|---------|
| [agent-hospital](https://github.com/Clawies/agent-hospital) | Hospital server -- AI diagnosis, repair planning, absence detection |
| [hospital-agent-sidecar](https://github.com/Clawies/hospital-agent-sidecar) | This repo -- Go sidecar for crash detection and repair |
| [agent-hospital-client](https://github.com/Clawies/agent-hospital-client) | npm client SDK for pull-based healing |
| [agent-hospital-mcp](https://github.com/Clawies/agent-hospital-mcp) | MCP server (heal, diagnose, check_health tools) |
