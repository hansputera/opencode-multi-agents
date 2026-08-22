# Business Logic

The "rules of the game": how the gateway makes routing decisions, handles failure, rotates IPs, and estimates cost. This is the behavioral contract the system implements on top of the mechanics described in [architecture.md](architecture.md).

## The Core Business Problem

Upstream AI providers protect themselves with **per-IP rate limits**. A single server making many requests gets throttled (HTTP 429), stalling every user behind it. Buying more quota or accounts raises cost. This gateway attacks the constraint differently: **many IPs, rotated automatically**, so a throttle on one IP is an inconvenience, not an outage.

Three levers, in order of preference:

1. **Prevention** — spread containers across different VPN servers up front so no two proxies share an exit IP subnet.
2. **Transparent recovery** — when a 429 happens anyway, retry through a fresh IP without telling the client.
3. **Honest degradation** — only if no fresh IP materializes quickly, return a real `429` with `Retry-After` so clients can back off properly.

## Rule 1 — IP Diversity at Birth

When any container is created:

- Server selection receives an **avoid-set** of logical servers already used by live pool proxies.
- Selection skips avoided servers → each new container lands on a different physical server → different exit IP subnet.
- If all servers are already used (pool larger than server count), the avoid-set is dropped: availability wins over diversity.
- Duplicate egress IPs that still occur (server-side NAT) are surfaced as red "duplicate ip" flags on the dashboard.

## Rule 2 — Rate Limit Detection

A response is treated as a rate limit if either:

- HTTP status is `429`, **or**
- the body contains throttle wording ("rate limit", "quota exceeded", ...) regardless of status code.

Keyword matching matters because some providers wrap 429s in 200/502 responses.

## Rule 3 — Ban & Replace

On detected rate limiting for proxy P:

1. **Ban**: P's egress IP *and* its region are excluded from routing for `max(IP_BAN_DURATION, upstream Retry-After)`. New containers will not select servers from a banned region.
2. **Cooldown**: P moves to cooldown state — it keeps running but receives no traffic until the ban expires.
3. **Replacement**: a new container boots immediately on a different server/region (asynchronously; the current request doesn't wait for boot unless needed).

## Rule 4 — Transparent Retry Window

For the request that hit the limit:

- The gateway waits up to `RATE_LIMIT_FRESH_IP_WAIT` (default 90s — covers typical container boot of 40–90s) for an unbanned proxy to become available.
- If one appears: the request is retried there and **succeeds normally** — the client sees latency, never an error.
- Standard retry policy (`MAX_RETRIES`, exponential backoff capped at `RETRY_MAX_DELAY`) applies for transient network errors.

## Rule 5 — Honest Failure

If no fresh IP appears within the wait window:

- Respond `429 Too Many Requests` with `Retry-After: RATE_LIMIT_RETRY_AFTER`.
- Never masquerade exhaustion as a gateway bug (`502`) — clients that honor `Retry-After` recover cleanly.

## Rule 6 — Account-Level Throttling Escape Hatch

Region rotation cannot dodge **account/key-based** limits (throttle follows the credential, not the IP). For those:

- `UPSTREAM_API_KEYS=a,b,c` — a random key is picked per request, spreading quota across accounts.
- Entries may be `public`, `zent-...`, or `header:value` custom credentials.

## Rule 7 — Sticky Sessions Override Rotation (Deliberately)

`conversation_id` pins a conversation to one proxy for `STICKY_SESSION_TTL`. Consistency beats diversity for that conversation: if the pinned IP gets banned mid-conversation, the session re-pins to a fresh proxy on next use.

## Cost Estimation Model

- Every response's token usage (prompt/completion/cached) is extracted and stored.
- Cost = `prompt_tokens/1M × input_rate + completion_tokens/1M × output_rate + cached_tokens/1M × cached_rate`.
- Rates come from `MODEL_PRICING` (`model:in,out,cached`, USD per 1M tokens; cached defaults to input × 0.5).
- Unknown model ⇒ $0 estimate (never guessed). Estimates are labeled as such everywhere in the UI.

This makes the dashboard a usage ledger: totals, per-model breakdowns, and per-minute traffic — enough to answer "which model burns my free tier fastest?" without being a billing system.

## Health & Lifecycle Rules

- Health probe: fetch egress IP through each SOCKS5 port every `HEALTH_CHECK_PERIOD`. Verifies the full path (tunnel + proxy), not just process liveness.
- Unhealthy proxies are removed and replaced automatically — pool size converges back to `PROXY_POOL_SIZE`.
- Region bans expire lazily; expired entries are pruned during lookups and periodic sweeps.
- On startup, orphaned VPN containers from previous runs are removed before the pool fills.

## Design Trade-offs Worth Knowing

| Decision | Alternative | Why this way |
|---|---|---|
| Free-tier shared tunnel address `10.2.0.2/32` | Paid cert-provided addresses | Cert API omits IPv4/DNS on free accounts; self-healing covers concurrency limits |
| In-memory sticky sessions | Redis-backed | Single-binary simplicity; documented restart trade-off |
| `--privileged` containers | Minimal caps | `wg-quick` needs sysctl/iptables control; can be tightened later |
| SQLite metrics | Prometheus-only | Dashboard needs history without external infra |
| Keyword-based 429 detection | Status-code only | Providers wrap 429s inconsistently |

---

Related: [core-features.md](core-features.md), [architecture.md](architecture.md).
