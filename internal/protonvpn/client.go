package protonvpn

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/curve25519"
)

// Client handles ProtonVPN API operations
type Client struct {
	store            *Store
	auth             *SRPAuth
	vpnAPIBase       string
	httpClient       *http.Client
	log              *zerolog.Logger
	username         string
	password         string
	certMu           sync.Mutex // Serializes certificate creation to avoid key conflicts
	sessionCookiesRaw string    // raw cookie string for re-import on expiry
}

// NewClient creates a new ProtonVPN client
func NewClient(store *Store, apiBase, vpnAPIBase, username, password string, log *zerolog.Logger) *Client {
	return &Client{
		store:      store,
		auth:       NewSRPAuth(username, password, apiBase, log),
		vpnAPIBase: vpnAPIBase,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log:      log,
		username: username,
		password: password,
	}
}

// SetSessionCookies sets the raw cookie string for imported browser sessions.
// Must be called before EnsureSession so the client knows to re-import on
// expiry instead of falling back to SRP login.
func (c *Client) SetSessionCookies(cookieStr string) {
	c.sessionCookiesRaw = cookieStr
}

// ParseCookieString parses a browser cookie string (e.g. "name=value; name2=value2")
// into []*http.Cookie. It strips cookie attributes (Domain, Path, etc.) and only
// keeps name=value pairs.
func ParseCookieString(cookieStr string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Split on first '=' only — value may contain '='
		idx := strings.IndexByte(part, '=')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:  name,
			Value: value,
		})
	}
	return cookies
}

// ImportBrowserCookies imports cookies from a browser cookie string and stores
// them as the active session. This bypasses SRP authentication entirely.
// The UID is extracted from the AUTH-{UID} cookie name (ProtonVPN convention).
func (c *Client) ImportBrowserCookies(cookieStr string) error {
	cookies := ParseCookieString(cookieStr)
	if len(cookies) == 0 {
		return fmt.Errorf("no valid cookies found in string")
	}

	// Extract UID from the AUTH-{UID} cookie name
	var uid string
	for _, ck := range cookies {
		if strings.HasPrefix(ck.Name, "AUTH-") {
			uid = strings.TrimPrefix(ck.Name, "AUTH-")
			break
		}
	}
	if uid == "" {
		return fmt.Errorf("AUTH-{UID} cookie not found — cannot determine UID")
	}

	// Store as a session with a far-future expiry (browser cookies are
	// refreshed client-side; we treat them as valid until they fail)
	session := &Session{
		UID:       uid,
		UserID:    uid, // best-effort; not critical for API calls
		Cookies:   cookies,
		ExpiresAt: time.Now().Add(24 * time.Hour), // re-check daily
	}

	if err := c.store.SetSession(session); err != nil {
		return fmt.Errorf("failed to store imported session: %w", err)
	}

	c.sessionCookiesRaw = cookieStr
	c.log.Info().Int("cookies", len(cookies)).Str("uid", uid).Msg("Imported browser cookies for ProtonVPN")
	return nil
}

// Login authenticates and stores credentials
func (c *Client) Login(username, password string) error {
	// Perform SRP authentication first
	session, err := c.auth.Login()
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Only store credentials after successful auth
	if err := c.store.SetCredentials(username, password); err != nil {
		return fmt.Errorf("failed to store credentials: %w", err)
	}

	// Store session
	if err := c.store.SetSession(session); err != nil {
		return fmt.Errorf("failed to store session: %w", err)
	}

	c.log.Info().Str("uid", session.UID).Msg("Successfully logged in to ProtonVPN")
	return nil
}

