package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/proxy"
)

// ExternalProxyManager manages external SOCKS5 proxies that coexist with
// ProtonVPN containers in the same pool. External proxies have no Docker
// containers — they are pre-existing SOCKS5 endpoints.
type ExternalProxyManager struct {
	addrs      []string
	ipCheckURL string
	log        *zerolog.Logger
	nextID     atomic.Int64
}

// NewExternalProxyManager creates a manager for external SOCKS5 proxies.
func NewExternalProxyManager(addrs []string, ipCheckURL string, log *zerolog.Logger) *ExternalProxyManager {
	if ipCheckURL == "" {
		ipCheckURL = "https://icanhazip.com/"
	}
	return &ExternalProxyManager{
		addrs:      addrs,
		ipCheckURL: ipCheckURL,
		log:        log,
	}
}

// Create returns the next external SOCKS5 proxy round-robin.
func (m *ExternalProxyManager) Create(ctx context.Context) (*Proxy, error) {
	return m.CreateEx(ctx, nil, nil)
}

// CreateEx returns the next external SOCKS5 proxy. bannedRegions and
// avoidServers are ignored (external proxies have no region/server metadata).
func (m *ExternalProxyManager) CreateEx(_ context.Context, _, _ map[string]bool) (*Proxy, error) {
	if len(m.addrs) == 0 {
		return nil, fmt.Errorf("no external SOCKS5 proxies configured")
	}

	idx := m.nextID.Add(1) - 1
	addr := m.addrs[idx%int64(len(m.addrs))]

	id := fmt.Sprintf("ext-%d", idx+1)
	return &Proxy{
		ID:         id,
		ContainerID: "", // no container
		SOCKS5Addr: addr,
		Port:       0, // not a host-mapped port
		State:      StateIdle,
		CreatedAt:  time.Now(),
	}, nil
}

// Remove is a no-op for external proxies (no container to destroy).
func (m *ExternalProxyManager) Remove(_ context.Context, _ string) error {
	return nil
}

// HealthCheck probes the external SOCKS5 proxy by dialing through it to the
// IP check endpoint, then records the egress IP.
func (m *ExternalProxyManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) {
	addr := strings.TrimPrefix(p.SOCKS5Addr, "socks5://")
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		return false, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	transport := &http.Transport{Dial: dialer.Dial}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", m.ipCheckURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return false, fmt.Errorf("invalid IP response: %s", ip)
	}

	p.EgressIP = ip
	return true, nil
}

// Close is a no-op for external proxies.
func (m *ExternalProxyManager) Close() error {
	return nil
}

// Exec returns an error — external SOCKS5 proxies have no container to exec into.
func (m *ExternalProxyManager) Exec(_ context.Context, _ string, _, _ []string) ([]byte, error) {
	return nil, fmt.Errorf("exec not supported for external SOCKS5 proxies")
}
