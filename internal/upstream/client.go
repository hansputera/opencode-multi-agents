package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/proxy"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	proxypkg "github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

// Client handles requests to upstream providers
type Client struct {
	cfg       *config.Config
	log       *zerolog.Logger
	httpCache map[string]*http.Client // Cache clients per proxy
}

// NewClient creates a new upstream client
func NewClient(cfg *config.Config, log *zerolog.Logger) *Client {
	return &Client{
		cfg:       cfg,
		log:       log,
		httpCache: make(map[string]*http.Client),
	}
}

// Do sends a request to the upstream provider through the given proxy
// Returns response, whether it was rate limited, and error
func (c *Client) Do(ctx context.Context, p *proxypkg.Proxy, body []byte, apiKey string, stream bool) (*http.Response, bool, error) {
	// Get or create HTTP client for this proxy
	client, err := c.getClient(p)
	if err != nil {
		return nil, false, err
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.UpstreamBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	// Send request
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}

	// Check for rate limit
	if resp.StatusCode == 429 {
		resp.Body.Close()
		return nil, true, fmt.Errorf("rate limited (429)")
	}

	// Check for rate limit in error body for some providers
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		bodyStr := strings.ToLower(string(bodyBytes))
		if isRateLimitError(resp.StatusCode, bodyStr) {
			return nil, true, fmt.Errorf("rate limited: %s", bodyStr)
		}

		// Return as normal error response
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, false, nil
	}

	return resp, false, nil
}

// getClient returns an HTTP client configured with the given proxy
func (c *Client) getClient(p *proxypkg.Proxy) (*http.Client, error) {
	// Check cache
	if client, ok := c.httpCache[p.ID]; ok {
		return client, nil
	}

	// Parse SOCKS5 address
	// Format: socks5://127.0.0.1:10801
	addr := strings.TrimPrefix(p.SOCKS5Addr, "socks5://")

	// Create SOCKS5 dialer
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	// Create HTTP client with SOCKS5 transport
	transport := &http.Transport{
		Dial: dialer.Dial,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   c.cfg.RequestTimeout,
	}

	// Cache for reuse
	c.httpCache[p.ID] = client

	return client, nil
}

// isRateLimitError checks if the response indicates rate limiting
func isRateLimitError(statusCode int, body string) bool {
	if statusCode == 429 {
		return true
	}

	// Check for common rate limit messages
	keywords := []string{
		"rate limit",
		"rate_limit",
		"too many requests",
		"quota exceeded",
		"quota_exceeded",
		"limit exceeded",
		"limit_exceeded",
		"requests per",
		"try again later",
		"slow down",
	}

	bodyLower := strings.ToLower(body)
	for _, kw := range keywords {
		if strings.Contains(bodyLower, kw) {
			return true
		}
	}

	return false
}

// ErrorResponse represents an error from the upstream
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// ParseError parses an error response from upstream
func ParseError(body []byte) string {
	var errResp ErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil {
		return errResp.Error.Message
	}
	return string(body)
}
