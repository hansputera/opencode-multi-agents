# Documentation

Complete documentation for the OpenAI-Compatible API Gateway with ProtonVPN IP rotation.

## What is this project?

A single Go binary that sits between your AI client (anything that speaks the OpenAI API) and an upstream provider (OpenCode Zen by default). Every request is forwarded through a **dedicated VPN container** running a ProtonVPN WireGuard tunnel, so:

- Your real server IP is never exposed to the upstream provider.
- When the upstream rate-limits an IP (HTTP 429), the gateway detects it, **bans that IP/region**, spins up a fresh container on a *different* VPN server, and retries transparently — most clients never see the error.
- A web dashboard shows live traffic, token usage, estimated costs, and per-proxy egress IPs.

## Document Map

| Document | Contents |
|---|---|
| [core-features.md](core-features.md) | Every feature explained in depth: what it does, why it exists, how it behaves at runtime |
| [architecture.md](architecture.md) | Technical deep dive: components, request lifecycle, container lifecycle, data model, concurrency |
| [protonvpn-integration.md](protonvpn-integration.md) | How the native ProtonVPN integration works: SRP auth, certificates, key derivation, WireGuard tunnel |
| [business-logic.md](business-logic.md) | The "rules of the game": rate-limit handling strategy, IP rotation policy, cost estimation model, failure & recovery semantics |

## The 60-Second Version

```
Your Client ──▶ Gateway (:8082) ──▶ SOCKS5 ──▶ VPN Container ──▶ ProtonVPN ──▶ Upstream
                     │                                (WireGuard tunnel)
                     │
                     ├─ 429 detected? → ban IP/region → new container on different server → retry
                     ├─ records tokens + estimated cost per request (SQLite)
                     └─ dashboard at http://localhost:8082
```

## Quick Links

- Setup & configuration: see the main [README](../README.md)
- Original integration plan: [PLANS/PROTONVPN-INTEGRATION.md](../PLANS/PROTONVPN-INTEGRATION.md)
