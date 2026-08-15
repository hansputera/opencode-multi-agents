package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/rs/zerolog"
)

var (
	ErrNoProxyAvailable = errors.New("no proxy available")
	ErrPoolClosed       = errors.New("pool is closed")
)

// PoolManager manages a pool of WARP proxy containers
type PoolManager struct {
	cfg    *config.Config
	log    *zerolog.Logger
	mgr    Manager
	mu     sync.RWMutex
	pool   map[string]*Proxy
	sticky map[string]string // conversation_id -> proxy_id

	// Signals
	available chan struct{}
	done      chan struct{}

	// Metrics
	totalRequests int
	totalErrors   int
}

// NewPoolManager creates a new proxy pool manager
func NewPoolManager(cfg *config.Config, log *zerolog.Logger) (*PoolManager, error) {
	mgr, err := NewDockerManager(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker manager: %w", err)
	}

	return &PoolManager{
		cfg:       cfg,
		log:       log,
		mgr:       mgr,
		pool:      make(map[string]*Proxy),
		sticky:    make(map[string]string),
		available: make(chan struct{}, cfg.ProxyPoolSize),
		done:      make(chan struct{}),
	}, nil
}

// Start initializes the proxy pool with the configured number of proxies
func (pm *PoolManager) Start(ctx context.Context) error {
	pm.log.Info().Int("pool_size", pm.cfg.ProxyPoolSize).Msg("Starting proxy pool")

	for i := 0; i < pm.cfg.ProxyPoolSize; i++ {
		if err := pm.createProxy(ctx); err != nil {
			pm.log.Error().Err(err).Int("index", i).Msg("Failed to create proxy")
			// Continue trying to create remaining proxies
		}
	}

	// Start background health check
	go pm.healthCheckLoop(ctx)

	// Start cooldown restoration
	go pm.cooldownLoop(ctx)

	pm.log.Info().Int("created", len(pm.pool)).Msg("Proxy pool started")

	return nil
}

// createProxy creates a new proxy container and adds it to the pool
func (pm *PoolManager) createProxy(ctx context.Context) error {
	proxy, err := pm.mgr.Create(ctx)
	if err != nil {
		return err
	}

	pm.mu.Lock()
	pm.pool[proxy.ID] = proxy
	pm.mu.Unlock()

	pm.log.Info().
		Str("id", proxy.ID).
		Str("socks5", proxy.SOCKS5Addr).
		Msg("Created new proxy container")

	// Signal availability
	select {
	case pm.available <- struct{}{}:
	default:
	}

	return nil
}

// GetProxy returns an available proxy, blocking if none available
func (pm *PoolManager) GetProxy(ctx context.Context, conversationID string) (*Proxy, error) {
	// Check for sticky session first
	if conversationID != "" && pm.cfg.StickySessionTTL > 0 {
		pm.mu.RLock()
		proxyID, ok := pm.sticky[conversationID]
		if ok {
			proxy, exists := pm.pool[proxyID]
			if exists && proxy.IsHealthy() {
				pm.mu.RUnlock()
				return proxy, nil
			}
		}
		pm.mu.RUnlock()
	}

	// Wait for available proxy
	for {
		pm.mu.RLock()
		for _, proxy := range pm.pool {
			if proxy.IsAvailable() {
				proxy.State = StateActive
				proxy.LastUsed = time.Now()
				proxy.RequestsSent++
				pm.totalRequests++

				// Set sticky session
				if conversationID != "" {
					pm.sticky[conversationID] = proxy.ID
				}

				pm.mu.RUnlock()
				pm.log.Debug().Str("proxy_id", proxy.ID).Msg("Acquired proxy")
				return proxy, nil
			}
		}
		pm.mu.RUnlock()

		// No proxy available, wait
		select {
		case <-pm.available:
			// Check again
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pm.done:
			return nil, ErrPoolClosed
		}
	}
}

// ReleaseProxy returns a proxy to the pool
func (pm *PoolManager) ReleaseProxy(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pr, exists := pm.pool[proxy.ID]; exists {
		if pr.State == StateActive {
			pr.State = StateIdle
			pm.log.Debug().Str("proxy_id", pr.ID).Msg("Released proxy")
		}
	}

	// Signal availability
	select {
	case pm.available <- struct{}{}:
	default:
	}
}

// MarkRateLimited marks a proxy as rate limited and moves it to cooldown
func (pm *PoolManager) MarkRateLimited(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pr, exists := pm.pool[proxy.ID]; exists {
		pr.State = StateCooldown
		pr.LastUsed = time.Now()
		pr.ErrorCount++
		pm.totalErrors++

		pm.log.Warn().
			Str("proxy_id", pr.ID).
			Dur("cooldown", pm.cfg.CooldownDuration).
			Msg("Proxy rate limited, moving to cooldown")
	}

	// Try to create a new proxy if needed
	go func() {
		pm.mu.RLock()
		active := 0
		for _, pr := range pm.pool {
			if pr.IsHealthy() {
				active++
			}
		}
		pm.mu.RUnlock()

		if active < pm.cfg.ProxyPoolSize {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := pm.createProxy(ctx); err != nil {
				pm.log.Error().Err(err).Msg("Failed to create replacement proxy")
			}
		}
	}()
}

