# ProtonVPN Native Integration (As Built)

How the gateway talks to ProtonVPN: authentication, certificates, key derivation, and the in-container WireGuard tunnel. This reflects the **implemented** system; see [PLANS/PROTONVPN-INTEGRATION.md](../PLANS/PROTONVPN-INTEGRATION.md) for the original plan and its deviation log.

## Why This Design

ProtonVPN's modern clients don't use static config files — they:

1. Log in with SRP (the password never leaves the device in usable form),
2. Generate a local key pair,
3. Ask ProtonVPN's API to issue a *certificate* binding the public key to a chosen server,
4. Build a WireGuard tunnel from the certificate contents.

The gateway replicates exactly this flow, which means fully automated credential handling: you only supply your ProtonVPN username/password.

## Step 1 — SRP Authentication (`internal/protonvpn/auth.go`)

The flow mirrors `account.protonvpn.com`'s web app (headers include `x-pm-appversion: web-vpn-settings@...`, `x-pm-uid`, and `Accept: application/vnd.protonmail.v1+json`):

```
1. POST /api/auth/v4/sessions          -> AccessToken, RefreshToken, UID
2. GET  /api/core/v4/auth/cookies      -> bootstrap JWT AUTH cookie + friends
3. POST /api/core/v4/auth/info         -> Modulus(PGP-signed), ServerEphemeral,
        {Username, Intent:"Auto"}         Salt, SRPSession, Version
4. go-srp: NewAuth(version, username, password, salt, modulus,
        serverEphemeral).GenerateProofs(2048) -> ClientProof, ClientEphemeral
5. POST /api/core/v4/auth              -> UID, UserID, ExpiresIn
        NOTE: response sets a NEW simple AUTH cookie; mergeCookies()
        replaces the bootstrap JWT by name -- critical detail
6. GET  /api/core/v4/users             -> user info   (x-pm-uid + merged cookies)
7. GET  /api/core/v4/keys/salts
```

After step 5, every authenticated call carries the session UID header and the merged cookie jar. The session (UID + cookies + expiry) is persisted to SQLite so restarts skip re-login until it expires; `EnsureSession()` transparently re-authenticates when needed.

## Step 2 — Server Selection (`client.go: SelectServer`)

- The full logical-server list is fetched from `PROTONVPN_VPN_API_BASE` (`vpn-api.proton.me/api/vpn/v2/logicals`) and cached in SQLite.
- Filtering rules:
  - `Status == 0` only (0 = online; 1 = down, 2 = maintenance)
  - region must be within `PROTONVPN_REGIONS` (when specified) and not banned
  - servers already used by pool proxies are skipped (`avoidServers`) — this is what gives each container a distinct exit IP subnet
  - ranked by load/score among physical peers
- If everything is filtered out by the avoid-list, the list is re-admitted without it (availability beats diversity).

## Step 3 — Certificate Issuance (`client.go: GetCertificate`)

Request to `account.protonvpn.com/api/vpn/v1/certificate` (the account domain, not the VPN API domain):

```json
{
  "ClientPublicKey": "<Ed25519 public key, raw base64>",
  "Mode": "persistent",
  "Features": {
    "peerName":      "<logical server name>",
    "peerIp":        "<server entry IP>",
    "peerPublicKey": "<server X25519 public key>",
    "platform":      "linux"
  }
}
```

Key facts learned the hard way:

- The cert API wants an **Ed25519** public key (not X25519, not PEM).
- `Features` is an object; peer fields bind the cert to one server.
- Issuance is serialized behind a mutex — parallel requests cause key conflicts (HTTP 409 / code 2500).
- Response includes `ExpirationTime` plus (for paid plans) `IPv4`, `IPv6`, `DNS`. Free accounts omit these.

## Step 4 — Key Derivation

ProtonVPN's backend maps the certificate's Ed25519 identity to the WireGuard X25519 peer key. Therefore the X25519 private key is **derived**, never generated independently:

```
X25519 priv = clamp(SHA-512(Ed25519 priv)[0..32])   // ed25519PrivKeyToCurve25519
```

If an unrelated X25519 key is used, the tunnel handshake appears fine but **all data traffic is silently dropped**. This was the root cause of the original "handshake but no connectivity" bug.

## Step 5 — In-Container Tunnel & SOCKS5

Each proxy container runs the built image (Alpine + wireguard-tools + iptables + gost v3 from GitHub releases):

```
entrypoint:
  write /etc/wireguard/wg0.conf from env vars
  wg-quick up wg0                 # handles routing + DNS correctly
  exec gost -L socks5://:1080     # wait for egress first
```

wg0.conf mapping:

| Field | Value |
|---|---|
| PrivateKey | derived X25519 private key |
| Address | `cert.IPv4/32`, fallback `10.2.0.2/32` (free tier) |
| DNS | `cert.DNS[0]`, fallback `10.2.0.1` (ProtonVPN internal DNS; external DNS like 1.1.1.1 is blocked through the tunnel) |
| Peer PublicKey | `cert.Features.PeerPublicKey` |
| Endpoint | `cert.Features.PeerIP:51820` (PeerIP is the *server's* endpoint, not the client address) |
| AllowedIPs | `0.0.0.0/0` |

Container hardening/runtime: privileged + `NET_ADMIN` (WireGuard interface + sysctls), `rp_filter=2`, `src_valid_mark=1`, IPv6 disabled, CPU/memory limits, gost port published on `127.0.0.1` only.

## Failure Handling Summary

| Condition | Behavior |
|---|---|
| Session expired | Auto re-login via stored credentials |
| Cert expired/near refresh time | Re-request automatically |
| Key conflict (409/2500) | Regenerate Ed25519 key, retry |
| No online server in region | Try other regions in `PROTONVPN_REGIONS` |
| All servers avoided | Fall back to any online server |
| Tunnel dead (health check fails) | Proxy marked unhealthy, container replaced |

## Persistence Schema

SQLite at `PROTONVPN_STORE_PATH`: `credentials`, `session`, `certificates`, `server_cache`, `wireguard_keys`, `ed25519_keys`. Everything survives restarts; credentials are never logged or exposed through any API.
