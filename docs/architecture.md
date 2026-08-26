# Architecture

Technical deep dive: components, request lifecycle, container lifecycle, data model, and concurrency model.

## High-Level View

```
                        ┌──────────────────────────────────────────────────┐
                        │                Gateway Process                   │
                        │              (single Go binary)                  │
                        │                                                  │
 Client ──HTTP─────────▶│  handler ──▶ upstream client ──▶ proxy pool      │
 (OpenAI API)           │     │                            │               │
                        │     │                     SOCKS5 :1080x          │
 Dashboard ◀─HTTP──────▶│  web/metrics                     │               │
 Prometheus ◀─HTTP─────▶│                                  ▼               │
                        │         ┌──────────────────────────────┐         │
                        │         │ VPN container (per proxy)    │         │
                        │         │  gost (SOCKS5) → wg0 tunnel  │         │
                        │         └──────────────┬───────────────┘         │
                        └────────────────────────┼─────────────────────────┘
                                                 │ WireGuard (UDP 51820)
                                                 ▼
                                          ProtonVPN server
                                                 │
                                                 ▼
                                        Upstream AI provider
```

## Package Layout

| Package | Responsibility |
|---|---|
| `cmd/gateway` | Entry point: config load, logger, store init, pool start, HTTP server, graceful shutdown |
| `cmd/powsolver` | Native multi-core BLAKE3 CLI client for the PoW key gate |
| `internal/config` | Env + YAML config, defaults, validation, ConfigStore (SQLite `data/config.db` for accounts, proxies, settings) |
| `internal/handler` | HTTP routing (`/v1/*`, `/api/pow/*`, `/api/metrics`, ...), auth middleware (env ∪ issued keys + per-key limits), web-search tool rounds |
| `internal/upstream` | Provider drivers (`zen`, `opencode`, `opencode-cli`), SOCKS5 HTTP transport, retry logic |
| `internal/proxy` | Pool manager (state machine, region/IP bans, round-robin dispatch, duplicate-IP rotation), Docker manager (container lifecycle) |
| `internal/protonvpn` | SRP auth, certificate issuance, server selection, SQLite persistence |
| `internal/pow` | Hashcash-style challenge/verify domain, adaptive difficulty controller, key/challenge SQLite store (`data/pow.db`) |
| `internal/websearch` | Built-in web_search tool: providers (DuckDuckGo/SearXNG/Brave), page fetcher, result formatting |
| `internal/metrics` | SQLite metrics store (chat completions + endpoint traffic), token pricing, Prometheus exporter, system-info collector |
| `internal/web` | Embedded UI (`go:embed`): dashboard, chat, PoW solver view |

## Request Lifecycle (zen driver)

1. **Gateway auth** (middleware): bearer token resolved against env keys (`GATEWAY_API_KEYS`) and PoW-issued keys (in-memory cache, SQLite fallback). Issued keys additionally pass burst-cooldown and plan-RPM checks. Gateway-local credentials are then **stripped** — upstream auth is chosen separately by `resolveAuth` (client provider keys pass through; otherwise configured key / random `UPSTREAM_API_KEYS` pick / public tier).
2. **Receive & validate** — `POST /v1/chat/completions` arrives; request ID assigned; body capped at 10MB; OpenAI-standard validation (`model`, messages shape).
3. **Tool injection** — when the web_search tool is enabled, the function definition is added to the request's tools array (client-defined tools win).
4. **Proxy acquisition** — pool manager picks a proxy via round-robin:
   - Sticky session hit? Use that proxy if healthy.
   - Otherwise: least-recently-used **Idle** proxy.
   - All busy/banned? Wait for a free one or for a replacement container to boot (bounded by `RATE_LIMIT_FRESH_IP_WAIT` when everything is banned).
5. **Forward** — the upstream client dials the provider through `socks5://127.0.0.1:<port>` with exponential-backoff retries (`MAX_RETRIES`, `RETRY_BASE_DELAY`→`RETRY_MAX_DELAY`).
6. **Rate-limit check** — on 429 or keyword match: ban IP/region, move proxy to cooldown, spawn replacement, transparently retry through a fresh IP (see business-logic.md).
7. **Web-search rounds** — if the model called only the gateway's injected `web_search` tool, the gateway executes it through the same proxy's SOCKS5 tunnel, replays assistant+tool messages, and requests another round (up to `WEB_SEARCH_MAX_ROUNDS`). Streaming clients see one seamless stream; intercepted chunks never surface.
8. **Token accounting** — usage extracted from the response (aggregated across search rounds); cost estimated via pricing table.
9. **Traffic accounting** — the outer logging middleware records every request to `endpoint_traffic` (route template, status, latency, bytes out) regardless of outcome.
10. **Persist & release** — metrics rows written to SQLite + Prometheus counters bumped; proxy released back to Idle.

## Container Lifecycle

**Creation** (`internal/proxy/docker.go: CreateEx`):

