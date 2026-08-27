package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/rs/zerolog"
)

var (
	ErrNoProxyAvailable = errors.New("no proxy available")
	ErrPoolClosed       = errors.New("pool is closed")
)

// PoolManager manages a pool of VPN proxy containers
type PoolManager struct {
	cfg    *config.Config
	log    *zerolog.Logger
	mgr    Manager
	mu     sync.RWMutex
	pool   map[string]*Proxy
	sticky map[string]string // conversation_id -> proxy_id

	// ipBans maps egress IPs to the time until which they are excluded from
	// selection (upstream rate limited them). Guarded by mu.
	ipBans map[string]time.Time

	// regionBans maps VPN regions to the time until which they are excluded.
	// When a region is rate-limited, all its IPs are banned and new containers
	// are created from a different region. Guarded by mu.
	regionBans map[string]time.Time

	// usedServers tracks logical server names currently in use by pool proxies
	// so new containers are spread across different servers for diverse exit IPs.
	usedServers map[string]bool

	// spawning counts replacement containers currently being created by
	// ensurePoolCapacity (guarded by mu) so concurrent triggers never spawn
	// more than the deficit.
	spawning int

	// rr is the round-robin cursor for request dispatch (guarded by mu).
	rr int

	// Signals
	available chan struct{}
	done      chan struct{}

	// Metrics (atomic for lock-free reads)
	totalRequests atomic.Int64
	totalErrors   atomic.Int64
}

// NewPoolManager creates a new proxy pool manager
func NewPoolManager(cfg *config.Config, log *zerolog.Logger) (*PoolManager, error) {
	mgr, err := NewDockerManager(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker manager: %w", err)
	}
	return NewPoolManagerWithManager(mgr, cfg, log), nil
}

// NewPoolManagerWithManager creates a proxy pool manager with a custom manager
func NewPoolManagerWithManager(mgr Manager, cfg *config.Config, log *zerolog.Logger) *PoolManager {
	return &PoolManager{
		cfg:         cfg,
		log:         log,
		mgr:         mgr,
		pool:        make(map[string]*Proxy),
		sticky:      make(map[string]string),
		ipBans:      make(map[string]time.Time),
		regionBans:  make(map[string]time.Time),
		usedServers: make(map[string]bool),
		available:   make(chan struct{}, cfg.ProxyPoolSize),
		done:        make(chan struct{}),
	}
}

// Start initializes the proxy pool with the configured number of proxies
func (pm *PoolManager) Start(ctx context.Context) error {
	pm.log.Info().Int("pool_size", pm.cfg.ProxyPoolSize).Msg("Starting proxy pool")

	// Create proxies concurrently: a single VPN first-boot can take tens of
	// seconds (plus retry backoffs on failure), so creating them sequentially
	// would keep the pool empty for minutes. The Docker manager is safe for
	// concurrent use (atomic port allocator, mutex-guarded image ensure).
	var wg sync.WaitGroup
	for i := 0; i < pm.cfg.ProxyPoolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pm.createProxy(ctx); err != nil {
				pm.log.Error().Err(err).Msg("Failed to create proxy")
				// Continue trying to create remaining proxies
			}
		}()
	}
	wg.Wait()

	// Start background health check
	go pm.healthCheckLoop(ctx)

	// Start cooldown restoration
	go pm.cooldownLoop(ctx)

	pm.log.Info().Int("created", len(pm.pool)).Msg("Proxy pool started")

	return nil
}

// duplicateIPLocked reports whether p's egress IP is already used by another
// live (idle/active) pool proxy. Callers must hold pm.mu.
func (pm *PoolManager) duplicateIPLocked(p *Proxy) bool {
	if p.EgressIP == "" {
		return false
	}
	for _, pr := range pm.pool {
		if pr.ID == p.ID || pr.EgressIP == "" {
			continue
		}
		// Only healthy proxies count: cooldown/unhealthy ones are on their
		// way out and will be pruned.
		if pr.EgressIP == p.EgressIP && (pr.State == StateIdle || pr.State == StateActive) {
			return true
		}
	}
	return false
}

