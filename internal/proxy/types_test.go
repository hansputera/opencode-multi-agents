package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/rs/zerolog"
)

func TestProxyState(t *testing.T) {
	tests := []struct {
		name      string
		proxy     *Proxy
		isHealthy bool
		isAvail   bool
	}{
		{
			name:      "Idle proxy",
			proxy:     &Proxy{State: StateIdle},
			isHealthy: true,
			isAvail:   true,
		},
		{
			name:      "Active proxy",
			proxy:     &Proxy{State: StateActive},
			isHealthy: true,
			isAvail:   false,
		},
		{
			name:      "Cooldown proxy",
			proxy:     &Proxy{State: StateCooldown},
			isHealthy: false,
			isAvail:   false,
		},
		{
			name:      "Unhealthy proxy",
			proxy:     &Proxy{State: StateUnhealthy},
			isHealthy: false,
			isAvail:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.proxy.IsHealthy() != tt.isHealthy {
				t.Errorf("IsHealthy() = %v, want %v", tt.proxy.IsHealthy(), tt.isHealthy)
			}
			if tt.proxy.IsAvailable() != tt.isAvail {
				t.Errorf("IsAvailable() = %v, want %v", tt.proxy.IsAvailable(), tt.isAvail)
			}
		})
	}
}

func TestPoolStats(t *testing.T) {
	stats := PoolStats{
		Total:         5,
		Idle:          2,
		Active:        1,
		Cooldown:      1,
		Unhealthy:     1,
		TotalRequests: 100,
		TotalErrors:   5,
	}

	if stats.Total != 5 {
		t.Errorf("Total = %d, want 5", stats.Total)
	}

	if stats.Idle != 2 {
		t.Errorf("Idle = %d, want 2", stats.Idle)
	}
}

func TestProxyFields(t *testing.T) {
	now := time.Now()
	proxy := &Proxy{
		ID:           "test-123",
		ContainerID:  "container-456",
		SOCKS5Addr:   "socks5://127.0.0.1:10801",
		Port:         10801,
		State:        StateIdle,
		LastUsed:     now,
		LastCheck:    now,
		RequestsSent: 10,
		ErrorCount:   2,
		CreatedAt:    now,
	}

	if proxy.ID != "test-123" {
		t.Errorf("ID = %s, want test-123", proxy.ID)
	}

	if proxy.Port != 10801 {
		t.Errorf("Port = %d, want 10801", proxy.Port)
	}

	if proxy.RequestsSent != 10 {
		t.Errorf("RequestsSent = %d, want 10", proxy.RequestsSent)
	}
}