1. Ensure the VPN image exists locally (built on first run: protonwire base + wireguard-tools + iptables + gost).
2. Select a ProtonVPN logical server — online only (`Status == 0`), avoiding banned regions and already-used servers.
3. Obtain a certificate bound to that server (peerName/peerIp/peerPublicKey) using the stored Ed25519 key.
4. Derive the X25519 private key from Ed25519 (SHA-512 clamping).
5. Build wg0.conf: endpoint = `cert.Features.PeerIP:51820`, address = `10.2.0.2/32` fallback, DNS = `10.2.0.1`.
6. Create + start the container: privileged, `NET_ADMIN`, sysctls (`rp_filter=2`, `src_valid_mark=1`, IPv6 off), CPU/memory limits, SOCKS5 port bound to `127.0.0.1`.
7. Entrypoint runs `wg-quick up wg0`, waits for egress connectivity, then serves gost on :1080.

**Health checking** — every `HEALTH_CHECK_PERIOD`: curl icanhazip.com through each SOCKS5 port. Failures increment error counts; repeated failures mark the proxy unhealthy → removed and replaced.

**Removal** — explicit remove, unhealthy eviction, or shutdown cleanup; orphaned containers labeled `protonvpn-gateway=true` are reaped at startup.

## Data Model

### SQLite: metrics.db (`internal/metrics`)

```
requests(id, timestamp, model, stream, success, status,
         latency_ms, prompt_tokens, completion_tokens,
         total_tokens, cached_tokens, estimated_cost)
schema_version(version)
```

- Indexed by timestamp and model; pruned after 7 days.
- Versioned migrations (v1 schema, v2 token/cost columns).
- Snapshot API aggregates: summary totals, per-minute traffic series (zero-filled buckets), per-model usage.

### SQLite: pow.db (`internal/pow`)

```
pow_challenges(id, bind, plan, algo, difficulty, salt,
               issued_at, expires_at, used)
api_keys(key_hash, prefix, plan, rpm, created_at, expires_at, disabled)
pow_ip_bonus(ip_hash, bonus_bits, updated_at)   -- farming penalty
```

Challenges are single-use (atomic `used=0→1` consume), TTL'd, and pruned periodically. Keys are stored **hashed only**; the plaintext is shown once at issuance.

### SQLite: metrics.db — endpoint traffic (`internal/metrics`, migration v3)

```
endpoint_traffic(id, ts, method, route, status, duration_ms, bytes_out)
```

One row per server request (all routes; infra self-polling like `/metrics` and `/health` excluded). Aggregated into per-minute traffic series and top-endpoint tables for `/api/metrics`; pruned after 7 days.

### SQLite: protonvpn.db (`internal/protonvpn`)

```
credentials    — username/password
session        — UID, cookies, expiry
certificates   — cert, keys, expiry
server_cache   — fetched logical-server list
wireguard_keys — derived X25519 keypair
ed25519_keys   — certificate key pair
```

### SQLite: config.db (`internal/config`)

```
accounts(id, username, password, store_path, session_cookies, enabled, created_at)
proxies(id, address, enabled, created_at)
settings(key TEXT PRIMARY KEY, value TEXT)
```

- `accounts`: ProtonVPN credentials managed via the Pool Manager UI (`#/manage`) or seeded from `.env` on first boot.
- `proxies`: external SOCKS5 proxy addresses managed via UI.
- `settings`: all `.env` configuration as key-value pairs. Seeded from `.env` on first boot; after that the ConfigStore is authoritative. Hot-reloadable fields are applied to the live config pointer immediately on save.

## Concurrency Model

- **Pool state**: guarded by a single RWMutex; counters are atomic. Proxy snapshots for the dashboard are taken under read lock.
- **Metrics store**: mutex-serialized writes; WAL-mode SQLite with busy timeout.
- **Certificate issuance**: process-wide mutex — concurrent requests to ProtonVPN's cert API trigger key conflicts (409/code 2500).
- **Region/server bans**: maps under the pool lock; expired entries are lazily cleaned during lookups and periodic pruning.
- **HTTP clients**: cached per-proxy transports (connection reuse); global limits via `MAX_CONCURRENT`.
- **PoW service**: one mutex guards key cache + rate-limit maps; verification itself is stateless (single hash). Issued-key auth lookups are cache-first with a SQLite fallback.
- **Web-search rounds**: executed inline within the request goroutine; tool-call chunk buffering is per-request.

## Key Interfaces

```go
// internal/proxy/types.go
type Manager interface {
    Create(ctx) (*Proxy, error)
    CreateEx(ctx, bannedRegions, avoidServers map[string]bool) (*Proxy, error)
    Remove(ctx, id string) error
    HealthCheck(ctx, p *Proxy) (bool, error)
    Close() error
}
```

The pool depends only on this interface, which keeps pool logic unit-testable with stub managers (`types_test.go`, `handler_test.go`).

---

Related: [protonvpn-integration.md](protonvpn-integration.md), [business-logic.md](business-logic.md).