// rotateUntilUniqueIP inserts the proxy into the pool and, while its egress
// IP duplicates another live proxy's IP, replaces the container until it gets
// a unique IP. When ProxyIPRotateAttempts is exhausted, ONE duplicated proxy
// is kept (availability over diversity). Checking after insertion handles
// concurrent creation during Start(): goroutines see each other as they land.
func (pm *PoolManager) rotateUntilUniqueIP(ctx context.Context, proxy *Proxy) (*Proxy, error) {
	attempts := pm.cfg.ProxyIPRotateAttempts
	if attempts < 1 {
		attempts = 1
	}

	for i := 0; ; i++ {
		pm.mu.Lock()
		pm.pool[proxy.ID] = proxy
		dup := pm.duplicateIPLocked(proxy)
		if !dup {
			pm.mu.Unlock()
			return proxy, nil
		}
		// Take it back out while rotating so other creators don't see the dup.
		delete(pm.pool, proxy.ID)
		pm.mu.Unlock()

		if i >= attempts-1 {
			pm.log.Warn().
				Str("id", proxy.ID).
				Str("ip", proxy.EgressIP).
				Int("attempts", i+1).
				Msg("Duplicate egress IP persists after rotations; keeping one duplicate")
			pm.mu.Lock()
			pm.pool[proxy.ID] = proxy
			pm.mu.Unlock()
			return proxy, nil
		}

		pm.log.Warn().
			Str("id", proxy.ID).
			Str("ip", proxy.EgressIP).
			Int("attempt", i+1).
			Int("max_attempts", attempts).
			Msg("Duplicate egress IP detected, rotating container")

		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rmErr := pm.mgr.Remove(rctx, proxy.ContainerID)
		cancel()
		if rmErr != nil {
			pm.log.Error().Err(rmErr).Str("container_id", proxy.ContainerID).Msg("Failed to remove duplicate-IP container")
		}

		banned := pm.bannedRegions()
		avoid := pm.usedServersAvoid()
		np, err := pm.mgr.CreateEx(ctx, banned, avoid)
		if err != nil {
			pm.log.Error().Err(err).Msg("Proxy rotation after duplicate IP failed")
			return nil, err
		}
		proxy = np
	}
}

// createProxy creates a new proxy container and adds it to the pool, retrying
// transient failures (e.g. slow VPN first-boot) with a short backoff.
func (pm *PoolManager) createProxy(ctx context.Context) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt) * 10 * time.Second
			pm.log.Warn().Int("attempt", attempt).Dur("delay", delay).Msg("Retrying proxy creation")
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Get currently banned regions and used servers to avoid them
		banned := pm.bannedRegions()
		avoid := pm.usedServersAvoid()

		proxy, err := pm.mgr.CreateEx(ctx, banned, avoid)
		if err != nil {
			lastErr = err
			pm.log.Error().Err(err).Int("attempt", attempt).Msg("Proxy creation failed")
			continue
		}

		// Rotate while the new container shares an egress IP with an existing
		// pool proxy. rotateUntilUniqueIP inserts into the pool itself.
		final, err := pm.rotateUntilUniqueIP(ctx, proxy)
		if err != nil {
			lastErr = err
			continue
		}
		proxy = final

		if pm.mgr != nil {
			proxy.SetManager(pm.mgr)
		}

		pm.log.Info().
			Str("id", proxy.ID).
			Str("socks5", proxy.SOCKS5Addr).
			Str("region", proxy.Region).
			Str("ip", proxy.EgressIP).
			Msg("Created new proxy container")

		// Signal availability
		select {
		case pm.available <- struct{}{}:
		default:
		}

		return nil
	}

	return fmt.Errorf("proxy creation failed after %d attempts: %w", maxAttempts, lastErr)
}

// bannedRegions returns a set of regions that are currently banned.
func (pm *PoolManager) bannedRegions() map[string]bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	now := time.Now()
	banned := make(map[string]bool)
	for region, until := range pm.regionBans {
		if now.Before(until) {
			banned[region] = true
		} else {
			delete(pm.regionBans, region) // clean expired
		}
	}
	return banned
}

// usedServersAvoid returns a set of logical server names currently in use by
// pool proxies, so new containers are spread across different servers for
// diverse exit IPs.
func (pm *PoolManager) usedServersAvoid() map[string]bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	avoid := make(map[string]bool)
	for _, p := range pm.pool {
		if p.ServerName != "" {
			avoid[p.ServerName] = true
		}
	}
	return avoid
}

// ipBannedLocked reports whether the egress IP is currently banned.
// Callers must hold pm.mu (read or write).
func (pm *PoolManager) ipBannedLocked(ip string, now time.Time) bool {
	if ip == "" {
		return false
	}
	until, ok := pm.ipBans[ip]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(pm.ipBans, ip)
	return false
}

