# PoW-Gated API Keys

How the gateway's free API key issuance works: a Hashcash-style proof-of-work gate (no blockchain, no signup) where clients earn keys by spending compute. Server verification costs microseconds.

- **When active**: `POW_ENABLED=true` (default in docker-compose). `/v1/*` then requires a key — either from `GATEWAY_API_KEYS` or one earned here.
- **Server code**: `internal/pow/` + `internal/handler/pow.go`
- **Clients**: web UI (`#/getkey`, Workers + WebGPU) and native CLI (`cmd/powsolver`)

## Plans

| Plan | Extra difficulty | Rate limit | Typical effort |
|---|---|---|---|
| `basic` | +0 bits | 100 req/min | ~30–90 s |
| `plus` | +4 bits (~16× work) | 250 req/min | ~8–25 min, faster with GPU |
| `pro` | +8 bits (~256× work) | 500 req/min | Heavy — GPU strongly recommended |

Difficulty bits are added on top of the adaptive global base and clamped to `[POW_MIN_DIFFICULTY, POW_MAX_DIFFICULTY]`. Keys expire after `POW_KEY_TTL` (default **7 days**).

## Protocol v1

### Challenge — `GET /api/pow/challenge?plan=basic|plus|pro[&algo=blake3|sha256]`

```json
{
  "challenge": {
    "version": 1,
    "id": "01JZX...",
    "resource": "api-key",
    "algo": "sha256",
    "difficulty": 26,
    "salt": "<32 bytes base64>",
    "bind": "<base64>",          // BLAKE3-128 of (IP + User-Agent)
    "issued_at": 1737500000,
    "expires_at": 1737500600,
    "plan": "basic"
  },
  "target_seconds": [30, 90],
  "warning": "Solving runs your CPU and GPU at full power..."
}
```

`algo` defaults to `sha256` (browser/WebGPU friendly); native clients may request `blake3`. The server verifies either.

### Canonical preimage

Solvers hash this exact byte string:

```
pow-v1|<id>|<resource>|<algo>|<difficulty>|<salt>|<bind>|<counter>
```

- `counter` is **fixed-width 10-digit zero-padded decimal** (`0000000000`…`4294967295`) — the fixed width is what lets the WebGPU kernel patch digits into a prebuilt message template.
- The format MUST stay byte-stable within protocol version 1; it is pinned by a golden test (`TestPreimageFormatStability`). Changing it means bumping `version`.

A solution is valid iff `leadingZeroBits(H(preimage)) >= difficulty`, where `H` is BLAKE3-256 or SHA-256 per the challenge's `algo`.

### Solution — `POST /api/pow/redeem`

```json
{
  "challenge_id": "01JZX...",
  "counters": ["0000001234"],
  "client_meta": { "workers": 8, "gpu": true, "elapsed_ms": 41230 }
}
```

`client_meta` is telemetry **only** — the server never trusts client-claimed hashrate or timing.

Success response:

```json
{
  "api_key": "sk-gw-<43 chars>",
  "plan": "basic",
  "rpm": 100,
  "prefix": "sk-gw-abcd...",
  "created_at": 1737500123,
  "expires_at": 1738104923
}
```

The key is shown once; only its SHA-256 hash is stored.

