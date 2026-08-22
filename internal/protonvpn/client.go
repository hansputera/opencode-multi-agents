package protonvpn

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/curve25519"
)

// Client handles ProtonVPN API operations
type Client struct {
	store       *Store
	auth        *SRPAuth
	vpnAPIBase  string
	httpClient  *http.Client
	log         *zerolog.Logger
	username    string
	password    string
	certMu      sync.Mutex // Serializes certificate creation to avoid key conflicts
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

// Login authenticates and stores credentials
func (c *Client) Login(username, password string) error {
	// Store credentials
	if err := c.store.SetCredentials(username, password); err != nil {
		return fmt.Errorf("failed to store credentials: %w", err)
	}

	// Perform SRP authentication
	session, err := c.auth.Login()
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
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
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	req.Header.Set("x-pm-appversion", "web-vpn-settings@5.0.353.0")
	req.Header.Set("x-pm-locale", "en_US")
	req.Header.Set("x-pm-uid", session.UID)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")
	for _, cookie := range session.Cookies {
		req.AddCookie(cookie)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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

	// Ensure session
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
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	req.Header.Set("x-pm-appversion", "web-vpn-settings@5.0.353.0")
	req.Header.Set("x-pm-locale", "en_US")
	req.Header.Set("x-pm-uid", session.UID)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")
	for _, c := range session.Cookies {
		req.AddCookie(c)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	c.log.Debug().Int("status", resp.StatusCode).Int("body_len", len(body)).Msg("Server list response")

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

// SelectServer selects the best server for a region
func (c *Client) SelectServer(region string, bannedRegions map[string]bool) (*LogicalServer, *Server, error) {
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

	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no available servers for region %s", region)
	}

	// Select based on score and load
	best := candidates[0]
	for _, c := range candidates[1:] {
		// Prefer higher score, lower load
		if c.logical.Score > best.logical.Score ||
			(c.logical.Score == best.logical.Score && c.logical.Load < best.logical.Load) {
			best = c
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

// EnsureSession ensures the session is valid, refreshing if needed
func (c *Client) EnsureSession() error {
	session, err := c.store.GetSession()
	if err != nil {
		// No session, need to login
		if c.username == "" || c.password == "" {
			storedUser, storedPass, credErr := c.store.GetCredentials()
			if credErr != nil {
				return fmt.Errorf("no credentials stored and no credentials provided")
			}
			return c.Login(storedUser, storedPass)
		}
		return c.Login(c.username, c.password)
	}

	// Check if session is expired (with 5 minute buffer)
	if time.Now().Add(5 * time.Minute).After(session.ExpiresAt) {
		c.log.Info().Msg("Session expired, refreshing")
		if c.username == "" || c.password == "" {
			storedUser, storedPass, err := c.store.GetCredentials()
			if err != nil {
				return fmt.Errorf("no credentials stored for refresh and no credentials provided")
			}
			return c.Login(storedUser, storedPass)
		}
		return c.Login(c.username, c.password)
	}

	return nil
}