// ipBanned reports whether the egress IP is currently banned.
func (pm *PoolManager) ipBanned(ip string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.ipBannedLocked(ip, time.Now())
}

// GetProxy returns an available proxy, blocking if none available
func (pm *PoolManager) GetProxy(ctx context.Context, conversationID string) (*Proxy, error) {
	// Check for sticky session first
	if conversationID != "" && pm.cfg.StickySessionTTL > 0 {
		pm.mu.RLock()
		proxyID, ok := pm.sticky[conversationID]
		if ok {
			proxy, exists := pm.pool[proxyID]
			now := time.Now()
			if exists && proxy.IsHealthy() && !pm.ipBannedLocked(proxy.EgressIP, now) {
				pm.mu.RUnlock()
				return proxy, nil
			}
		}
		pm.mu.RUnlock()
	}

	// Wait for available proxy
	for {
		// Use write lock since we modify proxy state and counters
		pm.mu.Lock()
		now := time.Now()
		var candidates []*Proxy
		for _, proxy := range pm.pool {
			if proxy.IsAvailable() && !pm.ipBannedLocked(proxy.EgressIP, now) {
				candidates = append(candidates, proxy)
			}
		}
		if len(candidates) > 0 {
			// Round-robin over a deterministically ordered candidate list so
			// incoming requests are distributed evenly across containers
			// (map iteration order is random in Go).
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
					return candidates[i].ID < candidates[j].ID
				}
				return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
			})
			proxy := candidates[pm.rr%len(candidates)]
			pm.rr++

			proxy.State = StateActive
			proxy.LastUsed = time.Now()
			proxy.RequestsSent++
			pm.totalRequests.Add(1)

			// Set sticky session
			if conversationID != "" {
				pm.sticky[conversationID] = proxy.ID
			}

			pm.mu.Unlock()
			pm.log.Debug().Str("proxy_id", proxy.ID).Msg("Acquired proxy")
			return proxy, nil
		}
		pm.mu.Unlock()

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

// usableCountLocked counts proxies that are healthy (idle/active) and whose
// egress IP is not currently banned. Callers must hold pm.mu (read or write).
func (pm *PoolManager) usableCountLocked(now time.Time) int {
	n := 0
	for _, pr := range pm.pool {
		if pr.IsHealthy() && !pm.ipBannedLocked(pr.EgressIP, now) {
			n++
		}
	}
	return n
}

// ensurePoolCapacity keeps the pool topped up: whenever fewer than
// ProxyPoolSize proxies are usable (healthy and unbanned), it asynchronously
// spawns fresh containers for the exact deficit. Safe to call concurrently —
// a spawning counter prevents duplicate spawns. Once replacements come up,
// pruneDeadProxies removes the dead (cooldown/unhealthy) ones, so the pool
// settles at exactly ProxyPoolSize usable containers and never grows past
// ProxyPoolSize + the deficit being created.
func (pm *PoolManager) ensurePoolCapacity(ctx context.Context) {
	pm.mu.Lock()
	now := time.Now()
	usable := pm.usableCountLocked(now)
	deficit := pm.cfg.ProxyPoolSize - usable
	toSpawn := deficit - pm.spawning
	if toSpawn < 0 {
		toSpawn = 0
	}
	if toSpawn > 0 {
		pm.spawning += toSpawn
	}
	pm.mu.Unlock()

	if toSpawn <= 0 || pm.mgr == nil {
		pm.pruneDeadProxies()
		return
	}

	pm.log.Info().
		Int("usable", usable).
		Int("target", pm.cfg.ProxyPoolSize).
		Int("spawning", toSpawn).
		Msg("Pool below capacity, spawning replacement proxies")

	for i := 0; i < toSpawn; i++ {
		go func() {
			defer pm.spawnDone()
			spawnCtx, cancel := context.WithTimeout(context.Background(), pm.cfg.RateLimitFreshIPWait+time.Minute)
			defer cancel()
			if err := pm.createProxy(spawnCtx); err != nil {
				pm.log.Error().Err(err).Msg("Failed to spawn replacement proxy for pool capacity")
			}
			pm.pruneDeadProxies()
		}()
	}
}

// spawnDone decrements the in-flight spawn counter.
func (pm *PoolManager) spawnDone() {
	pm.mu.Lock()
	if pm.spawning > 0 {
		pm.spawning--
	}
	pm.mu.Unlock()
}

