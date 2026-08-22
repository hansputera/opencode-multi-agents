package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// ProxyState represents the current state of a proxy
type ProxyState string

const (
	StateIdle      ProxyState = "idle"
	StateActive    ProxyState = "active"
	StateCooldown  ProxyState = "cooldown"
	StateUnhealthy ProxyState = "unhealthy"
)

// Proxy represents a single VPN container proxy
type Proxy struct {
	ID           string       `json:"-"`
	ContainerID  string       `json:"-"`
	SOCKS5Addr   string       `json:"-"`
	Port         int          `json:"port"`
	State        ProxyState   `json:"state"`
	LastUsed     time.Time    `json:"last_used"`
	LastCheck    time.Time    `json:"last_check"`
	RequestsSent int          `json:"requests_sent"`
	ErrorCount   int          `json:"error_count"`
	CreatedAt    time.Time    `json:"created_at"`
	EgressIP     string       `json:"egress_ip"`
	Region       string       `json:"region"`
	ServerName   string       `json:"server_name"`
	ServerIP     string       `json:"server_ip"`

	// manager is the container manager that created this proxy, used by
	// ExecIn to docker-exec commands inside the container. Wired by the pool.
	manager atomic.Pointer[Manager]
}

// ProxySnapshot is a sanitized view of Proxy safe for API responses.
type ProxySnapshot struct {
	ID           string       `json:"id"`
	SOCKS5Addr   string       `json:"socks5_addr"`
	Port         int          `json:"port"`
	State        ProxyState   `json:"state"`
	LastUsed     time.Time    `json:"last_used"`
	LastCheck    time.Time    `json:"last_check"`
	RequestsSent int          `json:"requests_sent"`
	ErrorCount   int          `json:"error_count"`
	CreatedAt    time.Time    `json:"created_at"`
	EgressIP     string       `json:"egress_ip"`
	Region       string       `json:"region"`
	ServerName   string       `json:"server_name"`
	ServerIP     string       `json:"server_ip"`
}

// Snapshot returns a sanitized copy safe for API responses.
func (p *Proxy) Snapshot() ProxySnapshot {
	return ProxySnapshot{
		ID:           p.ID,
		SOCKS5Addr:   p.SOCKS5Addr,
		Port:         p.Port,
		State:        p.State,
		LastUsed:     p.LastUsed,
		LastCheck:    p.LastCheck,
		RequestsSent: p.RequestsSent,
		ErrorCount:   p.ErrorCount,
		CreatedAt:    p.CreatedAt,
		EgressIP:     p.EgressIP,
		Region:       p.Region,
		ServerName:   p.ServerName,
		ServerIP:     p.ServerIP,
	}
}

// IsHealthy returns true if the proxy is in a healthy state
func (p *Proxy) IsHealthy() bool {
	return p.State == StateIdle || p.State == StateActive
}

// IsAvailable returns true if the proxy can be used for requests
func (p *Proxy) IsAvailable() bool {
	return p.State == StateIdle
}

// PoolStats holds statistics about the proxy pool
type PoolStats struct {
	Total         int `json:"total"`
	Idle          int `json:"idle"`
	Active        int `json:"active"`
	Cooldown      int `json:"cooldown"`
	Unhealthy     int `json:"unhealthy"`
	TotalRequests int `json:"total_requests"`
	TotalErrors   int `json:"total_errors"`
}

// Manager defines the interface for managing proxy containers
type Manager interface {
	// Create creates a new proxy container
	Create(ctx context.Context) (*Proxy, error)
	// CreateEx creates a new proxy container, avoiding the given banned regions
	// and servers. avoidServers spreads containers across different servers for
	// diverse exit IPs.
	CreateEx(ctx context.Context, bannedRegions, avoidServers map[string]bool) (*Proxy, error)
	// Remove removes a proxy container
	Remove(ctx context.Context, id string) error
	// HealthCheck performs health check on a proxy
	HealthCheck(ctx context.Context, proxy *Proxy) (bool, error)
	// Close cleans up all resources
	Close() error
	// Exec runs a command inside a proxy container and returns the output.
	Exec(ctx context.Context, containerID string, env, args []string) ([]byte, error)
}

// ExecIn runs a command inside a proxy container via its manager.
func (p *Proxy) ExecIn(ctx context.Context, env, args []string) ([]byte, error) {
	mgr := p.manager.Load()
	if mgr == nil {
		return nil, fmt.Errorf("proxy %s has no manager", p.ID)
	}
	return (*mgr).Exec(ctx, p.ContainerID, env, args)
}

// SetManager wires the container manager used by ExecIn. The pool calls this
// when a proxy is created; tests use it to inject fakes.
func (p *Proxy) SetManager(mgr Manager) {
	p.manager.Store(&mgr)
}
