# @agent-hospital/sidecar

Crash detection, auto-repair, and health monitoring for AI agent gateways (OpenClaw, Hermes).

Runs alongside your agent gateway as a systemd service. Detects crashes in 5 seconds, runs instant local repairs, monitors LLM provider health, and reports to [Agent Hospital](https://agent-hospital.ai) for AI-powered diagnosis.

## Quick Start

```bash
npx @agent-hospital/sidecar setup \
  --api-key YOUR_API_KEY \
  --hospital-url https://api.agent-hospital.ai
```

That's it. The sidecar auto-detects your framework, LLM provider, and gateway configuration.

## What It Does

- Watches your gateway process (systemd polling every 5s)
- On crash: collects logs, exit code, disk/memory stats, runs pattern-matched repairs instantly
- Reports crash context to Agent Hospital for AI diagnosis (Claude-powered)
- Monitors LLM provider health (OpenRouter, local proxies, any OpenAI-compatible endpoint)
- Sends heartbeats every 60s so Agent Hospital can detect if your VM goes down

## Commands

```bash
hospital-sidecar setup       # Install and start as a systemd service
hospital-sidecar status      # Check if the sidecar is running
hospital-sidecar uninstall   # Stop and remove everything
hospital-sidecar version     # Print version
```

## Setup Options

```
--api-key KEY        (required) Your Agent Hospital API key
--hospital-url URL   (required) Agent Hospital server URL
--inbound-token TOK  Bearer token for inbound requests (auto-generated if omitted)
--framework NAME     openclaw or hermes (auto-detected from ~/.openclaw or ~/.hermes)
--state-dir DIR      Agent home directory (auto-detected)
--systemd-unit NAME  Systemd unit to watch (auto-detected)
--gateway-port PORT  Gateway port (default 18789)
--port PORT          Sidecar listen port (default 18793)
--name NAME          Agent name (default hostname)
```

## Auto-Detection

The sidecar reads your agent's config file to auto-detect:

- **Framework**: Checks for `~/.openclaw/` or `~/.hermes/`
- **LLM provider**: Reads `openclaw.json` to find the primary model provider (OpenRouter, local proxy, Anthropic, etc.)
- **Health check URL**: Constructs the right `/v1/models` endpoint for your provider
- **Auth**: Resolves API keys from config or environment variables

## Requirements

- Linux with systemd (user-level services)
- OpenClaw or Hermes agent gateway running
- Node.js >= 16 (for npm install only -- the sidecar itself is a static Go binary)

## How It Works

1. **Install**: Downloads a pre-built Go binary (~6MB) for your platform
2. **Setup**: Creates a systemd user service with your config
3. **Run**: Binary runs as a background service, survives SSH logout
4. **Monitor**: Polls gateway status, LLM health, disk/memory every few seconds
5. **Repair**: On crash, runs deterministic L0 repairs (restart, kill zombies, clear disk)
6. **Report**: Pushes crash context to Agent Hospital for AI diagnosis
7. **Learn**: Hospital learns from successful repairs for future incidents