// pruneDeadProxies removes cooldown/unhealthy proxies (containers + pool
// entries) that are no longer needed, keeping total pool size at or below
// ProxyPoolSize. Dead proxies are always removed first so a freshly topped-up
// pool deletes its old rate-limited containers and settles at exactly the
// configured size.
func (pm *PoolManager) pruneDeadProxies() {
	pm.mu.Lock()
	var toRemove []string
	for _, pr := range pm.pool {
		if pr.State == StateCooldown || pr.State == StateUnhealthy {
			toRemove = append(toRemove, pr.ID)
		}
	}
	// Never shrink below the configured pool size.
	if excess := len(pm.pool) - pm.cfg.ProxyPoolSize; excess > 0 && len(toRemove) > excess {
		toRemove = toRemove[:excess]
	}
	pm.mu.Unlock()

	if len(toRemove) == 0 || pm.mgr == nil {
		return
	}

	pm.log.Info().Int("removed", len(toRemove)).Int("pool_total", len(pm.pool)).Msg("Pruning dead proxies")
	for _, id := range toRemove {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := pm.RemoveProxy(ctx, id); err != nil {
			pm.log.Error().Err(err).Str("proxy_id", id).Msg("Failed to prune dead proxy")
		}
		cancel()
	}
}

// MarkRateLimited marks a proxy as rate limited: its egress IP is banned for
// max(IPBanDuration, RetryAfter) so no request is routed through it, the proxy
// itself is moved to cooldown, and the pool is topped up with a fresh
// replacement container from a different region.
//
// publicTier indicates the upstream throttle is tied to account identity
// rather than egress IP; it is informational (the caller already surfaces
// 429+Retry-After immediately for public tier; self-healing still runs).
func (pm *PoolManager) MarkRateLimited(proxy *Proxy, retryAfter string, publicTier bool) {
	banFor := pm.cfg.IPBanDuration
	if retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 {
			if d := time.Duration(secs) * time.Second; d > banFor {
				banFor = d
			}
		}
	}

	pm.mu.Lock()
	pm.banIPLocked(proxy.EgressIP, time.Now().Add(banFor))

	// Also ban the region so replacement containers use a different region
	if proxy.Region != "" {
		pm.regionBans[proxy.Region] = time.Now().Add(banFor)
	}

	if pr, exists := pm.pool[proxy.ID]; exists {
		pr.State = StateCooldown
		pr.LastUsed = time.Now()
		pr.ErrorCount++
		pm.totalErrors.Add(1)

		pm.log.Warn().
			Str("proxy_id", pr.ID).
			Str("ip", pr.EgressIP).
			Str("region", pr.Region).
			Dur("cooldown", pm.cfg.CooldownDuration).
			Dur("ip_ban", banFor).
			Bool("public_tier", publicTier).
			Msg("Proxy rate limited, moving to cooldown and banning region")
	}

	// Signals
	select {
	case pm.available <- struct{}{}:
	default:
	}
	pm.mu.Unlock()

	// Top the pool back up to full capacity: spawn fresh containers for the
	// deficit and prune the dead ones once replacements are ready.
	pm.ensurePoolCapacity(context.Background())
}

// banIPLocked records that an egress IP is rate limited until the given time.
// Callers must hold pm.mu.
func (pm *PoolManager) banIPLocked(ip string, until time.Time) {
	if ip == "" {
		return
	}
	if until.After(pm.ipBans[ip]) {
		pm.ipBans[ip] = until
	}
}

// MarkUnhealthy marks a proxy as unhealthy (failed health check or a
// transport failure mid-request — usually a dead VPN tunnel). Unhealthy
// proxies are skipped by GetProxy immediately, and the pool is topped back
// up with a replacement.
func (pm *PoolManager) MarkUnhealthy(proxy *Proxy) {
	if proxy == nil {
		return
	}

	topUp := false
	pm.mu.Lock()
	if pr, exists := pm.pool[proxy.ID]; exists {
		pr.State = StateUnhealthy
		pr.ErrorCount++
		pm.totalErrors.Add(1)
		topUp = true

		pm.log.Warn().
			Str("proxy_id", pr.ID).
			Int("error_count", pr.ErrorCount).
			Msg("Proxy marked unhealthy")
	}
	pm.mu.Unlock()

	if topUp {
		pm.ensurePoolCapacity(context.Background())
	}
}

