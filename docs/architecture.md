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
| `internal/config` | Env + YAML config, defaults, validation |
| `internal/handler` | HTTP routing: `/v1/*`, `/api/metrics`, `/metrics`, `/health`, `/stats`, embedded UI |
| `internal/upstream` | Provider drivers (`zen`, `opencode`, `opencode-cli`), SOCKS5 HTTP transport, retry logic |
| `internal/proxy` | Pool manager (state machine, bans, used-server tracking), Docker manager (container lifecycle), Manager interface |
| `internal/protonvpn` | SRP auth, certificate issuance, server selection, SQLite persistence |
| `internal/metrics` | SQLite metrics store, token extraction support, pricing table, Prometheus exporter |
| `internal/web` | Embedded dashboard/chat UI (`go:embed`) |

## Request Lifecycle (zen driver)

1. **Receive & validate** — `POST /v1/chat/completions` arrives; request ID assigned; body capped at 10MB.
2. **Proxy acquisition** — pool manager picks a proxy:
   - Sticky session hit? Use that proxy if healthy.
   - Otherwise: least-recently-used **Idle** proxy.
   - All busy/banned? Wait for a free one or for a replacement container to boot (bounded by `RATE_LIMIT_FRESH_IP_WAIT` when everything is banned).
3. **Forward** — the upstream client dials the provider through `socks5://127.0.0.1:<port>` with exponential-backoff retries (`MAX_RETRIES`, `RETRY_BASE_DELAY`→`RETRY_MAX_DELAY`).
4. **Rate-limit check** — on 429 or keyword match: ban IP/region, move proxy to cooldown, spawn replacement, transparently retry through a fresh IP (see business-logic.md).
5. **Token accounting** — usage is extracted from the response (streaming: from final SSE chunk / non-streaming: from JSON body); cost estimated via pricing table.
6. **Persist & release** — metrics row written to SQLite + Prometheus counters bumped; proxy released back to Idle.

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

### SQLite: protonvpn.db (`internal/protonvpn`)

```
credentials    — username/password
session        — UID, cookies, expiry
certificates   — cert, keys, expiry
server_cache   — fetched logical-server list
wireguard_keys — derived X25519 keypair
ed25519_keys   — certificate key pair
```

## Concurrency Model

- **Pool state**: guarded by a single RWMutex; counters are atomic. Proxy snapshots for the dashboard are taken under read lock.
- **Metrics store**: mutex-serialized writes; WAL-mode SQLite with busy timeout.
- **Certificate issuance**: process-wide mutex — concurrent requests to ProtonVPN's cert API trigger key conflicts (409/code 2500).
- **Region/server bans**: maps under the pool lock; expired entries are lazily cleaned during lookups and periodic pruning.
- **HTTP clients**: cached per-proxy transports (connection reuse); global limits via `MAX_CONCURRENT`.

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