func TestEgressIPFromTrace(t *testing.T) {
	tests := []struct {
		name  string
		trace string
		want  string
	}{
		{
			name:  "warp trace with ip",
			trace: "ip=2a09:bac5::123\nwarp=on\nloc=US\n",
			want:  "2a09:bac5::123",
		},
		{
			name:  "no ip line",
			trace: "warp=on\nloc=US\n",
			want:  "",
		},
		{
			name:  "empty trace",
			trace: "",
			want:  "",
		},
		{
			name:  "ip not at line start",
			trace: "x-ip=9.9.9.9\nwarp=on\n",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := egressIPFromTrace(tt.trace); got != tt.want {
				t.Errorf("egressIPFromTrace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPoolHasIP(t *testing.T) {
	pm := &PoolManager{
		pool: map[string]*Proxy{
			"a": {EgressIP: "1.1.1.1", State: StateIdle},
			"b": {EgressIP: "2.2.2.2", State: StateIdle},
			"c": {EgressIP: "3.3.3.3", State: StateUnhealthy},
			"d": {EgressIP: "4.4.4.4", State: StateCooldown},
		},
	}

	if !pm.poolHasIP("x", "1.1.1.1") {
		t.Error("poolHasIP() = false for IP used by healthy proxy a, want true")
	}
	if !pm.poolHasIP("x", "2.2.2.2") {
		t.Error("poolHasIP() = false for IP used by healthy proxy b, want true")
	}
	// All states (including unhealthy, which may recover) count so that a
	// replacement container is never assigned an IP already in use by any
	// member of the pool.
	if !pm.poolHasIP("x", "3.3.3.3") {
		t.Error("poolHasIP() = false for IP used by unhealthy proxy c, want true")
	}
	if !pm.poolHasIP("x", "4.4.4.4") {
		t.Error("poolHasIP() = false for IP used by cooldown proxy d, want true")
	}
	if pm.poolHasIP("x", "9.9.9.9") {
		t.Error("poolHasIP() = true for unused IP, want false")
	}
	if pm.poolHasIP("a", "1.1.1.1") {
		t.Error("poolHasIP() = true for own IP only (same proxy id), want false")
	}
}

// newTestPool builds a PoolManager whose manager is stubbed out, for unit
// testing ban/selection logic without Docker.
func newTestPool() *PoolManager {
	log := zerolog.Nop()
	return &PoolManager{
		cfg:    config.DefaultConfig(),
		log:    &log,
		pool:   make(map[string]*Proxy),
		ipBans: make(map[string]time.Time),
	}
}

func TestIPBanSkipsBannedProxy(t *testing.T) {
	pm := newTestPool()
	pm.pool["healthy"] = &Proxy{ID: "healthy", EgressIP: "10.0.0.1", State: StateIdle}
	pm.pool["banned"] = &Proxy{ID: "banned", EgressIP: "10.0.0.2", State: StateIdle}

	// Ban the egress IP of "banned".
	pm.mu.Lock()
	pm.ipBans["10.0.0.2"] = time.Now().Add(5 * time.Minute)
	pm.mu.Unlock()

	p, err := pm.GetProxy(context.Background(), "")
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if p.ID != "healthy" {
		t.Errorf("got proxy %q, want %q (banned IP must be skipped)", p.ID, "healthy")
	}
}

func TestMarkRateLimitedBansForRetryAfter(t *testing.T) {
	pm := newTestPool()
	// Two proxies; pool size 1 so the replacement goroutine is skipped and
	// we can test the ban/cooldown deterministically without a real manager.
	pm.cfg.ProxyPoolSize = 1
	pm.pool["p1"] = &Proxy{ID: "p1", EgressIP: "10.0.0.3", State: StateIdle}
	pm.pool["p2"] = &Proxy{ID: "p2", EgressIP: "10.0.0.9", State: StateIdle}

	// Pre-ban briefly; MarkRateLimited must extend to max(config, RetryAfter).
	pm.ipBans["10.0.0.3"] = time.Now().Add(5 * time.Minute)
	pm.MarkRateLimited(pm.pool["p1"], "900", false)

	pm.mu.RLock()
	until := pm.ipBans["10.0.0.3"]
	st := pm.pool["p1"].State
	pm.mu.RUnlock()

	if time.Until(until) < 8*time.Minute {
		t.Errorf("ban duration shorter than Retry-After 900s: %v left", time.Until(until))
	}
	if st != StateCooldown {
		t.Errorf("proxy state = %q, want cooldown", st)
	}
}

func TestBannedExpiryUnblocks(t *testing.T) {
	pm := newTestPool()
	pm.pool["p"] = &Proxy{ID: "p", EgressIP: "10.0.0.4", State: StateIdle}

	pm.mu.Lock()
	pm.ipBans["10.0.0.4"] = time.Now().Add(10 * time.Millisecond)
	pm.mu.Unlock()

	time.Sleep(20 * time.Millisecond)

	pm.mu.Lock()
	banned := pm.ipBannedLocked("10.0.0.4", time.Now())
	pm.mu.Unlock()
	if banned {
		t.Error("expired ban still active")
	}
}

// stubManager fakes container management for pool scaling tests.
type stubManager struct {
	createCount int32
	removeCount int32
}

func (s *stubManager) Create(ctx context.Context) (*Proxy, error) {
	n := atomic.AddInt32(&s.createCount, 1)
	return &Proxy{
		ID:         fmt.Sprintf("stub-%d", n),
		SOCKS5Addr: "socks5://127.0.0.1:9999",
		State:      StateIdle,
		EgressIP:   fmt.Sprintf("10.0.0.%d", n+100),
	}, nil
}

func (s *stubManager) Remove(ctx context.Context, id string) error {
	atomic.AddInt32(&s.removeCount, 1)
	return nil
}
func (s *stubManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) { return true, nil }
func (s *stubManager) Close() error                                            { return nil }

// TestEnsurePoolCapacityScalesUpAndPrunes verifies that when every proxy in
// the pool is dead (cooldown + banned IP), the pool spawns a fresh replacement
// and prunes the dead container, settling at exactly ProxyPoolSize usable
// proxies.
func TestEnsurePoolCapacityScalesUpAndPrunes(t *testing.T) {
	mgr := &stubManager{}
	pm := newTestPool()
	pm.mgr = mgr
	pm.cfg.ProxyPoolSize = 1

	pm.pool["dead"] = &Proxy{ID: "dead", EgressIP: "10.0.0.5", State: StateCooldown}
	pm.ipBans["10.0.0.5"] = time.Now().Add(10 * time.Minute)

	pm.ensurePoolCapacity(context.Background())

	// Give the async spawn goroutine time to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pm.mu.RLock()
		total := len(pm.pool)
		spawning := pm.spawning
		pm.mu.RUnlock()
		if total >= 1 && spawning == 0 && atomic.LoadInt32(&mgr.createCount) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pm.mu.RLock()
	total := len(pm.pool)
	usable := pm.usableCountLocked(time.Now())
	pm.mu.RUnlock()

	if mgr.removeCount == 0 {
		t.Error("expected dead proxy to be pruned (Remove called)")
	}
	if total != 1 {
		t.Errorf("pool total = %d, want 1 (exactly ProxyPoolSize)", total)
	}
	if usable != 1 {
		t.Errorf("usable = %d, want 1 (fresh replacement should be usable)", usable)
	}
}

// TestEnsurePoolCapacityNoDoubleSpawn verifies that concurrent triggers don't
// spawn more than the deficit.
func TestEnsurePoolCapacityNoDoubleSpawn(t *testing.T) {
	mgr := &stubManager{}
	pm := newTestPool()
	pm.mgr = mgr
	pm.cfg.ProxyPoolSize = 2
	pm.pool["dead1"] = &Proxy{ID: "dead1", EgressIP: "10.0.0.11", State: StateCooldown}
	pm.pool["dead2"] = &Proxy{ID: "dead2", EgressIP: "10.0.0.12", State: StateCooldown}
	pm.ipBans["10.0.0.11"] = time.Now().Add(10 * time.Minute)
	pm.ipBans["10.0.0.12"] = time.Now().Add(10 * time.Minute)

	// Fire several concurrent triggers.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pm.ensurePoolCapacity(context.Background())
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pm.mu.RLock()
		spawning := pm.spawning
		pm.mu.RUnlock()
		if spawning == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&mgr.createCount); got != 2 {
		t.Errorf("creates = %d, want 2 (exactly the deficit, no double spawn)", got)
	}
}
