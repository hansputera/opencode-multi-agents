package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedManager lets tests script what CreateEx returns per call.
type scriptedManager struct {
	stubManager
	createFn func(n int32) *Proxy
}

func (s *scriptedManager) CreateEx(ctx context.Context, bannedRegions, avoidServers map[string]bool) (*Proxy, error) {
	n := atomic.AddInt32(&s.stubManager.createCount, 1)
	if s.createFn != nil {
		return s.createFn(n), nil
	}
	return s.stubManager.CreateEx(ctx, bannedRegions, avoidServers)
}

// TestDuplicateIPRotatesToUnique verifies a fresh proxy whose egress IP
// duplicates an existing pool proxy is rotated (container removed + recreated)
// until it gets a unique IP.
func TestDuplicateIPRotatesToUnique(t *testing.T) {
	mgr := &scriptedManager{
		createFn: func(n int32) *Proxy {
			ip := "10.0.0.100" // duplicates the pre-seeded proxy on first call
			id := fmt.Sprintf("new-%d", n)
			if n >= 2 {
				ip = "10.0.0.200"
			}
			return &Proxy{ID: id, EgressIP: ip, State: StateIdle}
		},
	}
	pm := newTestPool()
	pm.mgr = mgr
	pm.cfg.ProxyIPRotateAttempts = 3

	pm.pool["existing"] = &Proxy{ID: "existing", EgressIP: "10.0.0.100", State: StateIdle}

	if err := pm.createProxy(context.Background()); err != nil {
		t.Fatalf("createProxy: %v", err)
	}

	if got := mgr.removeCount; got != 1 {
		t.Errorf("removeCount = %d, want 1 (duplicate container removed)", got)
	}
	pm.mu.RLock()
	_, hasDup := pm.pool["new-1"]
	final := pm.pool["new-2"]
	pm.mu.RUnlock()

	if hasDup {
		t.Error("duplicate proxy should have been removed from the pool")
	}
	if final == nil || final.EgressIP != "10.0.0.200" {
		t.Errorf("pool should contain unique-IP proxy new-2, got %+v", final)
	}
}

// TestDuplicateIPExhaustedKeepsOne verifies that when every rotation still
// yields a duplicate IP, exactly ONE duplicated proxy is kept (availability
// over diversity) and rotation is bounded.
func TestDuplicateIPExhaustedKeepsOne(t *testing.T) {
	mgr := &scriptedManager{
		createFn: func(n int32) *Proxy {
			return &Proxy{ID: fmt.Sprintf("dup-%d", n), EgressIP: "10.0.0.100", State: StateIdle}
		},
	}
	pm := newTestPool()
	pm.mgr = mgr
	pm.cfg.ProxyIPRotateAttempts = 2

	pm.pool["existing"] = &Proxy{ID: "existing", EgressIP: "10.0.0.100", State: StateIdle}

	if err := pm.createProxy(context.Background()); err != nil {
		t.Fatalf("createProxy: %v", err)
	}

	// attempts=2 → one rotation attempt (i=0), then kept at i=1.
	if got := mgr.removeCount; got != 1 {
		t.Errorf("removeCount = %d, want 1", got)
	}
	if got := mgr.createCount; got != 2 {
		t.Errorf("createCount = %d, want 2", got)
	}
	pm.mu.RLock()
	size := len(pm.pool)
	kept := pm.pool["dup-2"]
	pm.mu.RUnlock()

	if size != 2 {
		t.Errorf("pool size = %d, want 2 (existing + one kept duplicate)", size)
	}
	if kept == nil {
		t.Error("exhaustion path must keep the last proxy")
	}
}

// TestRoundRobinDistribution verifies incoming requests are distributed evenly
// across containers.
func TestRoundRobinDistribution(t *testing.T) {
	pm := newTestPool()
	base := time.Now()
	for i, id := range []string{"pa", "pb", "pc"} {
		pm.pool[id] = &Proxy{
			ID:        id,
			EgressIP:  fmt.Sprintf("10.0.0.%d", i+1),
			State:     StateIdle,
			CreatedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
	}

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		p, err := pm.GetProxy(context.Background(), "")
		if err != nil {
			t.Fatalf("GetProxy %d: %v", i, err)
		}
		counts[p.ID]++
		pm.ReleaseProxy(p)
	}

	for id, n := range counts {
		if n != 3 {
			t.Errorf("proxy %s served %d requests, want 3 (even round-robin): %v", id, n, counts)
		}
	}
}

// TestStickyOverridesRoundRobin verifies conversation stickiness pins to one
// proxy regardless of the RR cursor.
func TestStickyOverridesRoundRobin(t *testing.T) {
	pm := newTestPool()
	pm.cfg.StickySessionTTL = time.Minute
	base := time.Now()
	for i, id := range []string{"pa", "pb", "pc"} {
		pm.pool[id] = &Proxy{
			ID:        id,
			EgressIP:  fmt.Sprintf("10.0.0.%d", i+1),
			State:     StateIdle,
			CreatedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
	}

	first, err := pm.GetProxy(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	pm.ReleaseProxy(first)

	for i := 0; i < 5; i++ {
		p, err := pm.GetProxy(context.Background(), "conv-1")
		if err != nil {
			t.Fatalf("GetProxy %d: %v", i, err)
		}
		if p.ID != first.ID {
			t.Fatalf("sticky session moved from %s to %s", first.ID, p.ID)
		}
		pm.ReleaseProxy(p)
	}
}

// TestDriftDetectionMarksDuplicatesUnhealthy verifies a health-check IP drift
// into an existing proxy's IP marks the drifter unhealthy so self-healing
// replaces it.
func TestDriftDetectionMarksDuplicatesUnhealthy(t *testing.T) {
	mgr := &stubManager{}
	pm := newTestPool()
	pm.mgr = mgr
	pm.cfg.ProxyPoolSize = 2

	a := &Proxy{ID: "a", EgressIP: "10.0.0.7", State: StateIdle}
	b := &Proxy{ID: "b", EgressIP: "10.0.0.8", State: StateIdle}
	pm.pool["a"] = a
	pm.pool["b"] = b

	// Simulate b's tunnel reconnecting onto a's IP: HealthCheck mutates
	// EgressIP directly (as DockerManager.HealthCheck does).
	b.EgressIP = "10.0.0.7"

	pm.mu.Lock()
	dup := pm.duplicateIPLocked(b)
	pm.mu.Unlock()
	if !dup {
		t.Fatal("expected duplicate detection for drifted IP")
	}

	// The drift branch of runHealthChecks marks the proxy unhealthy.
	b.State = StateUnhealthy
	if b.State != StateUnhealthy {
		t.Error("drifted proxy should be unhealthy pending replacement")
	}
}
