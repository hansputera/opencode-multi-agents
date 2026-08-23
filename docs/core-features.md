# Core Features

In-depth explanation of every feature: what it does, why it exists, and how it behaves at runtime.

---

## 1. OpenAI-Compatible API

**What**: The gateway exposes `POST /v1/chat/completions` and `GET /v1/models` with request/response shapes identical to the OpenAI API.

**Why**: Any existing tool that can call OpenAI (chat UIs, IDE extensions, agents, SDKs) works without code changes — you only change the base URL and (optionally) the API key.

**Runtime behavior**:

- Requests are validated, then forwarded through a proxy from the pool.
- Streaming (`"stream": true`) uses Server-Sent Events; chunks are relayed as they arrive from upstream.
- An optional `conversation_id` field (non-standard) enables sticky sessions.
- Errors are returned in OpenAI's error envelope (`{"error": {"message", "type", "code"}}`) with sanitized messages — internal details never leak to clients.

## 2. Per-Request VPN IP (IP Rotation)

**What**: Every proxy container runs its own ProtonVPN WireGuard tunnel. New containers are deliberately spread across **different logical servers**, so pool proxies have exit IPs in different subnets.

**Why**: Upstream providers throttle per-IP. If all your traffic comes from one IP, one 429 stops everything. With N containers on N servers, a rate limit on one IP doesn't touch the others — and the gateway actively rotates when limits do hit.

**Runtime behavior**:

- The pool manager tracks "used servers"; when creating a replacement container it passes this set as an avoid-list to server selection.
- If every server in scope is already used, selection falls back to any online server (never fails just because of the avoid-list).
- **Duplicate IPs are rotated away**: a fresh proxy whose egress IP matches another live pool proxy is automatically replaced — up to `PROXY_IP_ROTATE_ATTEMPTS` (default 3) times — until it gets a unique IP. One duplicate is kept only when the free-tier IP pool is exhausted. Health-check drift (a tunnel reconnecting onto an existing IP) is detected and rotated as well.
- Incoming requests are **load-balanced round-robin** across available, unbanned containers; sticky sessions still take precedence.
- Each proxy card on the dashboard shows its egress IP; duplicate IPs are flagged in red (only possible after rotation attempts are exhausted).

## 3. Rate Limit Detection & Transparent Recovery

**What**: The gateway detects throttling two ways:

1. HTTP status `429`
2. Rate-limit keywords in the response body ("rate limit", "quota exceeded", ...) even when wrapped in other status codes

**Why**: Providers don't always signal limits cleanly. Missing keyword-based detection means clients receive confusing errors mid-conversation.

**Recovery sequence** (full rules in [business-logic.md](business-logic.md)):

1. The responsible egress IP **and** region are banned for `max(IP_BAN_DURATION, upstream Retry-After)`.
2. The proxy enters *cooldown*; a replacement container boots asynchronously on a different server/region.
3. The request waits up to `RATE_LIMIT_FRESH_IP_WAIT` for the fresh proxy — if it appears, the request succeeds through the new IP and the client never sees a 429.
4. Only if nothing materializes in time does the gateway answer `429 Too Many Requests` with a `Retry-After` header — never a misleading `502`.

## 4. Native ProtonVPN Integration

**What**: The gateway authenticates to ProtonVPN exactly like their web app: SRP (Secure Remote Password) login with username/password, then requests VPN certificates and builds WireGuard tunnels automatically.

**Why**: Zero manual key management. Add credentials to `.env`, and certificates, keys, and server lists are obtained, refreshed, cached, and persisted to SQLite automatically.

**Highlights**:

- Full SRP flow via `ProtonMail/go-srp`; session cookies merged correctly (the auth step replaces the bootstrap AUTH cookie).
- Ed25519 certificate key pairs are generated locally; the WireGuard X25519 key is *derived* from the Ed25519 key — ProtonVPN requires this mapping or traffic is silently dropped.
- Certificates are requested per-server (peerName/peerIp bound), serialized under a mutex to avoid key-conflict churn.
- Everything persists in `PROTONVPN_STORE_PATH` (SQLite): credentials, sessions, certs, server cache, keys — restarts don't re-authenticate unnecessarily.

Full detail: [protonvpn-integration.md](protonvpn-integration.md).

## 5. Proxy Pool & Self-Healing

