package proxy

import (
	"context"
	"encoding/binary"
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

// parseSOCKS5Addr parses a socks5:// or socks5h:// address into its components.
// Returns the proxy host, optional auth, and whether remote DNS (socks5h) is requested.
func parseSOCKS5Addr(raw string) (host string, auth *proxy.Auth, remoteDNS bool) {
	remoteDNS = strings.HasPrefix(raw, "socks5h://")
	raw = strings.TrimPrefix(raw, "socks5h://")
	raw = strings.TrimPrefix(raw, "socks5://")

	if idx := strings.LastIndex(raw, "@"); idx != -1 {
		cred := raw[:idx]
		host = raw[idx+1:]
		if u, pass, ok := strings.Cut(cred, ":"); ok {
			auth = &proxy.Auth{User: u, Password: pass}
		}
	} else {
		host = raw
	}
	return
}

// socks5hDialer implements proxy.Dialer for socks5h:// (remote DNS resolution).
// It connects to the SOCKS5 proxy and sends a CONNECT with the hostname
// (not an IP), so the proxy resolves DNS remotely.
type socks5hDialer struct {
	proxyAddr string
	auth      *proxy.Auth
}

func (d *socks5hDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *socks5hDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// Connect to the SOCKS5 proxy
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5h: connect to proxy %s: %w", d.proxyAddr, err)
	}

	// SOCKS5 handshake
	if err := socks5Handshake(conn, d.auth, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// socks5Handshake performs the SOCKS5 protocol handshake and CONNECT.
// targetAddr is passed as-is (hostname:port) so the proxy resolves DNS.
func socks5Handshake(conn net.Conn, auth *proxy.Auth, targetAddr string) error {
	// Step 1: greeting
	greeting := []byte{0x05} // SOCKS5
	if auth != nil {
		greeting = append(greeting, 0x02, 0x00, 0x02) // 2 methods: no-auth, user/pass
	} else {
		greeting = append(greeting, 0x01, 0x00) // 1 method: no-auth
	}
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5h: write greeting: %w", err)
	}

	// Step 2: read server choice
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5h: read server choice: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5h: unsupported SOCKS version: %d", resp[0])
	}

	// Step 3: auth negotiation
	switch resp[1] {
	case 0x02: // user/pass auth
		if auth == nil {
			return fmt.Errorf("socks5h: proxy requires auth but none provided")
		}
		if err := socks5Auth(conn, auth); err != nil {
			return err
		}
	case 0x00: // no auth
	default:
		return fmt.Errorf("socks5h: unsupported auth method: %d", resp[1])
	}

	// Step 4: CONNECT request
	// Parse host and port
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fmt.Errorf("socks5h: parse target addr: %w", err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return fmt.Errorf("socks5h: lookup port: %w", err)
	}

	// Build CONNECT request: VER=5 CMD=1(CONNECT) RSV=0 ATYP=3(DOMAIN)
	req := []byte{0x05, 0x01, 0x00, 0x03}
	// Domain name
	req = append(req, byte(len(host)))
	req = append(req, []byte(host)...)
	// Port (big-endian)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5h: write CONNECT: %w", err)
	}

	// Step 5: read CONNECT response
	connectResp := make([]byte, 4)
	if _, err := io.ReadFull(conn, connectResp); err != nil {
		return fmt.Errorf("socks5h: read CONNECT response: %w", err)
	}
	if connectResp[1] != 0x00 {
		return fmt.Errorf("socks5h: CONNECT failed with code: %d", connectResp[1])
	}

	// Read remaining response (bind address)
	switch connectResp[3] {
	case 0x01: // IPv4
		io.CopyN(io.Discard, conn, 4+2)
	case 0x03: // Domain
		domainLen := make([]byte, 1)
		io.ReadFull(conn, domainLen)
		io.CopyN(io.Discard, conn, int64(domainLen[0])+2)
	case 0x04: // IPv6
		io.CopyN(io.Discard, conn, 16+2)
	}

	return nil
}

// socks5Auth performs username/password authentication (RFC 1929).
func socks5Auth(conn net.Conn, auth *proxy.Auth) error {
	// VER=1 ULEN PL UNAME PLEN PASSWD
	req := []byte{0x01}
	req = append(req, byte(len(auth.User)))
	req = append(req, []byte(auth.User)...)
	req = append(req, byte(len(auth.Password)))
	req = append(req, []byte(auth.Password)...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5h: write auth: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5h: read auth response: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5h: auth failed with code: %d", resp[1])
	}
	return nil
}

// HealthCheck probes the external SOCKS5 proxy by dialing through it to the
// IP check endpoint, then records the egress IP.
func (m *ExternalProxyManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) {
	proxyHost, auth, remoteDNS := parseSOCKS5Addr(p.SOCKS5Addr)

	var dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

	if remoteDNS {
		// socks5h:// — proxy resolves DNS remotely
		d := &socks5hDialer{proxyAddr: proxyHost, auth: auth}
		dialFunc = d.DialContext
	} else {
		// socks5:// — local DNS resolution via standard SOCKS5
		dialer, err := proxy.SOCKS5("tcp", proxyHost, auth, proxy.Direct)
		if err != nil {
			return false, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
		}
		dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}

	transport := &http.Transport{
		DialContext:           dialFunc,
		MaxIdleConns:          1,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", m.ipCheckURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("health check failed for %s: %w", proxyHost, err)
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
