# OpenAI-Compatible API Gateway with IP Rotation

Lightweight and performant API gateway that provides OpenAI-compatible endpoints with automatic IP rotation using ProtonVPN WireGuard containers.

> 📚 **Full documentation** in [`docs/`](docs/README.md): [core features](docs/core-features.md), [architecture](docs/architecture.md), [ProtonVPN integration](docs/protonvpn-integration.md), and [business logic](docs/business-logic.md).

## Features

- **OpenAI-Compatible API**: Drop-in replacement for OpenAI API clients
- **Automatic IP Rotation**: Rotates through ProtonVPN regions *and* spreads containers across different servers for diverse exit IPs
- **Native ProtonVPN WireGuard**: SRP username/password authentication with automatic certificate issuance and X25519 key derivation — no manual key files
- **Rate Limit Detection**: Automatically detects and handles rate limits (429 errors)
- **Streaming Support**: Full support for Server-Sent Events (SSE) streaming
- **Retry Logic**: Exponential backoff with configurable retries
- **Sticky Sessions**: Optional conversation-based session persistence
- **Health Monitoring**: Background health checks for all proxies
- **Token Metrics & Cost Estimation**: Per-request token usage (prompt/completion/cached) and configurable per-model cost estimation
- **Prometheus Endpoint**: `/metrics` with request/token counters
- **Resource Efficient**: Minimal memory footprint, fast startup
- **Web Dashboard**: Neobrutalism-styled UI with live metrics, traffic chart, model usage, token totals and estimated cost (SQLite-backed)
- **Chat UI**: ChatGPT-like interface with SSE streaming and sticky-session conversations

## Quick Start

### Prerequisites

- Go 1.20+ (for building from source)
- Docker (for running VPN containers)
- Docker Compose (optional, for easy deployment)
- A ProtonVPN account (free tier works)

### Installation

1. Clone the repository:
```bash
git clone https://github.com/hansputera/opencode-multi-agents.git
cd opencode-multi-agents
```

2. Build the binary:
```bash
make build
```

3. Create a `.env` file:
```bash
cp .env.example .env
# Edit .env with your configuration
```

4. Set your ProtonVPN credentials in `.env`:
```bash
PROTONVPN_USERNAME=your-protonvpn-username
PROTONVPN_PASSWORD=your-protonvpn-password
```

5. Run the gateway:
```bash
./bin/gateway
```

## ProtonVPN Setup

The gateway authenticates to ProtonVPN natively — no manual WireGuard config downloads needed:

1. The gateway performs a full **SRP login flow** against `account.protonvpn.com` (session → cookies → SRP proof → user info), matching what the ProtonVPN web app does.
2. It fetches the **server list** from `vpn-api.proton.me` and picks online free servers in your configured regions (`PROTONVPN_REGIONS`).
3. For each proxy container it requests a **VPN certificate** with an auto-generated Ed25519 key pair, then derives the WireGuard **X25519** key from it (required — independently generated keys are silently dropped by ProtonVPN servers).
4. Each container brings up a `wg-quick` tunnel plus a `gost` SOCKS5 sidecar; all upstream traffic egresses through the tunnel.

Credentials, sessions, certificates, and server cache are persisted in SQLite (`PROTONVPN_STORE_PATH`), so restarts don't re-authenticate unnecessarily.