**What**: A pool of `PROXY_POOL_SIZE` containers managed as a state machine:

```
Idle ──(request assigned)──▶ Active ──(released)──▶ Idle
  ▲                            │
  │                     (429 detected)
  │                            ▼
Unhealthy ◀──(failed check)── Cooldown ──(ban expires)──▶ back to rotation
```

- **Idle**: healthy, ready for traffic
- **Active**: currently serving a request
- **Cooldown**: banned due to rate limit; excluded from routing
- **Unhealthy**: failed health checks; removed and replaced

**Self-healing**: background health checks run every `HEALTH_CHECK_PERIOD`, probing `icanhazip.com` through each container's SOCKS5 port. Dead/unhealthy proxies are removed and replaced automatically. On startup, orphaned containers from a previous run are cleaned up.

## 6. Sticky Sessions

**What**: Sending `"conversation_id": "<id>"` pins that conversation to one proxy for `STICKY_SESSION_TTL`.

**Why**: Some providers behave better when consecutive turns of a conversation share an IP; it also avoids mid-conversation IP switches.

**Runtime behavior**: In-memory map `conversation_id → proxy_id`, refreshed on each use, expired after TTL. Cleared on restart (documented trade-off; Redis would be needed for multi-instance).

## 7. Token Metrics & Cost Estimation

**What**: Every request's token usage is extracted (from both streaming and non-streaming responses) and persisted:

- prompt / completion / total / cached tokens
- estimated cost per model using configurable pricing

**Why**: Usage across many free-tier models is easy to lose track of. This gives spend visibility even though the gateway itself is not a billing system.

**Configuration** (`MODEL_PRICING`):

```
MODEL_PRICING=model-a:input,output,cached;model-b:input,output
```

Rates are USD per 1M tokens. Cached defaults to input x 0.5 if omitted; unknown models estimate at $0.

**Where you see it**:

- Dashboard stat cards: Total Tokens (in/out breakdown), Est. Cost (+cached count)
- Model usage panel: per-model prompt/completion/total tokens + cost
- `/api/metrics` JSON and Prometheus counters at `/metrics`

## 8. PoW-Gated Free API Keys

**What**: With `POW_ENABLED=true`, `/v1/*` requires an API key — either from `GATEWAY_API_KEYS` or a free key **earned by solving a proof-of-work puzzle** (Hashcash-style; no blockchain, no signup).

**Why**: The service is free but not free-to-abuse. PoW makes every key cost real compute: casual users pay one minute, abusers paying for thousands of keys pay thousands of minutes.

**Plans** (harder puzzle → higher limit):

| Plan | Extra bits | Rate limit |
|---|---|---|
| `basic` | +0 | 100 req/min |
| `plus` | +4 (~16×) | 250 req/min |
| `pro` | +8 (~256×) | 500 req/min |

**Runtime behavior**: challenges are single-use, expire in 10 minutes, and are bound to the requester's IP+fingerprint. Difficulty adapts automatically to keep honest solves in the 30–90 s window. Keys last 7 days and are stored hashed. Bursting >5 req/s cools a key down for 5 minutes.

Full protocol and solver details: [pow-api-keys.md](pow-api-keys.md).

## 9. Built-in Web Search

**What**: the gateway injects a `web_search(query)` function into chat requests (zen driver). When the model calls it, the gateway searches the web server-side — through the same rotating VPN IP as model traffic — extracts readable text from top pages, and feeds results back for another round.

**Why**: models without live data can answer current-events questions anyway; clients just see a normal streaming response.

**Runtime behavior**:
- Works identically for streaming and non-streaming requests; intercepted tool-call chunks never reach the client
- Providers: SearXNG (`SEARXNG_URL`), Brave (`BRAVE_API_KEY`), or keyless DuckDuckGo fallback
- Client-defined tools always win: your own `web_search` suppresses injection; mixed turns are relayed untouched
- Mid-loop VPN tunnel failures retry on fresh proxies automatically


## 10. OpenAI-Standard API Compliance

**What**: the API surface follows OpenAI's published behavior precisely, so stock SDKs work unchanged.