// RemoveProxy removes a proxy from the pool
func (pm *PoolManager) RemoveProxy(ctx context.Context, proxyID string) error {
	pm.mu.Lock()
	proxy, exists := pm.pool[proxyID]
	if !exists {
		pm.mu.Unlock()
		return nil
	}
	delete(pm.pool, proxyID)
	pm.mu.Unlock()

	// Remove container
	if err := pm.mgr.Remove(ctx, proxy.ContainerID); err != nil {
		pm.log.Error().Err(err).Str("container_id", proxy.ContainerID).Msg("Failed to remove container")
		return err
	}

	pm.log.Info().Str("proxy_id", proxyID).Msg("Removed proxy")
	return nil
}

// AddExternalProxy adds an external SOCKS5 proxy to the live pool.
func (pm *PoolManager) AddExternalProxy(ctx context.Context, addr string) (*Proxy, error) {
	extMgr := NewExternalProxyManager([]string{addr}, pm.cfg.ProtonVPNIPCheckURL, pm.log)
	p, err := extMgr.CreateEx(ctx, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create external proxy: %w", err)
	}

	// Health check must pass before adding to pool
	ok, err := extMgr.HealthCheck(ctx, p)
	if err != nil || !ok {
		extMgr.Close()
		if err != nil {
			return nil, fmt.Errorf("health check failed for %s: %w", addr, err)
		}
		return nil, fmt.Errorf("health check failed for %s: proxy is unreachable", addr)
	}

	p.SetManager(extMgr)

	pm.mu.Lock()
	pm.pool[p.ID] = p
	pm.mu.Unlock()

	// Signal availability
	select {
	case pm.available <- struct{}{}:
	default:
	}

	pm.log.Info().Str("id", p.ID).Str("addr", addr).Str("ip", p.EgressIP).Msg("Added external proxy (healthy)")
	return p, nil
}

// RemoveExternalProxy removes an external proxy from the pool by ID.
func (pm *PoolManager) RemoveExternalProxy(ctx context.Context, proxyID string) error {
	return pm.RemoveProxy(ctx, proxyID)
}

// SetPoolSize updates the target pool size at runtime.
func (pm *PoolManager) SetPoolSize(newSize int) {
	pm.mu.Lock()
	pm.cfg.ProxyPoolSize = newSize
	pm.mu.Unlock()

	pm.log.Info().Int("new_size", newSize).Msg("Pool size updated")
	pm.ensurePoolCapacity(context.Background())
}

// AddVPNContainer spawns a new VPN container and adds it to the pool.
func (pm *PoolManager) AddVPNContainer(ctx context.Context) error {
	return pm.createProxy(ctx)
}

// RefreshPool reloads external proxies from the ConfigStore and ensures
// the VPN pool is at capacity. Call this after adding accounts or proxies
// via the management UI to pick them up without a restart.
// Returns a list of addresses that failed health checks.
func (pm *PoolManager) RefreshPool(ctx context.Context, proxies []string) []string {
	var failed []string

	for _, addr := range proxies {
		if addr == "" {
			continue
		}
		if _, err := pm.AddExternalProxy(ctx, addr); err != nil {
			pm.log.Warn().Err(err).Str("addr", addr).Msg("External proxy failed health check during refresh")
			failed = append(failed, addr+": "+err.Error())
		}
	}

	// Ensure VPN pool is at capacity
	pm.ensurePoolCapacity(ctx)

	pm.log.Info().Int("external_addrs", len(proxies)).Int("failed", len(failed)).Msg("Pool refresh triggered")
	return failed
}

