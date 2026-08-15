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
	base      string
	log       *zerolog.Logger
	httpCache map[string]*http.Client // Cache clients per proxy
}

// NewClient creates a new upstream client
func NewClient(cfg *config.Config, log *zerolog.Logger) *Client {
	return &Client{
		cfg:       cfg,
		base:      normalizeBase(cfg.UpstreamBaseURL),
		log:       log,
		httpCache: make(map[string]*http.Client),
	}
}

// normalizeBase ensures the upstream base URL ends with /v1 so that
// endpoint paths can be appended directly. Accepts both styles:
// "https://openrouter.ai/api" and "https://opencode.ai/zen/v1".
func normalizeBase(base string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		return trimmed + "/v1"
	}
	return trimmed
}

// Auth carries the upstream authentication header to send.
type Auth struct {
	Header string // e.g. "Authorization" or "x-api-key"
	Value  string // full header value, e.g. "Bearer zent-x" or "public"
}

// RateLimit describes an upstream rate-limit signal. RetryAfter carries the
// upstream's Retry-After header value ("" when absent), so the pool can ban
// the egress IP for at least as long as the provider asked us to wait.
type RateLimit struct {
	RetryAfter string
}

// Upstream is the contract every upstream driver (Zen/OpenAI or OpenCode Server)
// must implement. The handler selects the driver via config.UpstreamProvider.
type Upstream interface {
	Do(ctx context.Context, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*http.Response, *RateLimit, error)
	DoModels(ctx context.Context, p *proxypkg.Proxy, auth Auth) (*http.Response, *RateLimit, error)
}

// Do sends a request to the upstream provider through the given proxy
// Returns response, whether it was rate limited, and error
func (c *Client) Do(ctx context.Context, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*http.Response, *RateLimit, error) {
	// Get or create HTTP client for this proxy
	client, err := c.getClient(p)
	if err != nil {
		return nil, nil, err
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if auth.Value != "" {
		req.Header.Set(auth.Header, auth.Value)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	// Send request
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// Check for rate limit
	if resp.StatusCode == 429 {
		rl := &RateLimit{RetryAfter: resp.Header.Get("Retry-After")}
		resp.Body.Close()
		return nil, rl, fmt.Errorf("rate limited (429)")
	}

	// Check for rate limit in error body for some providers
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		bodyStr := strings.ToLower(string(bodyBytes))
		if isRateLimitError(resp.StatusCode, bodyStr) {
			return nil, &RateLimit{RetryAfter: resp.Header.Get("Retry-After")}, fmt.Errorf("rate limited: %s", bodyStr)
		}

		// Return as normal error response
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, nil, nil
	}

	return resp, nil, nil
}

// DoModels fetches the upstream model list through the given proxy.
// Returns the response, whether it was rate limited, and an error.
func (c *Client) DoModels(ctx context.Context, p *proxypkg.Proxy, auth Auth) (*http.Response, *RateLimit, error) {
	client, err := c.getClient(p)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/models", nil)
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Accept", "application/json")
	if auth.Value != "" {
		req.Header.Set(auth.Header, auth.Value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// Check for rate limit
	if resp.StatusCode == 429 {
		rl := &RateLimit{RetryAfter: resp.Header.Get("Retry-After")}
		resp.Body.Close()
		return nil, rl, fmt.Errorf("rate limited (429)")
	}

	// Check for rate limit in error body for some providers
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		bodyStr := strings.ToLower(string(bodyBytes))
		if isRateLimitError(resp.StatusCode, bodyStr) {
			return nil, &RateLimit{RetryAfter: resp.Header.Get("Retry-After")}, fmt.Errorf("rate limited: %s", bodyStr)
		}

		// Return as normal error response
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, nil, nil
	}

	return resp, nil, nil
}

// getClient returns an HTTP client configured with the given proxy
func (c *Client) getClient(p *proxypkg.Proxy) (*http.Client, error) {
	// Check cache
	if client, ok := c.httpCache[p.ID]; ok {
		return client, nil
	}

	// Parse SOCKS5 address
	// Format: socks5://127.0.0.1:10801
	dialer, err := NewSOCKS5Dialer(p.SOCKS5Addr)
	if err != nil {
		return nil, err
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

// NewSOCKS5Dialer builds a SOCKS5 dialer (no-auth) from a "socks5://host:port"
// address. Shared by the Zen client and the OpenCode client so every upstream
// request is egressed through the assigned proxy container's WARP IP.
func NewSOCKS5Dialer(socks5Addr string) (proxy.Dialer, error) {
	addr := strings.TrimPrefix(socks5Addr, "socks5://")
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}
	return dialer, nil
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