// MarkUnhealthy marks a proxy as unhealthy
func (pm *PoolManager) MarkUnhealthy(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pr, exists := pm.pool[proxy.ID]; exists {
		pr.State = StateUnhealthy
		pr.ErrorCount++
		pm.totalErrors++

		pm.log.Warn().
			Str("proxy_id", pr.ID).
			Int("error_count", pr.ErrorCount).
			Msg("Proxy marked unhealthy")
	}
}

// RemoveProxy removes a proxy from the pool
func (pm *PoolManager) RemoveProxy(ctx context.Context, proxyID string) {
	pm.mu.Lock()
	proxy, exists := pm.pool[proxyID]
	if !exists {
		pm.mu.Unlock()
		return
	}
	delete(pm.pool, proxyID)
	pm.mu.Unlock()

	// Remove container
	if err := pm.mgr.Remove(ctx, proxy.ContainerID); err != nil {
		pm.log.Error().Err(err).Str("container_id", proxy.ContainerID).Msg("Failed to remove container")
	}

	pm.log.Info().Str("proxy_id", proxyID).Msg("Removed proxy")
}

// Stats returns current pool statistics
func (pm *PoolManager) Stats() PoolStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	stats := PoolStats{
		Total:         len(pm.pool),
		TotalRequests: pm.totalRequests,
		TotalErrors:   pm.totalErrors,
	}

	for _, proxy := range pm.pool {
		switch proxy.State {
		case StateIdle:
			stats.Idle++
		case StateActive:
			stats.Active++
		case StateCooldown:
			stats.Cooldown++
		case StateUnhealthy:
			stats.Unhealthy++
		}
	}

	return stats
}

// healthCheckLoop periodically checks proxy health
func (pm *PoolManager) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(pm.cfg.HealthCheckPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.checkAllProxies(ctx)
		case <-ctx.Done():
			return
		case <-pm.done:
			return
		}
	}
}

// checkAllProxies performs health check on all proxies
func (pm *PoolManager) checkAllProxies(ctx context.Context) {
	pm.mu.RLock()
	proxies := make([]*Proxy, 0, len(pm.pool))
	for _, proxy := range pm.pool {
		proxies = append(proxies, proxy)
	}
	pm.mu.RUnlock()

	for _, proxy := range proxies {
		healthy, err := pm.mgr.HealthCheck(ctx, proxy)
		if err != nil {
			pm.log.Debug().Err(err).Str("proxy_id", proxy.ID).Msg("Health check failed")
		}

		pm.mu.Lock()
		if pr, exists := pm.pool[proxy.ID]; exists {
			pr.LastCheck = time.Now()
			if !healthy && pr.State != StateCooldown {
				pr.State = StateUnhealthy
				pm.log.Warn().Str("proxy_id", proxy.ID).Msg("Proxy unhealthy after health check")
			} else if healthy && pr.State == StateUnhealthy {
				pr.State = StateIdle
				pm.log.Info().Str("proxy_id", proxy.ID).Msg("Proxy recovered")
				// Signal availability
				select {
				case pm.available <- struct{}{}:
				default:
				}
			}
		}
		pm.mu.Unlock()
	}
}

// cooldownLoop periodically restores proxies from cooldown
func (pm *PoolManager) cooldownLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.restoreCooldownProxies()
		case <-ctx.Done():
			return
		case <-pm.done:
			return
		}
	}
}

// restoreCooldownProxies moves expired cooldown proxies back to idle
func (pm *PoolManager) restoreCooldownProxies() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	for _, proxy := range pm.pool {
		if proxy.State == StateCooldown {
			if now.Sub(proxy.LastUsed) >= pm.cfg.CooldownDuration {
				proxy.State = StateIdle
				proxy.ErrorCount = 0
				pm.log.Info().Str("proxy_id", proxy.ID).Msg("Proxy restored from cooldown")

				// Signal availability
				select {
				case pm.available <- struct{}{}:
				default:
				}
			}
		}
	}
}

// Close shuts down the pool manager
func (pm *PoolManager) Close() error {
	close(pm.done)

	pm.mu.Lock()
	proxies := make([]*Proxy, 0, len(pm.pool))
	for _, proxy := range pm.pool {
		proxies = append(proxies, proxy)
	}
	pm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, proxy := range proxies {
		if err := pm.mgr.Remove(ctx, proxy.ContainerID); err != nil {
			pm.log.Error().Err(err).Str("container_id", proxy.ContainerID).Msg("Failed to remove container during shutdown")
		}
	}

	return pm.mgr.Close()
}