// ForceRotate creates a new VPN container immediately, rotating one existing
// proxy if needed to make room. Returns the new proxy or an error.
func (pm *PoolManager) ForceRotate(ctx context.Context) (*Proxy, error) {
	if pm.mgr == nil {
		return nil, fmt.Errorf("no VPN manager available")
	}

	// If at capacity, remove the oldest idle proxy to make room
	pm.mu.Lock()
	if len(pm.pool) >= pm.cfg.ProxyPoolSize {
		// Find an idle proxy to remove
		for id, pr := range pm.pool {
			if pr.State == StateIdle {
				delete(pm.pool, id)
				if pr.ContainerID != "" {
					go func() {
						rCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						if err := pm.mgr.Remove(rCtx, pr.ContainerID); err != nil {
							pm.log.Warn().Err(err).Str("id", pr.ID).Msg("Failed to remove old proxy during rotation")
						}
					}()
				}
				break
			}
		}
	}
	pm.mu.Unlock()

	// Create new proxy
	if err := pm.createProxy(ctx); err != nil {
		return nil, fmt.Errorf("failed to create new proxy: %w", err)
	}

	// Return the most recently added proxy
	pm.mu.RLock()
	var newest *Proxy
	for _, pr := range pm.pool {
		if newest == nil || pr.CreatedAt.After(newest.CreatedAt) {
			newest = pr
		}
	}
	pm.mu.RUnlock()

	if newest == nil {
		return nil, fmt.Errorf("proxy created but not found in pool")
	}

	pm.log.Info().
		Str("id", newest.ID).
		Str("socks5", newest.SOCKS5Addr).
		Str("region", newest.Region).
		Msg("Force rotated VPN container")

	return newest, nil
}

// Stats returns current pool statistics
func (pm *PoolManager) Stats() PoolStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	stats := PoolStats{
		Total:         len(pm.pool),
		TotalRequests: int(pm.totalRequests.Load()),
		TotalErrors:   int(pm.totalErrors.Load()),
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

// List returns a snapshot of all proxies in the pool
func (pm *PoolManager) List() []*Proxy {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	proxies := make([]*Proxy, 0, len(pm.pool))
	for _, proxy := range pm.pool {
		proxies = append(proxies, proxy)
	}
	return proxies
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

	driftDetected := false
	for _, proxy := range proxies {
		healthy, err := pm.mgr.HealthCheck(ctx, proxy)
		if err != nil {
			pm.log.Warn().Err(err).Str("proxy_id", proxy.ID).Msg("Health check failed")
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
			// Drift detection: a VPN reconnect can silently change a proxy's
			// egress IP into a duplicate of another live proxy. Mark it
			// unhealthy so the normal self-healing machinery rotates it.
			if healthy && pr.State == StateIdle && pm.duplicateIPLocked(pr) {
				pr.State = StateUnhealthy
				driftDetected = true
				pm.log.Warn().
					Str("proxy_id", pr.ID).
					Str("ip", pr.EgressIP).
					Msg("Egress IP drifted into a duplicate of another proxy; rotating")
			}
		}
		pm.mu.Unlock()
	}

	// Top the pool back up for any drift-rotated proxies.
	if driftDetected {
		pm.ensurePoolCapacity(ctx)
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
			pm.ensurePoolCapacity(ctx)
		case <-ctx.Done():
			return
		case <-pm.done:
			return
		}
	}
}

// restoreCooldownProxies moves expired cooldown proxies back to idle, prunes
// excess containers that were spawned during rate-limit rotation, and clears
// stale IP and region bans.
func (pm *PoolManager) restoreCooldownProxies() {
	pm.mu.Lock()
	now := time.Now()
	var toRemove []string

	// Clear expired IP bans
	for ip, until := range pm.ipBans {
		if !now.Before(until) {
			delete(pm.ipBans, ip)
		}
	}

	// Clear expired region bans
	for region, until := range pm.regionBans {
		if !now.Before(until) {
			delete(pm.regionBans, region)
		}
	}

	for _, proxy := range pm.pool {
		if proxy.State != StateCooldown {
			continue
		}
		if now.Sub(proxy.LastUsed) < pm.cfg.CooldownDuration {
			continue
		}

		// Keep the pool at its configured size: if we spawned a replacement
		// during rate-limit rotation and no longer need this container, drop
		// it outright instead of returning it to idle.
		if len(pm.pool) > pm.cfg.ProxyPoolSize {
			toRemove = append(toRemove, proxy.ID)
			continue
		}

		proxy.State = StateIdle
		proxy.ErrorCount = 0
		pm.log.Info().Str("proxy_id", proxy.ID).Msg("Proxy restored from cooldown")

		// Only signal availability if the proxy is actually usable now; a
		// banned IP must not wake waiters who'd just re-block on it.
		if !pm.ipBannedLocked(proxy.EgressIP, now) {
			select {
			case pm.available <- struct{}{}:
			default:
			}
		}
	}
	pm.mu.Unlock()

	for _, id := range toRemove {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := pm.RemoveProxy(ctx, id); err != nil {
			pm.log.Error().Err(err).Str("proxy_id", id).Msg("Failed to prune excess proxy")
		}
		cancel()
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
