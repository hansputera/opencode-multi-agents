package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// CompositeManager wraps multiple Manager implementations (external SOCKS5 +
// one or more ProtonVPN DockerManagers) and round-robins across them when
// creating proxies. Proxy ownership is tracked so Remove/HealthCheck/Exec
// route to the correct sub-manager.
type CompositeManager struct {
	ext       *ExternalProxyManager // nil when no external proxies
	vpnMgrs   []Manager             // one per ProtonVPN account
	vpnIdx    atomic.Int64          // round-robin counter for VPN accounts
	log       *zerolog.Logger

	mu          sync.RWMutex
	proxyOwner  map[string]Manager // proxy.ID -> owning sub-manager
}

// NewCompositeManager creates a composite manager.
// ext may be nil if no external SOCKS5 proxies are configured.
// vpnMgrs must have at least one entry.
func NewCompositeManager(ext *ExternalProxyManager, vpnMgrs []Manager, log *zerolog.Logger) *CompositeManager {
	return &CompositeManager{
		ext:        ext,
		vpnMgrs:    vpnMgrs,
		log:        log,
		proxyOwner: make(map[string]Manager),
	}
}

// Create picks the next proxy source: external proxies first (round-robin),
// then VPN accounts (round-robin).
func (cm *CompositeManager) Create(ctx context.Context) (*Proxy, error) {
	return cm.CreateEx(ctx, nil, nil)
}

// CreateEx creates a new proxy. External proxies are tried first (if
// available), then VPN accounts round-robin. bannedRegions/avoidServers are
// only passed to VPN managers.
func (cm *CompositeManager) CreateEx(ctx context.Context, bannedRegions, avoidServers map[string]bool) (*Proxy, error) {
	// Try external proxies first
	if cm.ext != nil {
		p, err := cm.ext.CreateEx(ctx, bannedRegions, avoidServers)
		if err == nil {
			cm.trackOwner(p.ID, cm.ext)
			return p, nil
		}
		cm.log.Debug().Err(err).Msg("External proxy creation failed, falling back to VPN")
	}

	// Round-robin VPN accounts
	if len(cm.vpnMgrs) == 0 {
		return nil, fmt.Errorf("no proxy managers available")
	}

	idx := cm.vpnIdx.Add(1) - 1
	mgr := cm.vpnMgrs[idx%int64(len(cm.vpnMgrs))]

	p, err := mgr.CreateEx(ctx, bannedRegions, avoidServers)
	if err != nil {
		return nil, fmt.Errorf("VPN proxy creation failed: %w", err)
	}

	cm.trackOwner(p.ID, mgr)
	return p, nil
}

// Remove routes to the correct sub-manager based on proxy ID prefix.
func (cm *CompositeManager) Remove(ctx context.Context, id string) error {
	mgr := cm.getOwner(id)
	if mgr == nil {
		return fmt.Errorf("no manager found for proxy %s", id)
	}
	if err := mgr.Remove(ctx, id); err != nil {
		return err
	}
	cm.clearOwner(id)
	return nil
}

// HealthCheck routes to the correct sub-manager based on proxy ownership.
func (cm *CompositeManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) {
	mgr := cm.getOwner(p.ID)
	if mgr == nil {
		// Fallback: try all managers
		if cm.ext != nil {
			if ok, err := cm.ext.HealthCheck(ctx, p); err == nil {
				return ok, nil
			}
		}
		for _, m := range cm.vpnMgrs {
			if ok, err := m.HealthCheck(ctx, p); err == nil {
				return ok, nil
			}
		}
		return false, fmt.Errorf("no manager found for proxy %s", p.ID)
	}
	return mgr.HealthCheck(ctx, p)
}

// Close shuts down all sub-managers.
func (cm *CompositeManager) Close() error {
	var errs []string
	if cm.ext != nil {
		if err := cm.ext.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, m := range cm.vpnMgrs {
		if err := m.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors closing composite manager: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Exec routes to the correct sub-manager. External proxies return an error.
func (cm *CompositeManager) Exec(ctx context.Context, containerID string, env, args []string) ([]byte, error) {
	mgr := cm.getOwner(containerID)
	if mgr == nil {
		return nil, fmt.Errorf("no manager found for container %s", containerID)
	}
	return mgr.Exec(ctx, containerID, env, args)
}

// --- ownership tracking ---

func (cm *CompositeManager) trackOwner(id string, mgr Manager) {
	cm.mu.Lock()
	cm.proxyOwner[id] = mgr
	cm.mu.Unlock()
}

func (cm *CompositeManager) getOwner(id string) Manager {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.proxyOwner[id]
}

func (cm *CompositeManager) clearOwner(id string) {
	cm.mu.Lock()
	delete(cm.proxyOwner, id)
	cm.mu.Unlock()
}
