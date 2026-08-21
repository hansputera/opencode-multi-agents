# ProtonVPN Native WireGuard Integration Plan

## Overview

Replace the current WireGuard key-file-based ProtonVPN approach with ProtonVPN's native WireGuard protocol using certificate-based authentication via SRP (Secure Remote Password).

## Current State

- **Current**: Uses `.key` files containing raw WireGuard private keys
- **New**: Uses OpenVPN credentials (username/password) to authenticate via SRP, obtain certificates, and establish WireGuard tunnels

## Library Stack

| Purpose | Library | License |
|---|---|---|
| SRP Authentication | `github.com/ProtonMail/go-srp` | MIT |
| PGP Operations | `github.com/ProtonMail/go-crypto` | BSD-3 |
| PGP High-Level | `github.com/ProtonMail/gopenpgp/v3` | MIT |
| SQLite3 | `modernc.org/sqlite` (already in project) | MIT |

---

## Authentication Flow

```
┌──────────────────────────────────────────────────────────────┐
│ 1. POST /api/core/v4/auth/info                               │
│    Request:  {"Username":"user@example.com","Intent":"Auto"} │
│    Response: Modulus (PGP signed), ServerEphemeral, Salt,    │
│              SRPSession, Version                             │
├──────────────────────────────────────────────────────────────┤
│ 2. POST /api/core/v4/auth/info (Intent:"Proton")             │
│    Response: Same structure, different ServerEphemeral       │
├──────────────────────────────────────────────────────────────┤
│ 3. SRP Computation (client-side)                             │
│    - Verify PGP signature on Modulus                         │
│    - Compute: ClientProof, ClientEphemeral                   │
│    - Derive: AuthKey from password + salt                    │
├──────────────────────────────────────────────────────────────┤
│ 4. POST /api/core/v4/auth                                    │
│    Request:  ClientProof, ClientEphemeral, SRPSession,       │
│              Username, Payload (encrypted with AuthKey)      │
│    Response: UID, UserID, Scopes, ServerProof                │
│    Cookies: AUTH-*, Session-Id, Tag, Domain, st, Features    │
├──────────────────────────────────────────────────────────────┤
│ 5. GET /api/core/v4/users (with cookies)                     │
│    Response: User info, Keys (encrypted PGP private keys)    │
├──────────────────────────────────────────────────────────────┤
│ 6. GET /api/core/v4/keys/salts (with cookies)                │
│    Response: KeySalts for key decryption                     │
├──────────────────────────────────────────────────────────────┤
│ 7. POST /api/vpn/v1/certificate (with cookies)               │
│    Request:  ClientPublicKey (X25519), Mode, Features        │
│    Response: Certificate, ServerPublicKey, etc.              │
├──────────────────────────────────────────────────────────────┤
│ 8. Configure WireGuard tunnel                                │
│    - PrivateKey: Generated X25519                            │
│    - PublicKey: Server's X25519 key (from certificate)       │
│    - Address: VPN IP from certificate                        │
│    - Endpoint: Server IP:51820                               │
└──────────────────────────────────────────────────────────────┘
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Gateway Process                          │
├─────────────────────────────────────────────────────────────┤
│  internal/protonvpn/                                        │
│  ├── types.go       # API request/response types           │
│  ├── store.go       # SQLite3 credential/certificate store │
│  ├── auth.go        # SRP authentication (go-srp)          │
│  └── client.go      # VPN API client (certificate, servers)│
├─────────────────────────────────────────────────────────────┤
│  internal/proxy/                                            │
│  ├── docker.go      # Updated to use new ProtonVPN client  │
│  ├── types.go       # Updated Proxy struct                  │
│  └── assets/                                               │
│      ├── protonwire-gost.Dockerfile  # wireguard-tools base│
│      └── protonwire-entrypoint.sh    # WG config from env  │
├─────────────────────────────────────────────────────────────┤
│  internal/config/config.go  # Updated ProtonVPN config      │
└─────────────────────────────────────────────────────────────┘
```

---

## File Specifications

### 1. `internal/protonvpn/types.go` (NEW)

All API request/response types for ProtonVPN authentication and VPN operations.

