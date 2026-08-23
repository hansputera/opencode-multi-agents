# Documentation

Complete documentation for the OpenAI-Compatible API Gateway with ProtonVPN IP rotation.

## What is this project?

A single Go binary that sits between your AI client (anything that speaks the OpenAI API) and an upstream provider (OpenCode Zen by default). Every request is forwarded through a **dedicated VPN container** running a ProtonVPN WireGuard tunnel, so:

- Your real server IP is never exposed to the upstream provider.
- When the upstream rate-limits an IP (HTTP 429), the gateway detects it, **bans that IP/region**, spins up a fresh container on a *different* VPN server, and retries transparently — most clients never see the error.
- Free API keys are earned by solving a **proof-of-work challenge** (three effort tiers unlock higher rate limits).
- A built-in **web_search tool** gives models live internet data through the same rotating IPs.
- A web dashboard shows live traffic per endpoint, token usage, estimated costs, system specifications, and per-proxy egress IPs.

## Document Map

| Document | Contents |
|---|---|
| [core-features.md](core-features.md) | Every feature explained in depth: what it does, why it exists, how it behaves at runtime |
| [pow-api-keys.md](pow-api-keys.md) | The free-key gate: protocol spec, plans, difficulty adaptation, abuse controls, solvers |
| [architecture.md](architecture.md) | Technical deep dive: components, request lifecycle, container lifecycle, data model, concurrency |
| [protonvpn-integration.md](protonvpn-integration.md) | How the native ProtonVPN integration works: SRP auth, certificates, key derivation, WireGuard tunnel |
| [business-logic.md](business-logic.md) | The "rules of the game": rotation strategy, rate-limit handling, PoW economics, cost estimation |

## The 60-Second Version

```
                    ┌─ valid key? ─────────────────────────────────────────┐
Your Client ──▶ Gateway (:8082) ──▶ SOCKS5 ──▶ VPN Container ──▶ ProtonVPN ──▶ Upstream
   ▲                │                                (WireGuard tunnel)
   │                ├─ no key? earn one at #/getkey (PoW: CPU+GPU puzzle)
   │                ├─ model calls web_search? gateway searches via VPN IP, feeds results back
   │                ├─ 429 detected? → ban IP/region → new container on different server → retry
   │                └─ every request logged: route, status, latency, bytes (SQLite + Prometheus)
   │
   └─ dashboard: per-endpoint traffic · tokens · costs · system specs · proxy pool
```

## Quick Links

- Setup & configuration: see the main [README](../README.md)
- Original integration plan: [PLANS/PROTONVPN-INTEGRATION.md](../PLANS/PROTONVPN-INTEGRATION.md)
