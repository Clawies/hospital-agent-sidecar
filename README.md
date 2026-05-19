# hospital-agent

Go sidecar binary that runs alongside OpenClaw/Hermes agent gateways. Watches the agent process, detects crashes, runs deterministic pattern matching (L0) for immediate repairs, and pushes crash context to Agent Hospital for centralized AI diagnosis.

Zero external Go dependencies. Single static binary (~6MB).

## Architecture

```
                          AGENT VM (e.g. hermes-design, internal-automations)
 +-------------------------------------------------------------------------------------------+
 |                                                                                           |
 |   +---------------------+         +--------------------------------------------------+   |
 |   | openclaw-gateway     |         | hospital-agent (Go, port 18792, systemd)          |   |
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
              |  systemctl --user restart           |  HTTPS (x-api-key header)
              |  pkill / lsof+kill                  |
              |                                    |
              +------------------------------------+
                                                   |
                                                   v
                               +-------------------------------------------+
                               |  Hospital Server (agent-hospital)          |
                               |  Node.js, port 4000, GCP VM               |
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
                               |  AI Engine (ai.service.ts)                |
                               |    diagnoseLogs()  -- Claude API           |
                               |    planRepair()    -- repair planning      |
                               |    learnFromRepair() -- pattern memory    |
                               |                                           |
                               |  Absence Detector (30s interval)          |
                               |    no heartbeat > 180s? probe sidecar    |
                               |    unreachable? -> Slack alert            |
                               +-------------------------------------------+
```

## Crash Recovery Flow

```
T+0s   Gateway crashes (systemd unit goes "failed")
T+5s   Watcher detects via systemctl poll
T+5s   Collector gathers: exit code, journalctl, dmesg, disk, memory, log tail
T+5s   L0 pattern match (deterministic, instant):
         exit 137 -> OOM -> kill-zombies + restart
         ENOSPC   -> disk full -> clear-logs + clear-cache + restart
         EADDRINUSE -> port conflict -> kill-port + restart
         unknown  -> restart-gateway
T+5s   Execute L0 repairs locally
T+10s  Restart succeeds, 30s grace period
T+40s  Push crash context to Hospital Server
T+65s  Hospital responds with AI diagnosis + commands (if local fix insufficient)
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

```
HOSPITAL_AGENT_PORT=18792
HOSPITAL_AGENT_INBOUND_TOKEN=<required>
HOSPITAL_AGENT_API_KEY=<required>
HOSPITAL_AGENT_HOSPITAL_URL=<required>
HOSPITAL_AGENT_STATE_DIR=/home/themadme/.openclaw
HOSPITAL_AGENT_SYSTEMD_UNIT=openclaw-gateway.service
HOSPITAL_AGENT_FRAMEWORK=openclaw   # or "hermes"
HOSPITAL_AGENT_GATEWAY_PORT=18789
HOSPITAL_AGENT_GATEWAY_URL=http://localhost:18789
HOSPITAL_AGENT_HEARTBEAT_INTERVAL=60
HOSPITAL_AGENT_NAME=<vm-name>
```

## Build

```bash
make build          # native
make build-linux    # cross-compile linux/amd64
```

## Deploy

```bash
./deploy/deploy.sh <vm> <zone> <inbound-token> <api-key> <hospital-url>

# Environment overrides:
FRAMEWORK=hermes STATE_DIR=/home/themadme/.hermes SYSTEMD_UNIT=hermes-gateway.service \
  ./deploy/deploy.sh hermes-design asia-south1-b tok123 ah_abc https://hospital.example.com
```

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
  hospital-agent.service            user systemd unit
```