```go
package protonvpn

import (
    "net/http"
    "time"
)

// AuthInfoResponse represents the response from /api/core/v4/auth/info
type AuthInfoResponse struct {
    Code            int    `json:"Code"`
    Modulus         string `json:"Modulus"`
    ServerEphemeral string `json:"ServerEphemeral"`
    Version         int    `json:"Version"`
    Salt            string `json:"Salt"`
    SRPSession      string `json:"SRPSession"`
}

// AuthResponse represents the response from /api/core/v4/auth
type AuthResponse struct {
    Code      int      `json:"Code"`
    ExpiresIn int      `json:"ExpiresIn"`
    UID       string   `json:"UID"`
    UserID    string   `json:"UserID"`
    Scopes    []string `json:"Scopes"`
}

// UserResponse represents the response from /api/core/v4/users
type UserResponse struct {
    Code int  `json:"Code"`
    User User `json:"User"`
}

// User represents a Proton user
type User struct {
    ID    string `json:"ID"`
    Email string `json:"Email"`
    Keys  []Key  `json:"Keys"`
}

// Key represents a PGP key
type Key struct {
    ID         string `json:"ID"`
    Primary    int    `json:"Primary"`
    PrivateKey string `json:"PrivateKey"`
}

// KeySaltsResponse represents the response from /api/core/v4/keys/salts
type KeySaltsResponse struct {
    Code     int       `json:"Code"`
    KeySalts []KeySalt `json:"KeySalts"`
}

// KeySalt represents a key salt for decryption
type KeySalt struct {
    ID      string `json:"ID"`
    KeySalt string `json:"KeySalt"`
}

// CertificateResponse represents the response from /api/vpn/v1/certificate
type CertificateResponse struct {
    Code                int       `json:"Code"`
    SerialNumber        string    `json:"SerialNumber"`
    ClientKey           string    `json:"ClientKey"`
    Certificate         string    `json:"Certificate"`
    ExpirationTime      int64     `json:"ExpirationTime"`
    RefreshTime         int64     `json:"RefreshTime"`
    ServerPublicKey     string    `json:"ServerPublicKey"`
    ServerPublicKeyMode string    `json:"ServerPublicKeyMode"`
    Features            Features  `json:"Features"`
}

// ServerListResponse represents the response from /api/vpn/v2/logicals
type ServerListResponse struct {
    LogicalServers []LogicalServer `json:"LogicalServers"`
}

// LogicalServer represents a logical VPN server
type LogicalServer struct {
    Name         string   `json:"Name"`
    EntryCountry string   `json:"EntryCountry"`
    ExitCountry  string   `json:"ExitCountry"`
    Domain       string   `json:"Domain"`
    Tier         int      `json:"Tier"`
    Features     int      `json:"Features"`
    Score        float64  `json:"Score"`
    Servers      []Server `json:"Servers"`
    Status       int      `json:"Status"`
    Load         int      `json:"Load"`
}

// Server represents a physical VPN server
type Server struct {
    EntryIP         string `json:"EntryIP"`
    ExitIP          string `json:"ExitIP"`
    Domain          string `json:"Domain"`
    X25519PublicKey string `json:"X25519PublicKey"`
    Status          int    `json:"Status"`
}

// Features represents VPN server features
type Features struct {
    Bouncing       string `json:"Bouncing"`
    PortForwarding bool   `json:"PortForwarding"`
    SplitTCP       bool   `json:"SplitTCP"`
    PeerName       string `json:"peerName"`
    PeerIP         string `json:"peerIp"`
    PeerPublicKey  string `json:"peerPublicKey"`
    Platform       string `json:"platform"`
}

// Session holds authentication state
type Session struct {
    UID       string
    UserID    string
    AuthKey   []byte
    Cookies   []*http.Cookie
    ExpiresAt time.Time
}
```

### 2. `internal/protonvpn/store.go` (NEW)

SQLite3 store for credentials, sessions, certificates, and server cache.