If a certificate key conflicts (409/2500), the gateway regenerates the Ed25519 key automatically. Certificate creation is serialized to avoid churn.

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `LISTEN_ADDR` | Server listen address | `:8082` |
| `UPSTREAM_BASE_URL` | Upstream API base URL (OpenAI-compatible, `/v1` auto-appended if missing) | `https://opencode.ai/zen/v1` |
| `UPSTREAM_API_KEY` | Upstream API key (OpenCode Zen: `zent-...`). Leave empty for the public tier — the gateway then authenticates with `x-api-key: public` | - |
| `UPSTREAM_API_KEYS` | Comma-separated key list; a **random** key is picked per request (when the client sends no `Authorization`) to spread quota/rate limits across accounts. Entries: `public`, `zent-...`, or `header:value` | - |
| `UPSTREAM_PROVIDER` | Upstream driver: `zen` (default, raw OpenAI-compatible), `opencode` (drive an [OpenCode Server](https://opencode.ai/docs/server) via HTTP) or `opencode-cli` (docker-exec `opencode run` inside each VPN container). In all cases each request egresses through a unique per-container VPN IP | `zen` |
| `OPENCODE_SERVER_URL` | Base URL of `opencode serve` (used when `UPSTREAM_PROVIDER=opencode`). Must be reachable through the proxy container's SOCKS5 tunnel | `http://127.0.0.1:4096` |
| `OPENCODE_SERVER_PASSWORD` | Optional `OPENCODE_SERVER_PASSWORD` for OpenCode Server basic auth (`opencode`-mode) | - |
| `OPENCODE_PROVIDER_ID` | OpenCode provider used for `/session/:id/message` (e.g. `openai`, `anthropic`). If empty, a client `Authorization: Bearer <key>` is PUT to `/auth/<provider>` | - |
| `OPENCODE_MODEL` | Model handed to OpenCode's `/session/:id/message` (e.g. `gpt-5.1-codex`) | - |
| `OPENCODE_CLI_MODEL` | Model override for `opencode run --model` (`opencode-cli` mode; empty = client's `model` field or the CLI default) | - |
| `OPENCODE_CLI_PROVIDER_ENV` | Container env var injected with the client's credential per exec (`opencode-cli` mode) | `ANTHROPIC_API_KEY` |
| `OPENCODE_CLI_ARGS` | Extra `opencode run` args (space-separated, e.g. `--agent build`) | - |
| `OPENCODE_CLI_MODELS` | Comma-separated model list for `/v1/models` (empty = live `opencode models` output) | - |
| `PROXY_POOL_SIZE` | Number of VPN containers | `3` |
| `PROXY_BASE_PORT` | Starting port for VPN containers | `10801` |
| `VPN_IMAGE` | Docker image for VPN containers (wireguard-tools + gost SOCKS5) | `ghcr.io/tprasadtp/protonwire:latest` |
| `PROTONVPN_USERNAME` | ProtonVPN username (OpenVPN/OpenVPN-IKEv2 credential from account dashboard) | - |
| `PROTONVPN_PASSWORD` | ProtonVPN password | - |
| `PROTONVPN_API_BASE` | ProtonVPN account API (SRP auth, certificates) | `https://account.protonvpn.com` |
| `PROTONVPN_VPN_API_BASE` | ProtonVPN VPN API (server list) | `https://vpn-api.proton.me` |
| `PROTONVPN_STORE_PATH` | SQLite path for credentials/sessions/certificates/server cache | `data/protonvpn.db` |
| `PROTONVPN_REGIONS` | Comma-separated country codes for server selection (e.g. `NL,US,JP,DE`) | `NL,US,JP,DE` |
| `PROTONVPN_IP_CHECK_URL` | URL to verify egress IP | `https://icanhazip.com/` |
| `RATE_LIMIT_COOLDOWN` | Cooldown duration after rate limit | `5m` |
| `RATE_LIMIT_RETRY_AFTER` | `Retry-After` seconds returned to clients when upstream keeps rate limiting across all retries (responds `429` instead of `502`) | `60` |
| `IP_BAN_DURATION` | How long a rate-limited egress IP/region is kept out of rotation | `10m` |
| `RATE_LIMIT_FRESH_IP_WAIT` | Per-request max wait for a fresh, unbanned proxy to boot after all pool IPs are rate limited | `90s` |
| `HEALTH_CHECK_PERIOD` | Health check interval | `30s` |
| `RESOURCE_CPU_LIMIT` | CPU limit per container | `0.25` |
| `RESOURCE_MEMORY_LIMIT` | Memory limit per container | `512M` (below ~256M can cause WireGuard OOM-kill, exit 137) |
| `MAX_RETRIES` | Maximum retry attempts | `3` |
| `RETRY_BASE_DELAY` | Base delay for exponential backoff | `1s` |
| `RETRY_MAX_DELAY` | Maximum delay between retries | `30s` |
| `MAX_CONCURRENT` | Maximum concurrent requests | `100` |
| `STICKY_SESSION_TTL` | Sticky session TTL | `10m` |
| `LOG_LEVEL` | Log level (debug, info, warn, error) | `info` |
| `LOG_FORMAT` | Log format (json, console) | `json` |
| `REQUEST_TIMEOUT` | Request timeout | `60s` |
| `METRICS_DB_PATH` | SQLite database path for request metrics | `data/metrics.db` |
| `MODEL_PRICING` | Per-1M-token pricing for cost estimation. Format: `model:input,output,cached;model2:input,output` (cached optional, defaults to input × 0.5). Rates in USD per 1M tokens | - |
| `MODEL_FILTER` | Keep only models whose name contains this substring in `/v1/models` (case-insensitive; empty disables) | `-free` |
| `CORS_ORIGIN` | Allowed CORS origin for API responses | `*` |

### Configuration File

You can also use a YAML configuration file by setting `CONFIG_FILE` environment variable:

```yaml
listen_addr: ":8082"
upstream_base_url: "https://opencode.ai/zen/v1"
upstream_api_key: "zent-your-api-key"
proxy_pool_size: 5
rate_limit_cooldown: "5m"
log_level: "info"
log_format: "console"
```

## Web UI

Open `http://localhost:8082` in your browser:

- **Dashboard** (`#/dashboard`): live metrics — total requests, success rate, avg latency, **total tokens** (prompt in / completion out), **estimated cost** (with cached-token count), streaming requests, errors, uptime, per-minute traffic chart, per-model usage with per-model token counts and cost, and proxy pool status showing each container's state and egress IP (duplicate IPs flagged). Auto-refreshes every 5 seconds.
- **Chat** (`#/chat`): a ChatGPT-like interface with model selection, SSE streaming responses, and per-conversation sticky sessions (`conversation_id` handled automatically).

> Token metrics require `MODEL_PRICING` to be set for cost estimation; token counts are always recorded. Unknown models estimate at $0.

## API Endpoints

### Chat Completions

```bash
POST /v1/chat/completions
```

Compatible with OpenAI's chat completion API.

**Example (non-streaming):**
```bash
curl http://localhost:8082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ]
  }'
```

**Example (streaming):**
```bash
curl http://localhost:8082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [
      {"role": "user", "content": "Write a short poem"}
    ],
    "stream": true
  }'
```

**Sticky Sessions:**

To enable sticky sessions (same conversation uses same IP), add a `conversation_id`:

```json
{
  "model": "deepseek-v4-flash",
  "messages": [...],
  "conversation_id": "unique-conversation-id"
}
```

### List Models

```bash
GET /v1/models
```

Returns the model list from the configured upstream (proxied through the pool), so the chat UI always shows the real, current models from your provider. By default only models with `-free` in the name are returned — configure with `MODEL_FILTER` (e.g. `MODEL_FILTER=` to disable, or `MODEL_FILTER=zen` to match differently).

```bash
curl http://localhost:8082/v1/models
```

> Switching providers is a one-line change: set `UPSTREAM_BASE_URL=https://opencode.ai/zen/go/v1` for the cheaper Zen Go tier. The gateway auto-appends `/v1` when the base URL doesn't include it, so both styles work.

### Health Check

```bash
GET /health
```

Returns health status of the gateway and proxy pool.

```bash
curl http://localhost:8082/health
```

### Pool Statistics

```bash
curl http://localhost:8082/stats
```

Returns statistics about the proxy pool.

### Metrics

```bash
curl http://localhost:8082/api/metrics
```

Returns the dashboard payload: aggregated totals (requests, errors, success rate, avg latency, uptime, total/prompt/completion/cached tokens, estimated cost), per-minute traffic series, per-model usage with token counts and cost, pool statistics, and per-proxy details. Persisted in SQLite (see `METRICS_DB_PATH`), pruned after 7 days.

```bash
curl http://localhost:8082/metrics
```

Prometheus exposition format: request counters by model/status, token counters (prompt/completion/total), latency histograms, and pool gauges.

```bash
curl http://localhost:8082
```

Serves the embedded web UI (dashboard + chat).

## Docker Compose Deployment

```bash
# Set your ProtonVPN credentials in .env first:
#   PROTONVPN_USERNAME=...
#   PROTONVPN_PASSWORD=...

# Start the gateway
docker compose up -d --build
```

This will start the API gateway. The gateway manages its own ProtonVPN containers at runtime through the Docker socket (`PROXY_POOL_SIZE` of them), so only the gateway service is defined in the compose file.

Notes:

- **Custom port**: set `GATEWAY_PORT` in your `.env` (or inline) to change the host port, e.g. `GATEWAY_PORT=9090 docker compose up -d`. The gateway uses host networking so the UI + API are served directly on that port.
- The compose file auto-loads `.env` for all `UPSTREAM_*`, `PROXY_*`, `RESOURCE_*`, `LOG_*`, etc. variables (see `.env.example`).
- Metrics persist in a named Docker volume (`gateway-data`) mounted at `/app/data`.
- A healthcheck validates `/api/metrics`; view status with `docker compose ps`.

```bash
# Example: run on a custom port with more proxies
GATEWAY_PORT=9090 PROXY_POOL_SIZE=5 UPSTREAM_API_KEY=sk-... docker compose up -d --build
```

## Agent Mode (OpenCode Server)

Set `UPSTREAM_PROVIDER=opencode` to route requests through an [OpenCode Server](https://opencode.ai/docs/server) (`opencode serve`) instead of Zen. The gateway translates each OpenAI-compatible `/v1/chat/completions` call into OpenCode's `POST /session` → `POST /session/:id/message` flow (see `internal/upstream/opencode_client.go`), and still hands the request to **a fresh per-container VPN proxy** — so the agent's network egress comes from a unique IP on every request (and is rotated/banned automatically on 429s, like the Zen path).

- **Auth**: if `OPENCODE_SERVER_PASSWORD` is set, the gateway authenticates to the server with HTTP basic auth. Otherwise it PUTs the caller's `Authorization: Bearer <key>` to `/auth/<OPENCODE_PROVIDER_ID>` (or uses the configured provider id) before messaging.
- **Streaming**: when `stream:true`, the assistant text is relayed as OpenAI `data:` SSE chunks (one per word) with `data: [DONE]`; non-streaming returns a single OpenAI `chat.completion`.
- **Sessions**: one OpenCode session is created per request. Multi-message continuity within a single conversation is a follow-up (sticky session ids would key a persistent session).

```bash
# Run your own OpenCode Server, then have the gateway egress through ProtonVPN
opencode serve --port 4121 --hostname 0.0.0.0 &
UPSTREAM_PROVIDER=opencode OPENCODE_SERVER_URL=http://<reachable-host>:4121 \
  OPENCODE_PROVIDER_ID=openai OPENCODE_MODEL=gpt-5.1-codex \
  docker compose up -d --build
```

## Agent Mode (opencode CLI in containers)

Set `UPSTREAM_PROVIDER=opencode-cli` to run the **actual opencode CLI** inside every VPN container. At startup the gateway builds `opencode-multi-agents/protonvpn:latest` (base protonwire image + gost SOCKS5 proxy), and each `/v1/chat/completions` request is `docker exec`'d as `opencode run --format json` **inside a container** — the agent's tool use, browsing and API calls all egress from that container's unique VPN IP. Thinking/reasoning is relayed and rendered in the chat UI.

- **Auth**: this mode calls real provider APIs, so the client must send a real credential (`Authorization: Bearer sk-ant-...`). The gateway strips `Bearer ` and injects it as `OPENCODE_CLI_PROVIDER_ENV` (default `ANTHROPIC_API_KEY`; set `OPENAI_API_KEY`/`GEMINI_API_KEY` etc. to match your provider) on every exec.
- **Models**: `/v1/models` runs `opencode models` inside a container; override with `OPENCODE_CLI_MODELS=provider:model,provider:model`.
- **Streaming**: `opencode run` NDJSON events are relayed as OpenAI SSE (`reasoning` → `delta.reasoning_content`, `text` → `delta.content`, `error` events → error response). If no text event arrives (a known opencode stdout-flush bug), the raw stdout is relayed as plain text.
- **Sessions**: `conversation_id` maps to a persistent opencode session (`--session`) for multi-turn continuity.
- **Rate limits**: upstream throttling wording in the CLI output bans the container's egress IP and rotates in a fresh container, same as Zen mode.

```bash
# e.g. Anthropic provider; every exec injects ANTHROPIC_API_KEY=sk-ant-...
UPSTREAM_PROVIDER=opencode-cli OPENCODE_CLI_PROVIDER_ENV=ANTHROPIC_API_KEY \
  OPENCODE_CLI_MODEL=claude-sonnet-4-20250514 \
  docker compose up -d --build

curl -s http://localhost:8082/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer $ANTHROPIC_API_KEY" \
  -d '{"model":"claude-sonnet-4","messages":[{"role":"user","content":"what is my egress IP?"}],"stream":true}'
```

## IP Diversity & Rotation

New containers are spread across **different ProtonVPN servers** within the configured regions, so pool proxies get exit IPs from different subnets. When the upstream returns a rate limit (429), the gateway:

1. Bans the responsible egress IP and region for `max(IP_BAN_DURATION, upstream Retry-After)`
2. Spins up a replacement container on a **different server** (already-used servers are avoided when selecting)
3. Different server = different egress IP subnet

Check that all containers are reachable:

```bash
make check-ips
# or: PROXY_BASE_PORT=10801 PROXY_POOL_SIZE=3 bash scripts/check-ips.sh
# port 10801: ip=185.107.56.50
# port 10802: ip=195.242.214.98
# ...
# OK: all proxies are reachable
```

The script probes `icanhazip.com` through each container's SOCKS5 port and exits non-zero on any unreachable container.

## Architecture

```
Client → API Gateway → Proxy Pool → VPN Containers → Upstream Provider
                    ↓
             Rate Limit Detection
                    ↓
             Rotate to Different Region
```

### Components

1. **HTTP Server**: OpenAI-compatible API endpoints
2. **Proxy Pool Manager**: Manages lifecycle of VPN containers
3. **Docker Manager**: Creates and monitors VPN containers
4. **Upstream Client**: Forwards requests through SOCKS5 proxies
5. **Rate Limit Detector**: Detects and handles rate limiting
6. **Health Monitor**: Background health checks

### Rate Limit Handling

When the upstream returns a rate-limit response (HTTP 429 or a rate-limit error body, including when its `Retry-After` header is honored):

1. The egress IP and **region** of the responsible proxy are **banned** for `max(IP_BAN_DURATION, upstream Retry-After)` — no request is routed through it and new containers use a different region.
2. The proxy is moved to *cooldown*; a **replacement container from a different region** is spun up asynchronously.
3. The gateway waits **up to `RATE_LIMIT_FRESH_IP_WAIT`** for that fresh proxy to boot. If one appears, the request succeeds through the new IP instead of erroring.
4. Only if no unbanned proxy materializes in time does the gateway answer honestly: `429 Too Many Requests` with `Retry-After` (from `RATE_LIMIT_RETRY_AFTER`) — never a misleading `502`.

This rotates regions on per-IP limits. If the throttle is **account/key-based** (e.g. Zen's shared `public` tier), region rotation cannot dodge it — use `UPSTREAM_API_KEYS` to spread quota across multiple accounts.

## Development

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run Locally

```bash
make run
```

### Clean

```bash
make clean
```

## Performance Tuning

### For High Traffic

```bash
PROXY_POOL_SIZE=10
MAX_CONCURRENT=200
RESOURCE_CPU_LIMIT=0.5
RESOURCE_MEMORY_LIMIT=128M
```

### For Low Latency

```bash
RETRY_BASE_DELAY=500ms
RETRY_MAX_DELAY=5s
REQUEST_TIMEOUT=30s
```

### For Memory Constrained Environments

```bash
PROXY_POOL_SIZE=2
RESOURCE_MEMORY_LIMIT=256M
MAX_CONCURRENT=50
```

> `RESOURCE_MEMORY_LIMIT` below ~256M can cause the VPN container to be OOM-killed (exit 137).

## Supported Upstream Providers

- **OpenCode Zen** — `https://opencode.ai/zen/v1` (default; API key `zent-...` from https://opencode.ai/zen)
- **OpenCode Zen Go** — `https://opencode.ai/zen/go/v1` (cheaper tier, fewer models)
- Any OpenAI-compatible API endpoint

## Troubleshooting

### Containers Not Starting

Check Docker logs:
```bash
docker ps -a | grep protonvpn-gateway
docker logs <container-id>
```

### Health Checks Failing

The gateway checks VPN status via `icanhazip.com`. Ensure containers can reach the internet and your ProtonVPN key is valid.

### High Latency

- Increase `PROXY_POOL_SIZE` to have more proxies available
- Reduce `RATE_LIMIT_COOLDOWN` if you're being too cautious
- Check Docker resource limits

### Memory Issues

- Reduce `PROXY_POOL_SIZE`
- Lower `MAX_CONCURRENT`
- Set stricter `RESOURCE_MEMORY_LIMIT` per container

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## Acknowledgments

- [tprasadtp/protonwire](https://github.com/tprasadtp/protonvpn-docker) - ProtonVPN WireGuard Docker image
- [go-gost/gost](https://github.com/go-gost/gost) - SOCKS5 proxy used as sidecar
- OpenCode Zen - OpenAI-compatible API used as the default upstream
