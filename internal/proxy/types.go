package proxy

import (
	"context"
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

// Proxy represents a single WARP container proxy
type Proxy struct {
	ID           string       `json:"id"`
	ContainerID  string       `json:"container_id"`
	SOCKS5Addr   string       `json:"socks5_addr"`
	Port         int          `json:"port"`
	State        ProxyState   `json:"state"`
	LastUsed     time.Time    `json:"last_used"`
	LastCheck    time.Time    `json:"last_check"`
	RequestsSent int          `json:"requests_sent"`
	ErrorCount   int          `json:"error_count"`
	CreatedAt    time.Time    `json:"created_at"`
	EgressIP     string       `json:"egress_ip"`
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
	// Remove removes a proxy container
	Remove(ctx context.Context, id string) error
	// HealthCheck performs health check on a proxy
	HealthCheck(ctx context.Context, proxy *Proxy) (bool, error)
	// Close cleans up all resources
	Close() error
}