```go
package protonvpn

import (
    "database/sql"
    "encoding/json"
    "time"
)

// Store manages SQLite3 storage for ProtonVPN data
type Store struct {
    db *sql.DB
}

// NewStore creates a new SQLite3 store
func NewStore(path string) (*Store, error)

// Close closes the database
func (s *Store) Close() error

// migrate creates the schema
func (s *Store) migrate() error

// Schema tables:
// - credentials: id, username, password_hash, created_at
// - session: id, uid, user_id, auth_key, cookies, expires_at, created_at
// - certificates: id, certificate, client_key, server_key, server_ip, vpn_ip, expires_at, created_at
// - server_cache: id, server_list_json, fetched_at
// - wireguard_keys: id, private_key, public_key, created_at

// Credential operations
func (s *Store) GetCredentials() (username, password string, err error)
func (s *Store) SetCredentials(username, password string) error

// Session operations
func (s *Store) GetSession() (*Session, error)
func (s *Store) SetSession(session *Session) error

// Certificate operations
func (s *Store) GetCertificate() (*CertificateResponse, error)
func (s *Store) SetCertificate(cert *CertificateResponse) error

// Server cache operations
func (s *Store) GetServerList() (*ServerListResponse, error)
func (s *Store) SetServerList(list *ServerListResponse) error

// WireGuard key operations
func (s *Store) GetWireGuardKey() (privateKey, publicKey string, err error)
func (s *Store) SetWireGuardKey(privateKey, publicKey string) error
```

### 3. `internal/protonvpn/auth.go` (NEW)

SRP authentication implementation using `go-srp`.

```go
package protonvpn

import (
    "github.com/ProtonMail/go-srp"
    "github.com/ProtonMail/go-crypto/openpgp"
    "github.com/rs/zerolog"
)

// SRPAuth handles ProtonVPN SRP authentication
type SRPAuth struct {
    username   string
    password   string
    apiBase    string
    httpClient *http.Client
    log        *zerolog.Logger
}

// NewSRPAuth creates a new SRP authenticator
func NewSRPAuth(username, password, apiBase string, log *zerolog.Logger) *SRPAuth

// Login performs the full SRP authentication flow
func (a *SRPAuth) Login() (*Session, error)
// 1. POST /api/core/v4/auth/info (Intent:"Auto")
// 2. POST /api/core/v4/auth/info (Intent:"Proton")
// 3. Verify PGP signature on modulus
// 4. Compute SRP client proof using go-srp
// 5. POST /api/core/v4/auth with proof
// 6. Return session with cookies

// verifyModulusSignature verifies the PGP signature on the modulus
func verifyModulusSignature(signedModulus string) ([]byte, error)
// Uses go-crypto/openpgp to verify

// RefreshSession refreshes an expired session
func (a *SRPAuth) RefreshSession(session *Session) (*Session, error)
```

### 4. `internal/protonvpn/client.go` (NEW)

VPN API client for certificate and server operations.

```go
package protonvpn

import (
    "github.com/rs/zerolog"
)

// Client handles ProtonVPN API operations
type Client struct {
    store      *Store
    auth       *SRPAuth
    httpClient *http.Client
    log        *zerolog.Logger
}

// NewClient creates a new ProtonVPN client
func NewClient(store *Store, apiBase, username, password string, log *zerolog.Logger) *Client

// Login authenticates and stores credentials
func (c *Client) Login(username, password string) error

// GetCertificate gets or refreshes a VPN certificate
func (c *Client) GetCertificate() (*CertificateResponse, error)
// 1. Check store for valid certificate
// 2. If expired, refresh via API
// 3. Store new certificate

// FetchServerList fetches the server list from API
func (c *Client) FetchServerList() (*ServerListResponse, error)
// GET /api/vpn/v2/logicals

// SelectServer selects the best server for a region
func (c *Client) SelectServer(region string, bannedRegions map[string]bool) (*LogicalServer, *Server, error)

// GetOrCreateKeyPair gets or creates a WireGuard X25519 keypair
func (c *Client) GetOrCreateKeyPair() (privateKey, publicKey string, error)

// EnsureSession ensures the session is valid, refreshing if needed
func (c *Client) EnsureSession() error
```

### 5. `internal/config/config.go` (UPDATED)