// GetCertificate gets or refreshes a VPN certificate
func (c *Client) GetCertificate(serverName, serverIP, serverPubKey string) (*CertificateResponse, error) {
	c.certMu.Lock()
	defer c.certMu.Unlock()

	// Try to get existing certificate
	cert, err := c.store.GetCertificate()
	if err == nil {
		// Check if certificate is still valid (refresh 1 hour before expiry)
		refreshTime := time.Unix(cert.RefreshTime, 0)
		if time.Now().Before(refreshTime) {
			return cert, nil
		}
		c.log.Info().Msg("Certificate needs refresh")
	}

	// Ensure we have a valid session
	if err := c.EnsureSession(); err != nil {
		return nil, fmt.Errorf("failed to ensure session: %w", err)
	}

	// Get session
	session, err := c.store.GetSession()
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	// Get Ed25519 public key for certificate auth (base64-encoded raw key)
	edPubKeyB64, err := c.GetOrCreateEd25519Key()
	if err != nil {
		return nil, fmt.Errorf("failed to get Ed25519 key: %w", err)
	}

	// Request certificate with Ed25519 public key
	cert, err = c.requestCertificate(session, edPubKeyB64, serverName, serverIP, serverPubKey)
	if err != nil {
		// If key fingerprint conflict, regenerate Ed25519 key and retry once
		if strings.Contains(err.Error(), "2500") || strings.Contains(err.Error(), "fingerprint conflict") {
			c.log.Warn().Msg("Ed25519 key conflict, regenerating...")
			if delErr := c.store.DeleteEd25519Key(); delErr != nil {
				return nil, fmt.Errorf("failed to delete conflicting Ed25519 key: %w", delErr)
			}
			edPubKeyB64, err = c.GetOrCreateEd25519Key()
			if err != nil {
				return nil, fmt.Errorf("failed to regenerate Ed25519 key: %w", err)
			}
			cert, err = c.requestCertificate(session, edPubKeyB64, serverName, serverIP, serverPubKey)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to request certificate: %w", err)
		}
	}

	// Store certificate
	if err := c.store.SetCertificate(cert); err != nil {
		return nil, fmt.Errorf("failed to store certificate: %w", err)
	}

	return cert, nil
}

// GetOrCreateEd25519Key gets or creates an Ed25519 keypair for certificate auth
func (c *Client) GetOrCreateEd25519Key() (publicKeyB64 string, err error) {
	// Try to get existing Ed25519 key
	privKeyBytes, err := c.store.GetEd25519Key()
	if err == nil {
		pubKey := ed25519.PrivateKey(privKeyBytes).Public().(ed25519.PublicKey)
		return base64.StdEncoding.EncodeToString(pubKey), nil
	}

	// Generate new Ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("failed to generate Ed25519 key: %w", err)
	}

	// Store the private key
	if err := c.store.SetEd25519Key(privKey); err != nil {
		return "", fmt.Errorf("failed to store Ed25519 key: %w", err)
	}

	return base64.StdEncoding.EncodeToString(pubKey), nil
}