- **Error envelope**: `{"error": {"message", "type", "param", "code"}}` with correct types (`invalid_request_error`, `authentication_error`, `rate_limit_error`, ...) and `param` naming the offending field (e.g. `"messages[0].role"`)
- **Validation**: required `model`, role/content checks per message, JSON content-type enforcement (415)
- **Routing**: unknown `/v1/*` paths → JSON 404 (`unknown_url`); wrong methods → JSON 405 with `Allow`; `GET /v1/models/{id}` retrieves one model or 404 `model_not_found`
- **Streaming**: relays `reasoning_content` deltas, honors `stream_options.include_usage` (synthesizes a terminal usage chunk when possible), emits an in-band SSE error event if the upstream breaks mid-stream

## 11. Server Usage Metrics & System Specs

**What**: every request to any endpoint is recorded — route, status, latency, bytes served — and aggregated server-wide.

**Why**: "how is my free service being used" needs the whole picture, not just chat completions.

**Where you see it**:
- Dashboard stat cards: **All Server Requests**, **Data Served** (bytes)
- **Traffic by endpoint** panel: top routes over 24h with request bars, error %, avg latency
- **Server specifications** panel: CPU model/cores, OS/arch + Go version, host memory used/total, load averages, host/process uptime, process RSS + goroutines, disk free on the data volume
- The per-minute traffic chart shows ALL requests; infrastructure self-polling (healthchecks, Prometheus scrapes, dashboard refresh) is excluded
- Prometheus: `opencode_endpoint_requests_total{route,status}`, `opencode_bytes_served_total{route}`


## 12. Web Dashboard & Chat UI

**Dashboard** (`#/dashboard`): neobrutalism-styled, auto-refreshes every 5s:

- Stat cards: requests, success rate, latency, tokens, cost, all server requests, data served, streaming, errors, uptime
- Per-minute traffic chart covering ALL server requests (zero-filled buckets)
- Model usage panel with token/cost rows per model
- **Traffic by endpoint** panel: per-route request bars with error % and avg latency (last 24h)
- **Server specifications** panel: CPU model/cores, memory, disk, load averages, uptimes, goroutines
- Proxy pool panel: state badges, SOCKS5 address, egress IP, duplicate-IP flags, per-proxy request/error counts

**Get Key** (`#/getkey`): plan picker → heat/power warning → live PoW solver (see section 8) → issued key stored in localStorage and used automatically by Chat.

**Chat** (`#/chat`): ChatGPT-like interface — model picker, SSE streaming with reasoning display, multi-conversation sidebar backed by localStorage, automatic `conversation_id` sticky-session wiring.

## 13. Observability

- **Structured logging**: zerolog, JSON or console, level-configurable, request IDs on every HTTP response.
- **Prometheus**: `/metrics` exposes request counters by model/status, token counters, latency histograms, pool gauges.
- **Health endpoints**: `/health` (gateway + pool summary), `/stats` (pool statistics).
- **Metrics persistence**: SQLite (`METRICS_DB_PATH`), WAL mode, rows pruned after 7 days.

## 14. Multiple Upstream Drivers

`UPSTREAM_PROVIDER` selects how requests reach the AI:

| Driver | Behavior | Use case |
|---|---|---|
| `zen` (default) | Raw OpenAI-compatible proxy to OpenCode Zen | Standard API usage; API keys (`zent-...`, `public`, or a random pick from `UPSTREAM_API_KEYS`) |
| `opencode` | Drives an [OpenCode Server](https://opencode.ai/docs/server) via HTTP (session → message flow), relayed as OpenAI responses | Agent-style usage with an existing `opencode serve` instance |
| `opencode-cli` | Docker-exec's the real `opencode run` CLI *inside* each VPN container — tool use and browsing also egress from that container's IP | Full agentic workloads needing IP-diverse network egress |

In all modes every request egresses through a per-container VPN IP with automatic rotation on rate limits.

## 15. Security Posture

- Optional PoW key gate on `/v1/*`; issued keys stored hashed, 7-day expiry, burst-cooldown abuse guard.
- Containers run non-root in production images; pinned Alpine base; Docker HEALTHCHECK.
- Client-facing errors are sanitized; request bodies capped at 10MB; configurable CORS.
- ProtonVPN credentials live only in `.env`/SQLite on your host; they are never logged or returned by any API.
- SOCKS5 ports bind to `127.0.0.1` only — VPN exits are not reachable from the network.

---

Related: [architecture.md](architecture.md) for internals, [business-logic.md](business-logic.md) for rotation/cost rules.