**Remove:**
```go
ProtonVPNPrivateKeyDir string `yaml:"protonvpn_private_key_dir" env:"PROTONVPN_PRIVATE_KEY_DIR"`
```

**Add:**
```go
// ProtonVPN OpenVPN credentials
ProtonVPNUsername  string `yaml:"protonvpn_username" env:"PROTONVPN_USERNAME"`
ProtonVPNPassword  string `yaml:"protonvpn_password" env:"PROTONVPN_PASSWORD"`

// ProtonVPN API base URL
ProtonVPNAPIBase   string `yaml:"protonvpn_api_base" env:"PROTONVPN_API_BASE"`

// SQLite3 path for credential storage
ProtonVPNStorePath string `yaml:"protonvpn_store_path" env:"PROTONVPN_STORE_PATH"`

// Server region filter (comma-separated country codes)
ProtonVPNRegions   string `yaml:"protonvpn_regions" env:"PROTONVPN_REGIONS"`
```

**Default values:**
```go
ProtonVPNAPIBase:   "https://account.protonvpn.com",
ProtonVPNStorePath: "data/protonvpn.db",
ProtonVPNRegions:   "NL,US,JP,DE",
```

**Validation:**
```go
if c.ProtonVPNUsername == "" {
    return fmt.Errorf("PROTONVPN_USERNAME is required")
}
if c.ProtonVPNPassword == "" {
    return fmt.Errorf("PROTONVPN_PASSWORD is required")
}
```

### 6. `internal/proxy/assets/protonwire-gost.Dockerfile` (UPDATED)

```dockerfile
FROM alpine:3.19

RUN apk --no-cache add wireguard-tools gost bash

COPY protonwire-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

### 7. `internal/proxy/assets/protonwire-entrypoint.sh` (UPDATED)

```bash
#!/bin/bash
set -e

# Configure WireGuard from environment variables
cat > /etc/wireguard/wg0.conf << EOF
[Interface]
PrivateKey = ${WIREGUARD_PRIVATE_KEY}
Address = ${WIREGUARD_ADDRESS}
DNS = ${WIREGUARD_DNS}

[Peer]
PublicKey = ${WIREGUARD_SERVER_PUBLIC_KEY}
Endpoint = ${WIREGUARD_ENDPOINT}
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
EOF

# Start WireGuard
wg-quick up wg0

# Start gost SOCKS5 proxy
gost -L=socks5://:1080 &
GOST_PID=$!

# Wait for gost to be ready
sleep 1