// requestCertificate requests a new certificate from the API
func (c *Client) requestCertificate(session *Session, clientPublicKeyB64, serverName, serverIP, serverPubKey string) (*CertificateResponse, error) {
	features := map[string]interface{}{
		"PortForwarding": false,
		"SplitTCP":       true,
		"platform":       "Android",
	}
	if serverName != "" {
		features["peerName"] = serverName
	}
	if serverIP != "" {
		features["peerIp"] = serverIP
	}
	if serverPubKey != "" {
		features["peerPublicKey"] = serverPubKey
	}

	reqBody := map[string]interface{}{
		"ClientPublicKey": clientPublicKeyB64,
		"Mode":            "persistent",
		"DeviceName":      "",
		"Features":        features,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.auth.apiBase+"/api/vpn/v1/certificate", strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	commonHeaders(req, session.UID)
	for _, cookie := range session.Cookies {
		req.AddCookie(cookie)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var certResp CertificateResponse
	if err := json.Unmarshal(body, &certResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if certResp.Code != 1000 {
		return nil, fmt.Errorf("API error code %d", certResp.Code)
	}

	return &certResp, nil
}

// FetchServerList fetches the server list from API
func (c *Client) FetchServerList() (*ServerListResponse, error) {
	// Try cache first
	cached, err := c.store.GetServerList()
	if err == nil {
		c.log.Debug().Int("cached", len(cached.LogicalServers)).Msg("Using cached server list")
		return cached, nil
	}

	// Ensure we have a valid session
	if err := c.EnsureSession(); err != nil {
		return nil, fmt.Errorf("failed to ensure session: %w", err)
	}

	// Get session
	session, err := c.store.GetSession()
	if err != nil {
		return nil, fmt.Errorf("no session: %w", err)
	}

	c.log.Debug().Str("uid", session.UID).Int("cookies", len(session.Cookies)).Msg("Fetching server list")

	// Fetch from API
	req, err := http.NewRequest("GET", c.vpnAPIBase+"/api/vpn/v2/logicals", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	commonHeaders(req, session.UID)
	for _, c := range session.Cookies {
		req.AddCookie(c)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	c.log.Debug().Int("status", resp.StatusCode).Int("body_len", len(body)).Msg("Server list response")

	if resp.StatusCode == http.StatusUnauthorized {
		// Clear session so next call triggers re-auth
		c.store.SetSession(nil)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var serverList ServerListResponse
	if err := json.Unmarshal(body, &serverList); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	c.log.Debug().Int("servers", len(serverList.LogicalServers)).Msg("Fetched server list")

	// Debug: log first few servers
	for i := 0; i < min(3, len(serverList.LogicalServers)); i++ {
		s := &serverList.LogicalServers[i]
		key := ""
		if len(s.Servers) > 0 {
			key = s.Servers[0].X25519PublicKey
			if len(key) > 20 {
				key = key[:20] + "..."
			}
		}
		c.log.Debug().Str("name", s.Name).Str("country", s.ExitCountry).
			Int("status", s.Status).Int("phys", len(s.Servers)).
			Str("key", key).Msg("Server sample")
	}

	// Cache the server list
	if err := c.store.SetServerList(&serverList); err != nil {
		c.log.Warn().Err(err).Msg("Failed to cache server list")
	}

	return &serverList, nil
}

// SelectServer selects the best server for a region, optionally avoiding specific servers
func (c *Client) SelectServer(region string, bannedRegions, avoidServers map[string]bool) (*LogicalServer, *Server, error) {
	serverList, err := c.FetchServerList()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch server list: %w", err)
	}

	c.log.Debug().Int("total", len(serverList.LogicalServers)).Str("region", region).Msg("SelectServer: filtering servers")

	// Filter servers by region and banned status
	var candidates []struct {
		logical *LogicalServer
		server  *Server
	}

	for i := range serverList.LogicalServers {
		logical := &serverList.LogicalServers[i]

		// Skip if region doesn't match (if specified)
		if region != "" && logical.ExitCountry != region {
			continue
		}

		// Skip if region is banned
		if bannedRegions != nil && bannedRegions[logical.ExitCountry] {
			continue
		}

		// Skip if server is in the avoid set (to spread across servers)
		if avoidServers != nil && avoidServers[logical.Name] {
			continue
		}

		// Skip if server is offline (ProtonVPN: 0=online, 1=down, 2=maintenance)
		if logical.Status != 0 {
			if i < 3 {
				c.log.Debug().Str("name", logical.Name).Int("status", logical.Status).Msg("Skipping: server offline")
			}
			continue
		}

		// Find a suitable physical server
		for j := range logical.Servers {
			server := &logical.Servers[j]
			if server.Status == 0 && server.X25519PublicKey != "" {
				candidates = append(candidates, struct {
					logical *LogicalServer
					server  *Server
				}{logical, server})
			}
		}
	}

	c.log.Debug().Int("candidates", len(candidates)).Msg("SelectServer: found candidates")

	// If all candidates were avoided, fall back to any available server
	if len(candidates) == 0 && avoidServers != nil && len(avoidServers) > 0 {
		c.log.Debug().Msg("All servers avoided, falling back to any available")
		for i := range serverList.LogicalServers {
			logical := &serverList.LogicalServers[i]
			if region != "" && logical.ExitCountry != region {
				continue
			}
			if bannedRegions != nil && bannedRegions[logical.ExitCountry] {
				continue
			}
			if logical.Status != 0 {
				continue
			}
			for j := range logical.Servers {
				server := &logical.Servers[j]
				if server.Status == 0 && server.X25519PublicKey != "" {
					candidates = append(candidates, struct {
						logical *LogicalServer
						server  *Server
					}{logical, server})
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no available servers for region %s", region)
	}

	// Select based on score and load
	best := candidates[0]
	for _, cand := range candidates[1:] {
		// Prefer higher score, lower load
		if cand.logical.Score > best.logical.Score ||
			(cand.logical.Score == best.logical.Score && cand.logical.Load < best.logical.Load) {
			best = cand
		}
	}

	c.log.Debug().
		Str("server", best.logical.Name).
		Str("country", best.logical.ExitCountry).
		Msg("Selected VPN server")

	return best.logical, best.server, nil
}

// GetOrCreateKeyPair gets or creates a WireGuard X25519 keypair derived from the Ed25519 certificate key.
// ProtonVPN requires the WireGuard key to be derived from the Ed25519 certificate key
// so the server can map the WireGuard tunnel back to the certificate.
func (c *Client) GetOrCreateKeyPair() (privateKey, publicKey string, err error) {
	// Get the Ed25519 private key used for the certificate
	edPrivBytes, err := c.store.GetEd25519Key()
	if err != nil {
		return "", "", fmt.Errorf("no Ed25519 key available (must create certificate first): %w", err)
	}

	// Convert Ed25519 private key to X25519 for WireGuard
	edPriv := ed25519.PrivateKey(edPrivBytes)
	x25519Priv, err := ed25519PrivKeyToCurve25519(edPriv)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert Ed25519 to X25519: %w", err)
	}

	// Derive X25519 public key
	x25519Pub, err := curve25519.X25519(x25519Priv, curve25519.Basepoint)
	if err != nil {
		return "", "", fmt.Errorf("failed to derive X25519 public key: %w", err)
	}

	privateKey = base64.StdEncoding.EncodeToString(x25519Priv)
	publicKey = base64.StdEncoding.EncodeToString(x25519Pub)

	return privateKey, publicKey, nil
}

// ed25519PrivKeyToCurve25519 converts an Ed25519 private key to a Curve25519 (X25519) private key.
func ed25519PrivKeyToCurve25519(edKey ed25519.PrivateKey) ([]byte, error) {
	if len(edKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key size")
	}
	// The X25519 private key is the first 32 bytes of the SHA-512 of the Ed25519 seed
	h := sha512.Sum512(edKey[:32])
	// Clamp the key per X25519 spec
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64
	return h[:32], nil
}

// EnsureSession ensures the session is valid, refreshing if needed.
// When imported cookies are active, it checks them instead of SRP login.
func (c *Client) EnsureSession() error {
	// If we have imported cookies, validate them against the API
	if c.sessionCookiesRaw != "" {
		session, err := c.store.GetSession()
		if err == nil && time.Now().Before(session.ExpiresAt) {
			// Session exists locally — verify it still works with a lightweight API call
			if err := c.validateSession(session); err == nil {
				return nil
			}
			c.log.Warn().Err(err).Msg("Session cookies expired on server, re-importing")
		}
		// Session expired or invalid — re-import the cookies
		if err := c.ImportBrowserCookies(c.sessionCookiesRaw); err != nil {
			return fmt.Errorf("failed to re-import cookies: %w", err)
		}
		// Validate the freshly imported cookies
		session, err = c.store.GetSession()
		if err != nil {
			return fmt.Errorf("no session after import: %w", err)
		}
		if err := c.validateSession(session); err != nil {
			return fmt.Errorf("cookies are invalid: %w", err)
		}
		return nil
	}

	session, err := c.store.GetSession()
	if err == nil && time.Now().Add(5*time.Minute).Before(session.ExpiresAt) {
		return nil // session exists and is not expired
	}

	// Session missing or expired — need to login
	c.log.Info().Bool("expired", err == nil).Msg("Session invalid, logging in")
	return c.loginWithCredentials()
}

// validateSession makes a lightweight API call to verify the session is still valid.
func (c *Client) validateSession(session *Session) error {
	req, err := http.NewRequest("GET", c.vpnAPIBase+"/api/core/v4/users", nil)
	if err != nil {
		return err
	}
	commonHeaders(req, session.UID)
	for _, ck := range session.Cookies {
		req.AddCookie(ck)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readBody(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// loginWithCredentials attempts login using configured or stored credentials.
func (c *Client) loginWithCredentials() error {
	if c.username != "" && c.password != "" {
		return c.Login(c.username, c.password)
	}
	storedUser, storedPass, credErr := c.store.GetCredentials()
	if credErr != nil {
		return fmt.Errorf("no credentials available: %w", credErr)
	}
	return c.Login(storedUser, storedPass)
}
