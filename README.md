# OpenAI-Compatible API Gateway with IP Rotation

Lightweight and performant API gateway that provides OpenAI-compatible endpoints with automatic IP rotation using Cloudflare WARP containers.

## Features

- **OpenAI-Compatible API**: Drop-in replacement for OpenAI API clients
- **Automatic IP Rotation**: Rotates through multiple Cloudflare WARP containers
- **Rate Limit Detection**: Automatically detects and handles rate limits (429 errors)
- **Streaming Support**: Full support for Server-Sent Events (SSE) streaming
- **Retry Logic**: Exponential backoff with configurable retries
- **Sticky Sessions**: Optional conversation-based session persistence
- **Health Monitoring**: Background health checks for all proxies
- **Resource Efficient**: Minimal memory footprint, fast startup

## Quick Start

### Prerequisites

- Go 1.20+ (for building from source)
- Docker (for running WARP containers)
- Docker Compose (optional, for easy deployment)

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

4. Run the gateway:
```bash
./bin/gateway
```

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `LISTEN_ADDR` | Server listen address | `:8080` |
| `UPSTREAM_BASE_URL` | Upstream API base URL | `https://openrouter.ai/api` |
| `UPSTREAM_API_KEY` | Upstream API key | - |
| `PROXY_POOL_SIZE` | Number of WARP containers | `3` |
| `PROXY_BASE_PORT` | Starting port for WARP containers | `10801` |
| `WARP_IMAGE` | Docker image for WARP | `caomingjun/warp:latest` |
| `RATE_LIMIT_COOLDOWN` | Cooldown duration after rate limit | `5m` |
| `HEALTH_CHECK_PERIOD` | Health check interval | `30s` |
| `RESOURCE_CPU_LIMIT` | CPU limit per container | `0.25` |
| `RESOURCE_MEMORY_LIMIT` | Memory limit per container | `64M` |
| `MAX_RETRIES` | Maximum retry attempts | `3` |
| `RETRY_BASE_DELAY` | Base delay for exponential backoff | `1s` |
| `RETRY_MAX_DELAY` | Maximum delay between retries | `30s` |
| `MAX_CONCURRENT` | Maximum concurrent requests | `100` |
| `STICKY_SESSION_TTL` | Sticky session TTL | `10m` |
| `LOG_LEVEL` | Log level (debug, info, warn, error) | `info` |
| `LOG_FORMAT` | Log format (json, console) | `json` |
| `REQUEST_TIMEOUT` | Request timeout | `60s` |

### Configuration File

You can also use a YAML configuration file by setting `CONFIG_FILE` environment variable:

```yaml
listen_addr: ":8080"
upstream_base_url: "https://openrouter.ai/api"
upstream_api_key: "your-api-key"
proxy_pool_size: 5
rate_limit_cooldown: "5m"
log_level: "info"
log_format: "console"
```

## API Endpoints

### Chat Completions

```bash
POST /v1/chat/completions
```

Compatible with OpenAI's chat completion API.

**Example (non-streaming):**
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "meta-llama/llama-3.1-8b-instruct:free",
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ]
  }'
```

**Example (streaming):**
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "meta-llama/llama-3.1-8b-instruct:free",
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
  "model": "meta-llama/llama-3.1-8b-instruct:free",
  "messages": [...],
  "conversation_id": "unique-conversation-id"
}
```

### List Models

```bash
GET /v1/models
```

Returns a list of available models.

```bash
curl http://localhost:8080/v1/models
```

### Health Check

```bash
GET /health
```

Returns health status of the gateway and proxy pool.

```bash
curl http://localhost:8080/health
```

### Pool Statistics

```bash
GET /stats
```

Returns statistics about the proxy pool.

```bash
curl http://localhost:8080/stats
```

## Docker Compose Deployment

```bash
docker-compose up -d
```

This will start:
- The API gateway
- 3 Cloudflare WARP containers (configurable)

## Architecture

```
Client → API Gateway → Proxy Pool → WARP Containers → Upstream Provider
                    ↓
             Rate Limit Detection
                    ↓
             Automatic Retry with New IP
```

### Components

1. **HTTP Server**: OpenAI-compatible API endpoints
2. **Proxy Pool Manager**: Manages lifecycle of WARP containers
3. **Docker Manager**: Creates and monitors WARP containers
4. **Upstream Client**: Forwards requests through SOCKS5 proxies
5. **Rate Limit Detector**: Detects and handles rate limiting
6. **Health Monitor**: Background health checks

### Rate Limit Handling

When a rate limit is detected:
1. The current proxy is marked as "cooldown"
2. A new proxy is acquired from the pool
3. The request is retried with exponential backoff
4. After cooldown period, the proxy returns to active pool

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
RESOURCE_MEMORY_LIMIT=32M
MAX_CONCURRENT=50
```

## Supported Upstream Providers

- OpenRouter (free models)
- Groq
- Together AI
- Fireworks AI
- SambaNova
- Any OpenAI-compatible API endpoint

## Troubleshooting

### Containers Not Starting

Check Docker logs:
```bash
docker ps -a | grep warp-gateway
docker logs <container-id>
```

### Health Checks Failing

The gateway checks WARP status via `cloudflare.com/cdn-cgi/trace`. Ensure containers can reach the internet.

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

- [caomingjun/warp](https://hub.docker.com/r/caomingjun/warp) - Cloudflare WARP Docker image
- OpenRouter, Groq, and other free model providers