# Keep WireGuard running in foreground
wait $GOST_PID
```

### 8. `internal/proxy/docker.go` (UPDATED)

Key changes in `CreateEx`:

```go
func (dm *DockerManager) CreateEx(ctx context.Context, bannedRegions map[string]bool) (*Proxy, error) {
    // 1. Ensure session is valid
    if err := dm.protonvpn.EnsureSession(); err != nil {
        return nil, fmt.Errorf("protonvpn session error: %w", err)
    }

    // 2. Select server from API
    logical, server, err := dm.protonvpn.SelectServer("", bannedRegions)
    if err != nil {
        return nil, fmt.Errorf("failed to select server: %w", err)
    }

    // 3. Get or create WireGuard keypair
    privateKey, publicKey, err := dm.protonvpn.GetOrCreateKeyPair()
    if err != nil {
        return nil, fmt.Errorf("failed to get wireguard keys: %w", err)
    }

    // 4. Get certificate (refresh if needed)
    cert, err := dm.protonvpn.GetCertificate()
    if err != nil {
        return nil, fmt.Errorf("failed to get certificate: %w", err)
    }

    // 5. Create container with new env vars
    env := []string{
        "WIREGUARD_PRIVATE_KEY=" + privateKey,
        "WIREGUARD_SERVER_PUBLIC_KEY=" + server.X25519PublicKey,
        "WIREGUARD_ADDRESS=" + cert.Features.PeerIP + "/32",
        "WIREGUARD_ENDPOINT=" + server.ExitIP + ":51820",
        "WIREGUARD_DNS=10.2.0.1",
    }

    // ... rest of container creation
}
```

### 9. `internal/proxy/types.go` (UPDATED)

```go
type Proxy struct {
    // ... existing fields ...
    Region      string `json:"region"`
    ServerName  string `json:"server_name"`  // NEW: e.g., "NL-FREE#79"
    ServerIP    string `json:"server_ip"`    // NEW: e.g., "185.177.124.84"
    // REMOVE: KeyFile string `json:"-"`
}
```

### 10. `docker-compose.yml` (UPDATED)

```yaml
environment:
  - PROTONVPN_USERNAME=${PROTONVPN_USERNAME}
  - PROTONVPN_PASSWORD=${PROTONVPN_PASSWORD}
  - PROTONVPN_API_BASE=${PROTONVPN_API_BASE:-https://account.protonvpn.com}
  - PROTONVPN_STORE_PATH=/app/data/protonvpn.db
  - PROTONVPN_REGIONS=${PROTONVPN_REGIONS:-NL,US,JP,DE}
volumes:
  - gateway-data:/app/data
  # REMOVE: ./protonvpn-keys:/app/protonvpn-keys:ro
```

### 11. `.env.example` (UPDATED)

```bash
# ProtonVPN OpenVPN credentials
PROTONVPN_USERNAME=your-email@example.com
PROTONVPN_PASSWORD=your-password

# ProtonVPN API base URL
PROTONVPN_API_BASE=https://account.protonvpn.com

# SQLite3 path for credential storage
PROTONVPN_STORE_PATH=data/protonvpn.db

# Server region filter (comma-separated country codes)
PROTONVPN_REGIONS=NL,US,JP,DE
```

---

## Go Dependencies to Add

```
github.com/ProtonMail/go-srp v0.0.7
github.com/ProtonMail/go-crypto v1.4.1
github.com/ProtonMail/gopenpgp/v3 v3.1.0
```

---

## Implementation Order

1. **Phase 1-2**: Create `types.go` and `store.go` (data layer)
2. **Phase 3**: Create `auth.go` (SRP authentication)
3. **Phase 4**: Create `client.go` (VPN API client)
4. **Phase 5**: Update `config.go` (new config fields)
5. **Phase 6-7**: Update Dockerfile and entrypoint
6. **Phase 8-9**: Update `docker.go` and `types.go`
7. **Phase 10**: Update docker-compose.yml
8. **Phase 11**: Add Go dependencies
9. **Phase 12**: Test and verify

---

## Security Considerations

1. **Password Storage**: Store hashed/encrypted in SQLite3
2. **Session Tokens**: Store in SQLite3 with expiration
3. **Certificates**: Store with refresh time
4. **WireGuard Keys**: Store generated keypairs
5. **No Hardcoding**: All credentials via env vars or SQLite

---

## Error Handling

1. **Authentication Failure**: Return clear error, prompt re-login
2. **Session Expired**: Auto-refresh using stored credentials
3. **Certificate Expired**: Auto-refresh via API
4. **Server Unavailable**: Fall back to other servers
5. **Network Errors**: Retry with exponential backoff

---

## Testing Strategy

1. **Unit Tests**: Test each component in isolation
   - `store_test.go`: Test SQLite3 operations
   - `auth_test.go`: Test SRP authentication flow
   - `client_test.go`: Test API client operations

2. **Integration Tests**: Test full flow
   - Login → Certificate → Server Selection → Container Creation

3. **Manual Testing**: Test with real ProtonVPN account
   - Verify authentication works
   - Verify certificate is obtained
   - Verify WireGuard tunnel establishes
   - Verify IP check passes

---

## Rollback Plan

If the implementation fails or has issues:

1. Keep the old `protonvpn-keys/` directory
2. Add a config toggle: `PROTONVPN_USE_NATIVE=true`
3. If `false`, fall back to old key-file approach
4. Both approaches can coexist during transition

---

## References

- [ProtonVPN API Documentation](https://protonvpn.com/api)
- [go-srp Library](https://github.com/ProtonMail/go-srp)
- [go-crypto Library](https://github.com/ProtonMail/go-crypto)
- [gopenpgp Library](https://github.com/ProtonMail/gopenpgp)
- [WireGuard Quick Start](https://www.wireguard.com/quickstart/)