### Error codes

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_json` / `invalid_solution` / `invalid_algo` | Malformed request or failed proof |
| 401 | `invalid_api_key` | (on `/v1/*`) missing/wrong key |
| 403 | `binding_mismatch` | Redeemed from a different IP/fingerprint than issued |
| 404 | `unknown_challenge` | Unknown ID |
| 409 | `already_used` | Challenge consumed — single-use |
| 410 | `challenge_expired` | Past TTL |
| 429 | `too_fast` | Arrived <500 ms after issuance (impossible for a real solve) |
| 429 | `rate_limited` | Challenge quota or plan RPM exhausted |

## Verification Path (microseconds)

1. Parse request, load challenge by primary key (indexed SQLite)
2. Expiry / used-flag / binding / too-fast checks (comparisons only)
3. **One hash** of a ~150-byte string per submitted counter (cap: 8 counters)
4. `UPDATE pow_challenges SET used=1 WHERE id=? AND used=0 AND expires_at>?` — rows-affected decides single-use atomically
5. Issue key: 32 random bytes → `sk-gw-` + base64url; store SHA-256(key) + plan + expiry; add to in-memory auth cache

State lives in SQLite (`data/pow.db`, WAL): `pow_challenges`, `api_keys`, `pow_ip_bonus`. The hot auth path is memory-only — issued keys are cached at startup and on issuance (single-process gateway).

## Difficulty Adaptation

All signals are **server-measured** (issue → redeem wall time); client claims are ignored.

- **EWMA controller**: every redemption records elapsed solve time. α=0.2 smoothing; after ≥5 samples, base moves ±1 bit toward keeping the median solve inside the 30–90 s target window. Clamped `[20, 40]`.
- **Per-IP farming penalty**: each earned key adds +1 bit to that client's next challenge (capped +8, decays after 24 h idle) — mass issuance gets exponentially harder.
- **Flood escalation**: >10× the normal challenge rate globally triggers +2 bits for 15 minutes.

## Abuse Controls

| Control | Setting | Behavior |
|---|---|---|
| Challenge rate limit | `POW_CHALLENGE_RATE_PER_MIN=6`, `_PER_DAY=60` | Per-IP token bucket + daily cap on issuance |
| Burst cooldown | `POW_BURST_RPS=5`, `POW_BURST_COOLDOWN=5m` | >5 requests in any rolling 1 s window freezes the key; every request during cooldown gets `429 key_cooldown` + `Retry-After`. Checked **before** RPM so abusers see the punitive state |
| Plan RPM buckets | `POW_PLAN{n}_RPM` | Token bucket per issued key (burst capacity = one minute's worth); exceeded → `429 rate_limited` |
| Too-fast rejection | fixed 500 ms | Statistical impossibility filter for real difficulties |

Env keys (`GATEWAY_API_KEYS`) bypass all per-key limits (admin keys).

## Solver Implementations

**Browser** (`#/getkey` view, `internal/web/dist/getkey.js`):

- CPU: N Web Workers (pure-JS SHA-256), each scanning a disjoint counter lane
- GPU: WebGPU compute kernel (WGSL SHA-256) on an additional lane, running simultaneously with the workers
- The kernel self-tests against a JS-computed solution before use and falls back to CPU-only on any failure (driver quirks can't produce garbage keys)
- Live progress: aggregated H/s, probability-based progress bar (`P = 1 − e^(−tried/2^diff)`), constant ETA at current hashrate, cancel button; pauses when the tab is hidden

**Native CLI** (`cmd/powsolver`): multi-core BLAKE3 loop.

```bash
go run ./cmd/powsolver --server http://localhost:8082 --plan basic
go build -o powsolver ./cmd/powsolver && ./powsolver --server https://gw.example.com --plan pro --workers 16
```

**Writing another SDK**: fetch a challenge, replicate the preimage format byte-for-byte, scan counters until the digest has enough leading zero bits, POST the winning counter. That's the entire protocol — no session, no cookies, no nonce handshake.

## Configuration Reference

| Variable | Default | Purpose |
|---|---|---|
| `POW_ENABLED` | `false` (compose: `true`) | Gate `/v1/*` behind keys |
| `POW_STORE_PATH` | `data/pow.db` | SQLite location |
| `POW_CHALLENGE_TTL` | `10m` | Challenge lifetime |
| `POW_KEY_TTL` | `168h` | Issued-key lifetime |
| `POW_BASE_DIFFICULTY` / `_MIN` / `_MAX` | 24 / 20 / 40 | Adaptive base + clamps |
| `POW_PLAN{n}_DIFFICULTY` | 0 / 4 / 8 | Extra bits per plan |
| `POW_PLAN{n}_RPM` | 100 / 250 / 500 | Rate limit per plan |
| `POW_BURST_RPS` / `POW_BURST_COOLDOWN` | 5 / 5m | Burst guard (0 disables) |
| `POW_CHALLENGE_RATE_PER_MIN` / `_PER_DAY` | 6 / 60 | Issuance quota per IP |
| `POW_ADJUST_INTERVAL` | 30s | Housekeeping tick |

## Threat Mitigations Summary

- **Replay** — single-use atomic consume; per-challenge random salt
- **Pre-computation** — client binding is inside the preimage; work can't be front-run before assignment
- **Challenge farming / issuer DoS** — per-IP issuance quotas + flood escalation difficulty
- **Fake solutions** — server recomputes the hash; nothing client-reported is trusted
- **Key scaling** — hashed at rest, 7-day expiry, per-IP difficulty penalty on farming
- **Algorithm breakage** — `version` field end-to-end; v2 can change everything without breaking v1 clients (they simply fail verification and refetch)

---

Related: [core-features](core-features.md), [architecture](architecture.md), [business logic](business-logic.md).




