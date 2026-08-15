package proxy

import (
	"testing"
	"time"
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
