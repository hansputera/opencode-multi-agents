package protonvpn

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/curve25519"
)

// Client handles ProtonVPN API operations
type Client struct {
	store      *Store
	auth       *SRPAuth
	httpClient *http.Client
	log        *zerolog.Logger
}

// NewClient creates a new ProtonVPN client
func NewClient(store *Store, apiBase, username, password string, log *zerolog.Logger) *Client {
	return &Client{
		store: store,
		auth:  NewSRPAuth(username, password, apiBase, log),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log: log,
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
func (c *Client) GetCertificate() (*CertificateResponse, error) {
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

	// Get WireGuard public key
	_, publicKey, err := c.GetOrCreateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to get WireGuard keypair: %w", err)
	}

	// Request certificate
	cert, err = c.requestCertificate(session, publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to request certificate: %w", err)
	}

	// Store certificate
	if err := c.store.SetCertificate(cert); err != nil {
		return nil, fmt.Errorf("failed to store certificate: %w", err)
	}

	return cert, nil
}

// requestCertificate requests a new certificate from the API
func (c *Client) requestCertificate(session *Session, clientPublicKey string) (*CertificateResponse, error) {
	reqBody := map[string]interface{}{
		"ClientPublicKey": clientPublicKey,
		"Mode":            "1",
		"Features":        []string{"netshield", "port_forwarding"},
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
		return cached, nil
	}

	// Fetch from API
	req, err := http.NewRequest("GET", c.auth.apiBase+"/api/vpn/v2/logicals", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
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

	var serverList ServerListResponse
	if err := json.Unmarshal(body, &serverList); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
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

		// Skip if server is offline
		if logical.Status != 1 {
			continue
		}

		// Find a suitable physical server
		for j := range logical.Servers {
			server := &logical.Servers[j]
			if server.Status == 1 && server.X25519PublicKey != "" {
				candidates = append(candidates, struct {
					logical *LogicalServer
					server  *Server
				}{logical, server})
			}
		}
	}

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
		Str("ip", best.server.ExitIP).
		Msg("Selected VPN server")

	return best.logical, best.server, nil
}

// GetOrCreateKeyPair gets or creates a WireGuard X25519 keypair
func (c *Client) GetOrCreateKeyPair() (privateKey, publicKey string, err error) {
	// Try to get existing keys
	privateKey, publicKey, err = c.store.GetWireGuardKey()
	if err == nil {
		return privateKey, publicKey, nil
	}

	// Generate new keypair
	privateKeyBytes := make([]byte, 32)
	if _, err := rand.Read(privateKeyBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate private key: %w", err)
	}

	publicKeyBytes, err := curve25519.X25519(privateKeyBytes, curve25519.Basepoint)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate public key: %w", err)
	}

	privateKey = base64.StdEncoding.EncodeToString(privateKeyBytes)
	publicKey = base64.StdEncoding.EncodeToString(publicKeyBytes)

	// Store the keys
	if err := c.store.SetWireGuardKey(privateKey, publicKey); err != nil {
		return "", "", fmt.Errorf("failed to store wireguard keys: %w", err)
	}

	return privateKey, publicKey, nil
}

// EnsureSession ensures the session is valid, refreshing if needed
func (c *Client) EnsureSession() error {
	session, err := c.store.GetSession()
	if err != nil {
		// No session, need to login
		credentials, _, err := c.store.GetCredentials()
		if err != nil {
			return fmt.Errorf("no credentials stored, please login first")
		}
		return c.Login(credentials, credentials)
	}

	// Check if session is expired (with 5 minute buffer)
	if time.Now().Add(5 * time.Minute).After(session.ExpiresAt) {
		c.log.Info().Msg("Session expired, refreshing")
		credentials, _, err := c.store.GetCredentials()
		if err != nil {
			return fmt.Errorf("no credentials stored for refresh")
		}
		return c.Login(credentials, credentials)
	}

	return nil
}
